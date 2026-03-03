package slack

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/memohai/memoh/internal/channel"
	"github.com/memohai/memoh/internal/channel/adapters/common"
	"github.com/memohai/memoh/internal/media"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const inboundDedupTTL = time.Minute

type assetOpener interface {
	Open(ctx context.Context, botID, contentHash string) (io.ReadCloser, media.Asset, error)
}

type SlackAdapter struct {
	logger       *slog.Logger
	mu           sync.RWMutex
	clients      map[string]*slackapi.Client    // keyed by bot token
	smClients    map[string]*socketmode.Client   // keyed by bot token
	seenMessages map[string]time.Time            // keyed by token:ts
	assets       assetOpener
}

func NewSlackAdapter(log *slog.Logger) *SlackAdapter {
	if log == nil {
		log = slog.Default()
	}
	return &SlackAdapter{
		logger:       log.With(slog.String("adapter", "slack")),
		clients:      make(map[string]*slackapi.Client),
		smClients:    make(map[string]*socketmode.Client),
		seenMessages: make(map[string]time.Time),
	}
}

func (a *SlackAdapter) SetAssetOpener(opener assetOpener) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.assets = opener
}

func (a *SlackAdapter) Type() channel.ChannelType {
	return Type
}

func (a *SlackAdapter) Descriptor() channel.Descriptor {
	return channel.Descriptor{
		Type:        Type,
		DisplayName: "Slack",
		Capabilities: channel.ChannelCapabilities{
			Text:          true,
			Markdown:      true,
			Reply:         true,
			Threads:       true,
			Attachments:   true,
			Media:         true,
			Streaming:     true,
			BlockStreaming: true,
			Reactions:     true,
			Edit:          true,
		},
		ConfigSchema: channel.ConfigSchema{
			Version: 1,
			Fields: map[string]channel.FieldSchema{
				"botToken": {
					Type:        channel.FieldSecret,
					Required:    true,
					Title:       "Bot Token",
					Description: "Bot User OAuth Token (xoxb-...)",
				},
				"appToken": {
					Type:        channel.FieldSecret,
					Required:    true,
					Title:       "App-Level Token",
					Description: "App-Level Token for Socket Mode (xapp-...)",
				},
			},
		},
		UserConfigSchema: channel.ConfigSchema{
			Version: 1,
			Fields: map[string]channel.FieldSchema{
				"user_id":    {Type: channel.FieldString},
				"channel_id": {Type: channel.FieldString},
				"team_id":    {Type: channel.FieldString},
				"username":   {Type: channel.FieldString},
			},
		},
		TargetSpec: channel.TargetSpec{
			Format: "channel_id | user_id",
			Hints: []channel.TargetHint{
				{Label: "Channel ID", Example: "C01ABCDEF23"},
				{Label: "User ID", Example: "U01ABCDEF23"},
			},
		},
	}
}

func (a *SlackAdapter) getOrCreateClient(cfg Config, configID string) (*slackapi.Client, *socketmode.Client, error) {
	a.mu.RLock()
	api, apiOk := a.clients[cfg.BotToken]
	sm, smOk := a.smClients[cfg.BotToken]
	a.mu.RUnlock()
	if apiOk && smOk {
		return api, sm, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if api, ok := a.clients[cfg.BotToken]; ok {
		if sm, ok := a.smClients[cfg.BotToken]; ok {
			return api, sm, nil
		}
	}

	api = slackapi.New(
		cfg.BotToken,
		slackapi.OptionAppLevelToken(cfg.AppToken),
	)

	sm = socketmode.New(api)

	a.clients[cfg.BotToken] = api
	a.smClients[cfg.BotToken] = sm
	return api, sm, nil
}

func (a *SlackAdapter) Connect(ctx context.Context, cfg channel.ChannelConfig, handler channel.InboundHandler) (channel.Connection, error) {
	if a.logger != nil {
		a.logger.Info("start", slog.String("config_id", cfg.ID))
	}

	slackCfg, err := parseConfig(cfg.Credentials)
	if err != nil {
		return nil, err
	}

	api, sm, err := a.getOrCreateClient(slackCfg, cfg.ID)
	if err != nil {
		return nil, err
	}

	// Determine the bot's own user ID for mention/reply detection.
	authResp, err := api.AuthTest()
	if err != nil {
		return nil, fmt.Errorf("slack auth test: %w", err)
	}
	botUserID := authResp.UserID

	cancelCtx, cancel := context.WithCancel(ctx)

	go a.runSocketMode(cancelCtx, sm, api, slackCfg, cfg, handler, botUserID)

	stop := func(_ context.Context) error {
		if a.logger != nil {
			a.logger.Info("stop", slog.String("config_id", cfg.ID))
		}
		cancel()
		a.clearClientState(slackCfg.BotToken)
		return nil
	}

	return channel.NewConnection(cfg, stop), nil
}

func (a *SlackAdapter) runSocketMode(
	ctx context.Context,
	sm *socketmode.Client,
	api *slackapi.Client,
	slackCfg Config,
	cfg channel.ChannelConfig,
	handler channel.InboundHandler,
	botUserID string,
) {
	smHandler := socketmode.NewSocketmodeHandler(sm)

	smHandler.HandleEvents(slackevents.Message, func(evt *socketmode.Event, client *socketmode.Client) {
		client.Ack(*evt.Request)
		a.handleMessageEvent(ctx, evt, api, slackCfg, cfg, handler, botUserID)
	})

	smHandler.HandleEvents(slackevents.AppMention, func(evt *socketmode.Event, client *socketmode.Client) {
		client.Ack(*evt.Request)
		a.handleAppMentionEvent(ctx, evt, api, slackCfg, cfg, handler, botUserID)
	})

	smHandler.Handle(socketmode.EventTypeSlashCommand, func(evt *socketmode.Event, client *socketmode.Client) {
		cmd, ok := evt.Data.(slackapi.SlashCommand)
		if !ok {
			return
		}
		client.Ack(*evt.Request)
		a.handleSlashCommand(ctx, cmd, slackCfg, cfg, handler)
	})

	smHandler.HandleDefault(func(evt *socketmode.Event, client *socketmode.Client) {
		// Acknowledge unhandled events to avoid timeout warnings
		if evt.Request != nil {
			client.Ack(*evt.Request)
		}
	})

	if err := smHandler.RunEventLoopContext(ctx); err != nil && ctx.Err() == nil {
		if a.logger != nil {
			a.logger.Error("socket mode loop exited", slog.String("config_id", cfg.ID), slog.Any("error", err))
		}
	}
}

func (a *SlackAdapter) handleMessageEvent(
	ctx context.Context,
	evt *socketmode.Event,
	api *slackapi.Client,
	slackCfg Config,
	cfg channel.ChannelConfig,
	handler channel.InboundHandler,
	botUserID string,
) {
	eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}
	msgEvent, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}

	// Ignore bot messages and message subtypes like edits/deletes
	if msgEvent.BotID != "" || msgEvent.User == "" {
		return
	}
	if msgEvent.SubType != "" {
		return
	}

	if a.isDuplicateInbound(slackCfg.BotToken, msgEvent.TimeStamp) {
		return
	}

	text := strings.TrimSpace(msgEvent.Text)
	if text == "" {
		return
	}

	chatType := slackChannelType(msgEvent.ChannelType)
	isMentioned := isBotMentioned(text, botUserID)
	isThread := msgEvent.ThreadTimeStamp != ""

	// Strip bot mention from text for cleaner processing
	text = stripBotMention(text, botUserID)

	// Resolve display name (best-effort)
	displayName := msgEvent.User
	if userInfo, err := api.GetUserInfo(msgEvent.User); err == nil && userInfo != nil {
		if userInfo.Profile.DisplayName != "" {
			displayName = userInfo.Profile.DisplayName
		} else if userInfo.RealName != "" {
			displayName = userInfo.RealName
		}
	}

	// Determine the reply target: use thread_ts if in a thread, otherwise use message ts as thread parent
	replyTarget := msgEvent.Channel
	threadID := ""
	if msgEvent.ThreadTimeStamp != "" {
		threadID = msgEvent.ThreadTimeStamp
	}

	msg := channel.InboundMessage{
		Channel: Type,
		Message: channel.Message{
			ID:     msgEvent.TimeStamp,
			Format: channel.MessageFormatPlain,
			Text:   text,
			Thread: threadRef(threadID),
		},
		BotID:       cfg.BotID,
		ReplyTarget: replyTarget,
		Sender: channel.Identity{
			SubjectID:   msgEvent.User,
			DisplayName: displayName,
			Attributes: map[string]string{
				"user_id":  msgEvent.User,
				"username": displayName,
			},
		},
		Conversation: channel.Conversation{
			ID:       msgEvent.Channel,
			Type:     chatType,
			ThreadID: threadID,
		},
		ReceivedAt: time.Now().UTC(),
		Source:     "slack",
		Metadata: map[string]any{
			"team_id":         eventsAPIEvent.TeamID,
			"is_mentioned":    isMentioned,
			"is_thread":       isThread,
			"thread_ts":       msgEvent.ThreadTimeStamp,
			"is_reply_to_bot": false,
		},
	}

	if a.logger != nil {
		a.logger.Info("inbound received",
			slog.String("config_id", cfg.ID),
			slog.String("chat_type", chatType),
			slog.String("user_id", msgEvent.User),
			slog.String("username", displayName),
			slog.String("text", common.SummarizeText(text)),
		)
	}

	go func() {
		if err := handler(ctx, cfg, msg); err != nil && a.logger != nil {
			a.logger.Error("handle inbound failed", slog.String("config_id", cfg.ID), slog.Any("error", err))
		}
	}()
}

func (a *SlackAdapter) handleAppMentionEvent(
	ctx context.Context,
	evt *socketmode.Event,
	api *slackapi.Client,
	slackCfg Config,
	cfg channel.ChannelConfig,
	handler channel.InboundHandler,
	botUserID string,
) {
	eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}
	mentionEvent, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.AppMentionEvent)
	if !ok {
		return
	}

	if mentionEvent.BotID != "" {
		return
	}

	if a.isDuplicateInbound(slackCfg.BotToken, mentionEvent.TimeStamp) {
		return
	}

	text := strings.TrimSpace(mentionEvent.Text)
	if text == "" {
		return
	}

	text = stripBotMention(text, botUserID)

	displayName := mentionEvent.User
	if userInfo, err := api.GetUserInfo(mentionEvent.User); err == nil && userInfo != nil {
		if userInfo.Profile.DisplayName != "" {
			displayName = userInfo.Profile.DisplayName
		} else if userInfo.RealName != "" {
			displayName = userInfo.RealName
		}
	}

	threadID := ""
	if mentionEvent.ThreadTimeStamp != "" {
		threadID = mentionEvent.ThreadTimeStamp
	}

	msg := channel.InboundMessage{
		Channel: Type,
		Message: channel.Message{
			ID:     mentionEvent.TimeStamp,
			Format: channel.MessageFormatPlain,
			Text:   text,
			Thread: threadRef(threadID),
		},
		BotID:       cfg.BotID,
		ReplyTarget: mentionEvent.Channel,
		Sender: channel.Identity{
			SubjectID:   mentionEvent.User,
			DisplayName: displayName,
			Attributes: map[string]string{
				"user_id":  mentionEvent.User,
				"username": displayName,
			},
		},
		Conversation: channel.Conversation{
			ID:       mentionEvent.Channel,
			Type:     "channel",
			ThreadID: threadID,
		},
		ReceivedAt: time.Now().UTC(),
		Source:     "slack",
		Metadata: map[string]any{
			"team_id":         eventsAPIEvent.TeamID,
			"is_mentioned":    true,
			"is_thread":       threadID != "",
			"thread_ts":       mentionEvent.ThreadTimeStamp,
			"is_reply_to_bot": false,
		},
	}

	if a.logger != nil {
		a.logger.Info("app mention received",
			slog.String("config_id", cfg.ID),
			slog.String("user_id", mentionEvent.User),
			slog.String("text", common.SummarizeText(text)),
		)
	}

	go func() {
		if err := handler(ctx, cfg, msg); err != nil && a.logger != nil {
			a.logger.Error("handle app mention failed", slog.String("config_id", cfg.ID), slog.Any("error", err))
		}
	}()
}

func (a *SlackAdapter) handleSlashCommand(
	ctx context.Context,
	cmd slackapi.SlashCommand,
	slackCfg Config,
	cfg channel.ChannelConfig,
	handler channel.InboundHandler,
) {
	if a.isDuplicateInbound(slackCfg.BotToken, cmd.TriggerID) {
		return
	}

	text := strings.TrimSpace(cmd.Text)
	if text == "" {
		text = cmd.Command
	} else {
		text = cmd.Command + " " + text
	}

	msg := channel.InboundMessage{
		Channel: Type,
		Message: channel.Message{
			ID:     cmd.TriggerID,
			Format: channel.MessageFormatPlain,
			Text:   text,
		},
		BotID:       cfg.BotID,
		ReplyTarget: cmd.ChannelID,
		Sender: channel.Identity{
			SubjectID:   cmd.UserID,
			DisplayName: cmd.UserName,
			Attributes: map[string]string{
				"user_id":  cmd.UserID,
				"username": cmd.UserName,
			},
		},
		Conversation: channel.Conversation{
			ID:   cmd.ChannelID,
			Type: "channel",
		},
		ReceivedAt: time.Now().UTC(),
		Source:     "slack",
		Metadata: map[string]any{
			"team_id":      cmd.TeamID,
			"is_mentioned": true,
			"is_command":   true,
			"command":      cmd.Command,
		},
	}

	if a.logger != nil {
		a.logger.Info("slash command received",
			slog.String("config_id", cfg.ID),
			slog.String("command", cmd.Command),
			slog.String("user_id", cmd.UserID),
			slog.String("text", common.SummarizeText(text)),
		)
	}

	go func() {
		if err := handler(ctx, cfg, msg); err != nil && a.logger != nil {
			a.logger.Error("handle slash command failed", slog.String("config_id", cfg.ID), slog.Any("error", err))
		}
	}()
}

func (a *SlackAdapter) Send(ctx context.Context, cfg channel.ChannelConfig, msg channel.OutboundMessage) error {
	slackCfg, err := parseConfig(cfg.Credentials)
	if err != nil {
		return err
	}

	api, _, err := a.getOrCreateClient(slackCfg, cfg.ID)
	if err != nil {
		return err
	}

	target := strings.TrimSpace(msg.Target)
	if target == "" {
		return fmt.Errorf("slack target is required")
	}

	return a.sendSlackMessage(ctx, api, target, cfg, msg)
}

func (a *SlackAdapter) sendSlackMessage(ctx context.Context, api *slackapi.Client, channelID string, cfg channel.ChannelConfig, msg channel.OutboundMessage) error {
	text := truncateSlackText(msg.Message.Text)

	opts := []slackapi.MsgOption{
		slackapi.MsgOptionText(text, false),
	}

	if msg.Message.Reply != nil && msg.Message.Reply.MessageID != "" {
		opts = append(opts, slackapi.MsgOptionTS(msg.Message.Reply.MessageID))
	}
	if msg.Message.Thread != nil && msg.Message.Thread.ID != "" {
		opts = append(opts, slackapi.MsgOptionTS(msg.Message.Thread.ID))
	}

	if text == "" && len(msg.Message.Attachments) == 0 {
		return fmt.Errorf("cannot send empty message: no content and no valid attachments")
	}

	// Send text message first
	if text != "" {
		_, _, err := api.PostMessageContext(ctx, channelID, opts...)
		if err != nil {
			return err
		}
	}

	// Upload attachments separately
	for _, att := range msg.Message.Attachments {
		if err := a.uploadAttachment(ctx, api, channelID, cfg, att, ""); err != nil {
			if a.logger != nil {
				a.logger.Error("upload attachment failed", slog.Any("error", err))
			}
		}
	}

	return nil
}

func (a *SlackAdapter) uploadAttachment(ctx context.Context, api *slackapi.Client, channelID string, cfg channel.ChannelConfig, att channel.Attachment, threadTS string) error {
	name := att.Name
	if name == "" {
		name = "attachment"
		ext := mimeExtension(att.Mime)
		if ext != "" {
			name += ext
		}
	}

	var reader io.Reader
	var size int

	var botID string
	if att.Metadata != nil {
		if bid, ok := att.Metadata["bot_id"].(string); ok && bid != "" {
			botID = bid
		}
	}

	a.mu.RLock()
	opener := a.assets
	a.mu.RUnlock()

	if att.ContentHash != "" && botID != "" && opener != nil {
		if rc, _, err := opener.Open(ctx, botID, att.ContentHash); err == nil {
			data, _ := io.ReadAll(rc)
			rc.Close()
			if len(data) > 0 {
				reader = bytes.NewReader(data)
				size = len(data)
			}
		}
	}

	if reader == nil && att.Base64 != "" {
		data, err := base64DataURLToBytes(att.Base64)
		if err == nil {
			reader = bytes.NewReader(data)
			size = len(data)
		}
	}

	if reader == nil && att.URL != "" {
		resp, err := http.Get(att.URL)
		if err == nil {
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			reader = bytes.NewReader(data)
			size = len(data)
		}
	}

	if reader == nil {
		return nil
	}

	params := slackapi.UploadFileParameters{
		Reader:         reader,
		Filename:       name,
		FileSize:       size,
		Channel:        channelID,
		ThreadTimestamp: threadTS,
	}

	_, err := api.UploadFileContext(ctx, params)
	return err
}

func (a *SlackAdapter) OpenStream(ctx context.Context, cfg channel.ChannelConfig, target string, opts channel.StreamOptions) (channel.OutboundStream, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("slack target is required")
	}

	slackCfg, err := parseConfig(cfg.Credentials)
	if err != nil {
		return nil, err
	}

	api, _, err := a.getOrCreateClient(slackCfg, cfg.ID)
	if err != nil {
		return nil, err
	}

	return &slackOutboundStream{
		adapter:  a,
		cfg:      cfg,
		api:      api,
		target:   target,
		reply:    opts.Reply,
		threadTS: threadTSFromOpts(opts),
	}, nil
}

func (a *SlackAdapter) ProcessingStarted(ctx context.Context, cfg channel.ChannelConfig, msg channel.InboundMessage, info channel.ProcessingStatusInfo) (channel.ProcessingStatusHandle, error) {
	return channel.ProcessingStatusHandle{}, nil
}

func (a *SlackAdapter) ProcessingCompleted(ctx context.Context, cfg channel.ChannelConfig, msg channel.InboundMessage, info channel.ProcessingStatusInfo, handle channel.ProcessingStatusHandle) error {
	return nil
}

func (a *SlackAdapter) ProcessingFailed(ctx context.Context, cfg channel.ChannelConfig, msg channel.InboundMessage, info channel.ProcessingStatusInfo, handle channel.ProcessingStatusHandle, cause error) error {
	return nil
}

func (a *SlackAdapter) React(ctx context.Context, cfg channel.ChannelConfig, target string, messageID string, emoji string) error {
	slackCfg, err := parseConfig(cfg.Credentials)
	if err != nil {
		return err
	}

	api, _, err := a.getOrCreateClient(slackCfg, cfg.ID)
	if err != nil {
		return err
	}

	emoji = strings.Trim(emoji, ":")
	return api.AddReactionContext(ctx, emoji, slackapi.ItemRef{
		Channel:   target,
		Timestamp: messageID,
	})
}

func (a *SlackAdapter) Unreact(ctx context.Context, cfg channel.ChannelConfig, target string, messageID string, emoji string) error {
	slackCfg, err := parseConfig(cfg.Credentials)
	if err != nil {
		return err
	}

	api, _, err := a.getOrCreateClient(slackCfg, cfg.ID)
	if err != nil {
		return err
	}

	emoji = strings.Trim(emoji, ":")
	return api.RemoveReactionContext(ctx, emoji, slackapi.ItemRef{
		Channel:   target,
		Timestamp: messageID,
	})
}

func (a *SlackAdapter) NormalizeConfig(raw map[string]any) (map[string]any, error) {
	return normalizeConfig(raw)
}

func (a *SlackAdapter) NormalizeUserConfig(raw map[string]any) (map[string]any, error) {
	return normalizeUserConfig(raw)
}

func (a *SlackAdapter) NormalizeTarget(raw string) string {
	return normalizeTarget(raw)
}

func (a *SlackAdapter) ResolveTarget(userConfig map[string]any) (string, error) {
	return resolveTarget(userConfig)
}

func (a *SlackAdapter) MatchBinding(config map[string]any, criteria channel.BindingCriteria) bool {
	return matchBinding(config, criteria)
}

func (a *SlackAdapter) BuildUserConfig(identity channel.Identity) map[string]any {
	return buildUserConfig(identity)
}

// --- helpers ---

func (a *SlackAdapter) isDuplicateInbound(token, msgID string) bool {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(msgID) == "" {
		return false
	}

	now := time.Now().UTC()
	expireBefore := now.Add(-inboundDedupTTL)

	a.mu.Lock()
	defer a.mu.Unlock()

	for key, seenAt := range a.seenMessages {
		if seenAt.Before(expireBefore) {
			delete(a.seenMessages, key)
		}
	}

	seenKey := token + ":" + msgID
	if _, ok := a.seenMessages[seenKey]; ok {
		return true
	}
	a.seenMessages[seenKey] = now
	return false
}

func (a *SlackAdapter) clearClientState(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.clients, token)
	delete(a.smClients, token)
}

func truncateSlackText(text string) string {
	const slackMaxLength = 40000
	if len(text) > slackMaxLength {
		text = text[:slackMaxLength-3] + "..."
	}
	return text
}

func slackChannelType(ct string) string {
	switch ct {
	case "im":
		return "direct"
	case "mpim", "mim":
		return "group"
	case "channel":
		return "channel"
	case "group":
		return "group"
	default:
		return "channel"
	}
}

func isBotMentioned(text, botUserID string) bool {
	mention := "<@" + botUserID + ">"
	return strings.Contains(text, mention)
}

func stripBotMention(text, botUserID string) string {
	mention := "<@" + botUserID + ">"
	text = strings.ReplaceAll(text, mention, "")
	return strings.TrimSpace(text)
}

func threadRef(threadID string) *channel.ThreadRef {
	if threadID == "" {
		return nil
	}
	return &channel.ThreadRef{ID: threadID}
}

func threadTSFromOpts(opts channel.StreamOptions) string {
	if opts.Reply != nil && opts.Reply.MessageID != "" {
		return opts.Reply.MessageID
	}
	return ""
}

func base64DataURLToBytes(dataURL string) ([]byte, error) {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid data URL")
	}
	return base64.StdEncoding.DecodeString(parts[1])
}

func mimeExtension(mime string) string {
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "audio/wav":
		return ".wav"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	default:
		return ""
	}
}
