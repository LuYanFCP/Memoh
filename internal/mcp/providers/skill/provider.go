package skill

import (
	"context"
	"log/slog"

	mcpgw "github.com/memohai/memoh/internal/mcp"
)

const (
	toolUseSkill = "use_skill"
)

type Executor struct {
	logger *slog.Logger
}

func NewExecutor(log *slog.Logger) *Executor {
	if log == nil {
		log = slog.Default()
	}
	return &Executor{
		logger: log.With(slog.String("provider", "skill_tool")),
	}
}

func (e *Executor) ListTools(_ context.Context, session mcpgw.ToolSessionContext) ([]mcpgw.ToolDescriptor, error) {
	if session.IsSubagent {
		return []mcpgw.ToolDescriptor{}, nil
	}
	return []mcpgw.ToolDescriptor{
		{
			Name:        toolUseSkill,
			Description: "Use a skill if you think it is relevant to the current task",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"skillName": map[string]any{
						"type":        "string",
						"description": "The name of the skill to use",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "The reason why you think this skill is relevant to the current task",
					},
				},
				"required": []string{"skillName", "reason"},
			},
		},
	}, nil
}

func (e *Executor) CallTool(_ context.Context, session mcpgw.ToolSessionContext, toolName string, arguments map[string]any) (map[string]any, error) {
	if toolName != toolUseSkill {
		return nil, mcpgw.ErrToolNotFound
	}
	if session.IsSubagent {
		return mcpgw.BuildToolErrorResult("skill tools are not available in subagent context"), nil
	}

	skillName := mcpgw.StringArg(arguments, "skillName")
	reason := mcpgw.StringArg(arguments, "reason")
	if skillName == "" {
		return mcpgw.BuildToolErrorResult("skillName is required"), nil
	}

	return mcpgw.BuildToolSuccessResult(map[string]any{
		"success":   true,
		"skillName": skillName,
		"reason":    reason,
	}), nil
}
