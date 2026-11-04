package harness

import (
	"context"
	"fmt"

	"github.com/lannokaede/LanAgent/internal/llm"
)

// plan 只做一次模型调用；下一步由图上的条件边决定。
func (r *Runner) plan(ctx context.Context, s *runState) (*runState, error) {
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if s.round >= r.maxRounds {
		return s, fmt.Errorf("agent exceeded maximum model rounds (%d)", r.maxRounds)
	}
	s.round++
	req := &llm.ChatRequest{Messages: cloneMessages(s.messages), Tools: r.registry.ListSpecs(), ResponseFormat: r.format}
	if r.pinned != nil {
		if len(req.Messages) > 0 && req.Messages[0].Role == llm.RoleSystem {
			req.Messages[0].Content += "\n" + r.pinned()
		} else {
			req.Messages = append([]llm.Message{{Role: llm.RoleSystem, Content: r.pinned()}}, req.Messages...)
		}
	}
	// Context budgeting is explicit for each application profile.
	if r.budget.Window != 0 {
		prepared, stats, prepareErr := r.prepareContext(ctx, req)
		if prepareErr != nil {
			return s, prepareErr
		}
		s.usage.PromptTokens += stats.SummaryUsage.PromptTokens
		s.usage.CompletionTokens += stats.SummaryUsage.CompletionTokens
		req = prepared
		if stats.Removed > 0 && s.emit != nil {
			if err := s.emit(Event{Type: "context_compaction", Data: map[string]any{"before": stats.Before, "after": stats.After, "removed_messages": stats.Removed, "estimated_tokens": stats.Estimated}}); err != nil {
				return s, err
			}
		}
	}
	var response *llm.ChatResponse
	var err error
	if s.emit == nil {
		response, err = r.provider.Chat(ctx, req)
	} else {
		if err = s.emit(Event{Type: "delta", Data: map[string]any{"content": "", "reset": true, "round": s.round}}); err != nil {
			return s, err
		}
		if err := ctx.Err(); err != nil {
			return s, err
		}
		response, err = llm.ChatWithStream(ctx, r.provider, req, func(chunk string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if chunk == "" {
				return nil
			}
			return s.emit(Event{Type: "delta", Data: map[string]any{"content": chunk, "round": s.round}})
		})
	}
	if err != nil {
		return s, err
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if response == nil {
		return s, fmt.Errorf("model returned nil response")
	}
	if len(response.ToolCalls) == 0 && response.Content == "" {
		return s, fmt.Errorf("model returned empty answer")
	}
	seen := map[string]bool{}
	for _, call := range response.ToolCalls {
		if call.ID == "" || call.Name == "" {
			return s, fmt.Errorf("tool call is missing id or name")
		}
		if seen[call.ID] {
			return s, fmt.Errorf("duplicate tool call ID %q", call.ID)
		}
		seen[call.ID] = true
	}
	if len(response.ToolCalls) > 0 && s.emit != nil {
		if err := s.emit(Event{Type: "delta", Data: map[string]any{"content": "", "reset": true, "round": s.round}}); err != nil {
			return s, err
		}
	}
	s.usage.PromptTokens += response.Usage.PromptTokens
	s.usage.CompletionTokens += response.Usage.CompletionTokens
	s.messages = append(s.messages, llm.Message{Role: llm.RoleAssistant, Content: response.Content, ToolCalls: append([]llm.ToolCall(nil), response.ToolCalls...)})
	s.pending = append([]llm.ToolCall(nil), response.ToolCalls...)
	if len(s.pending) == 0 {
		final := *response
		s.final = &final
	}
	phase := "tools"
	if s.final != nil {
		phase = "done"
	}
	if err := s.checkpoint(phase); err != nil {
		return s, err
	}
	return s, nil
}
