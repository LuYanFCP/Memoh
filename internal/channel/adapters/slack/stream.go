package slack

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/memohai/memoh/internal/channel"
	slackapi "github.com/slack-go/slack"
)

const streamUpdateInterval = 1500 * time.Millisecond

type slackOutboundStream struct {
	adapter  *SlackAdapter
	cfg      channel.ChannelConfig
	api      *slackapi.Client
	target   string
	reply    *channel.ReplyRef
	threadTS string
	closed   atomic.Bool
	mu       sync.Mutex
	msgTS    string
	buffer   strings.Builder
	lastUpdate time.Time
}

func (s *slackOutboundStream) Push(ctx context.Context, event channel.StreamEvent) error {
	if s == nil || s.adapter == nil {
		return fmt.Errorf("slack stream not configured")
	}
	if s.closed.Load() {
		return fmt.Errorf("slack stream is closed")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	switch event.Type {
	case channel.StreamEventStatus:
		if event.Status == channel.StreamStatusStarted {
			return s.ensureMessage("Thinking...")
		}
		return nil

	case channel.StreamEventDelta:
		if event.Delta == "" || event.Phase == channel.StreamPhaseReasoning {
			return nil
		}
		s.mu.Lock()
		s.buffer.WriteString(event.Delta)
		s.mu.Unlock()

		if time.Since(s.lastUpdate) > streamUpdateInterval {
			return s.updateMessage()
		}
		return nil

	case channel.StreamEventFinal:
		if event.Final != nil && !event.Final.Message.IsEmpty() {
			finalText := strings.TrimSpace(event.Final.Message.PlainText())
			if finalText != "" {
				return s.finalizeMessage(finalText)
			}
		}
		s.mu.Lock()
		finalText := strings.TrimSpace(s.buffer.String())
		s.mu.Unlock()
		if finalText != "" {
			return s.finalizeMessage(finalText)
		}
		return nil

	case channel.StreamEventError:
		errText := strings.TrimSpace(event.Error)
		if errText == "" {
			return nil
		}
		return s.finalizeMessage("Error: " + errText)

	case channel.StreamEventAttachment:
		if len(event.Attachments) == 0 {
			return nil
		}
		s.mu.Lock()
		finalText := strings.TrimSpace(s.buffer.String())
		s.mu.Unlock()
		if finalText != "" {
			if err := s.finalizeMessage(finalText); err != nil {
				return err
			}
		}
		for _, att := range event.Attachments {
			if err := s.sendAttachment(ctx, att); err != nil {
				return err
			}
		}
		return nil

	case channel.StreamEventAgentStart, channel.StreamEventAgentEnd,
		channel.StreamEventPhaseStart, channel.StreamEventPhaseEnd,
		channel.StreamEventProcessingStarted, channel.StreamEventProcessingCompleted,
		channel.StreamEventProcessingFailed,
		channel.StreamEventToolCallStart, channel.StreamEventToolCallEnd:
		return nil

	default:
		return fmt.Errorf("unsupported stream event type: %s", event.Type)
	}
}

func (s *slackOutboundStream) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	s.closed.Store(true)
	return nil
}

func (s *slackOutboundStream) ensureMessage(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.msgTS != "" {
		return nil
	}

	content := truncateSlackText(text)

	opts := []slackapi.MsgOption{
		slackapi.MsgOptionText(content, false),
	}
	if ts := s.effectiveThreadTS(); ts != "" {
		opts = append(opts, slackapi.MsgOptionTS(ts))
	}

	_, ts, err := s.api.PostMessage(s.target, opts...)
	if err != nil {
		return err
	}

	s.msgTS = ts
	s.lastUpdate = time.Now()
	return nil
}

func (s *slackOutboundStream) updateMessage() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.msgTS == "" {
		return nil
	}

	content := s.buffer.String()
	if content == "" {
		return nil
	}

	content = truncateSlackText(content)

	_, _, _, err := s.api.UpdateMessage(s.target, s.msgTS,
		slackapi.MsgOptionText(content, false),
	)
	if err != nil {
		return err
	}

	s.lastUpdate = time.Now()
	return nil
}

func (s *slackOutboundStream) finalizeMessage(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	text = truncateSlackText(text)

	if s.msgTS == "" {
		opts := []slackapi.MsgOption{
			slackapi.MsgOptionText(text, false),
		}
		if ts := s.effectiveThreadTS(); ts != "" {
			opts = append(opts, slackapi.MsgOptionTS(ts))
		}

		_, ts, err := s.api.PostMessage(s.target, opts...)
		if err != nil {
			return err
		}
		s.msgTS = ts
		s.lastUpdate = time.Now()
		return nil
	}

	_, _, _, err := s.api.UpdateMessage(s.target, s.msgTS,
		slackapi.MsgOptionText(text, false),
	)
	return err
}

func (s *slackOutboundStream) sendAttachment(ctx context.Context, att channel.Attachment) error {
	return s.adapter.uploadAttachment(ctx, s.api, s.target, s.cfg, att, s.effectiveThreadTS())
}

func (s *slackOutboundStream) effectiveThreadTS() string {
	if s.threadTS != "" {
		return s.threadTS
	}
	if s.reply != nil && s.reply.MessageID != "" {
		return s.reply.MessageID
	}
	return ""
}
