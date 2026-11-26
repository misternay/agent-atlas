// Package claude adapts Claude Code hook payloads (verified against Claude Code
// 2.1.330 in the Phase 0 spike; see docs/compatibility.md).
package claude

import (
	"encoding/json"
	"errors"
	"strings"

	"agentatlas/internal/adapters"
	"agentatlas/internal/event"
)

// payload lists the native fields Atlas reads. Fields that carry content
// (prompt, tool_response, error, last_assistant_message, transcript_path) are
// decoded only to derive flags in memory and are never copied to events.
type payload struct {
	SessionID      string          `json:"session_id"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	PromptID       string          `json:"prompt_id"`
	PermissionMode string          `json:"permission_mode"`
	Model          json.RawMessage `json:"model"`
	Effort         *struct {
		Level string `json:"level"`
	} `json:"effort"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolResponse json.RawMessage `json:"tool_response"`
	DurationMS   float64         `json:"duration_ms"`
	IsInterrupt  bool            `json:"is_interrupt"`
	Source       string          `json:"source"`
	Reason       string          `json:"reason"`
	AgentID      string          `json:"agent_id"`
	AgentType    string          `json:"agent_type"`
	Trigger      string          `json:"trigger"`
	ToolCalls    []struct {
		ToolName     string          `json:"tool_name"`
		ToolInput    json.RawMessage `json:"tool_input"`
		ToolUseID    string          `json:"tool_use_id"`
		ToolResponse json.RawMessage `json:"tool_response"`
	} `json:"tool_calls"`
}

// Adapter implements adapters.Adapter for Claude Code.
type Adapter struct{}

// New returns a Claude Code adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapters.Adapter.
func (*Adapter) Name() string { return event.HarnessClaude }

// Capabilities implements adapters.Adapter.
func (*Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		SessionLifecycle: true, TurnLifecycle: true, ToolStart: true, ToolFinish: true,
		ToolFailure: true, ToolDenied: true, Subagents: true, MCPCalls: true,
		HookEvents: []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse",
			"PostToolUseFailure", "PostToolBatch", "PermissionRequest", "PermissionDenied", "Stop", "StopFailure",
			"SubagentStart", "SubagentStop"},
		ToolMatcherEvents: []string{"PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "PermissionDenied"},
		PayloadBasis:      "observed",
		Notes: []string{
			"PreToolUse may fire without PostToolUse when another PreToolUse hook blocks the call; PostToolBatch then reports the block text (classified as denied/inferred).",
			"tool_input can differ between PreToolUse and PostToolUse when a hook rewrites it; correlation uses tool_use_id only.",
			"Payloads carry no timestamp; the shim stamps receipt time.",
			"Bash internals (files/network touched by spawned processes) are not observable; parsed command operands are reported as inferred.",
		},
	}
}

var errNoSession = errors.New("claude payload missing session_id")

// Normalize implements adapters.Adapter.
func (a *Adapter) Normalize(raw []byte, meta adapters.Meta) ([]event.Event, error) {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.SessionID == "" {
		return nil, errNoSession
	}
	base := func(idx int) event.Event {
		e := adapters.Base(meta, event.HarnessClaude, p.HookEventName, p.SessionID, p.Cwd, idx)
		e.TurnID = p.PromptID
		e.AgentID = p.AgentID
		return e
	}
	attrs := func() map[string]string {
		m := map[string]string{}
		if p.PermissionMode != "" {
			m["permission_mode"] = p.PermissionMode
		}
		if p.Effort != nil && p.Effort.Level != "" {
			m["effort"] = p.Effort.Level
		}
		if s := modelName(p.Model); s != "" {
			m["model"] = s
		}
		return m
	}
	tool := func(e *event.Event) {
		adapters.ApplyToolInfo(e, adapters.ClassifyTool(p.ToolName, p.ToolInput, p.Cwd))
		e.ToolCallID = p.ToolUseID
	}

	switch p.HookEventName {
	case "SessionStart":
		e := base(0)
		e.Type = event.TypeSessionStarted
		e.Attrs = attrs()
		e.Attrs["source"] = p.Source
		return []event.Event{e}, nil
	case "SessionEnd":
		e := base(0)
		e.Type = event.TypeSessionEnded
		e.Attrs = map[string]string{"reason": p.Reason}
		return []event.Event{e}, nil
	case "UserPromptSubmit":
		e := base(0)
		e.Type = event.TypeTurnStarted
		e.Attrs = attrs()
		return []event.Event{e}, nil
	case "Stop", "StopFailure":
		e := base(0)
		e.Type = event.TypeTurnEnded
		e.Attrs = attrs()
		if p.HookEventName == "StopFailure" {
			e.Attrs["outcome_basis"] = "stop_failure"
		}
		return []event.Event{e}, nil
	case "PreToolUse":
		e := base(0)
		e.Type = event.TypeToolStarted
		e.Status = event.StatusRunning
		e.Attrs = attrs()
		tool(&e)
		return []event.Event{e}, nil
	case "PostToolUse":
		e := base(0)
		e.Type = event.TypeToolCompleted
		e.Status = event.StatusCompleted
		e.Attrs = attrs()
		e.DurationMS = adapters.DurationPtr(p.DurationMS)
		tool(&e)
		return []event.Event{e}, nil
	case "PostToolUseFailure":
		e := base(0)
		e.Type = event.TypeToolFailed
		e.Status = event.StatusFailed
		e.Attrs = attrs()
		e.DurationMS = adapters.DurationPtr(p.DurationMS)
		if p.IsInterrupt {
			e.Attrs["interrupted"] = "true"
			e.Attrs["outcome_basis"] = "interrupted"
		} else {
			e.Attrs["outcome_basis"] = "failure_hook"
		}
		tool(&e)
		return []event.Event{e}, nil
	case "PermissionDenied":
		e := base(0)
		e.Type = event.TypeToolDenied
		e.Status = event.StatusDenied
		e.Attrs = attrs()
		e.Attrs["outcome_basis"] = "permission_denied_hook"
		tool(&e)
		return []event.Event{e}, nil
	case "PermissionRequest":
		e := base(0)
		e.Type = event.TypePermissionRequested
		e.Attrs = attrs()
		tool(&e)
		return []event.Event{e}, nil
	case "PostToolBatch":
		out := make([]event.Event, 0, len(p.ToolCalls))
		for i, c := range p.ToolCalls {
			if c.ToolUseID == "" {
				continue
			}
			e := base(i + 1)
			e.Type = event.TypeToolBatchReported
			e.ToolCallID = c.ToolUseID
			e.Tool = &event.Tool{Name: c.ToolName}
			if basis := DeniedText(c.ToolResponse); basis != "" {
				e.Status = event.StatusDenied
				e.Evidence = event.EvidenceInferred
				e.Attrs = map[string]string{"outcome_basis": basis}
				if basis == "batch_hook_block_text" {
					e.Attrs["hook_blocked"] = "true"
				}
			} else {
				e.Status = event.StatusUnknown
				e.Evidence = event.EvidenceDerived
				e.Attrs = map[string]string{"outcome_basis": "batch_result_reported"}
			}
			out = append(out, e)
		}
		return out, nil
	case "SubagentStart", "SubagentStop":
		e := base(0)
		e.Type = event.TypeSubagentStarted
		if p.HookEventName == "SubagentStop" {
			e.Type = event.TypeSubagentStopped
		}
		e.Attrs = map[string]string{"agent_type": p.AgentType}
		return []event.Event{e}, nil
	}
	// Notification, PreCompact, PostCompact, etc. are intentionally not recorded.
	return nil, nil
}

// DeniedText classifies the text Claude Code returns to the model when a call
// never ran: a PreToolUse hook blocked it, or a permission rule denied it
// (both observed in the Phase 0 spike). It returns an outcome basis, or "".
// The response text itself is never retained.
func DeniedText(resp json.RawMessage) string {
	s := adapters.AsString(resp)
	if s == "" {
		return ""
	}
	head := s
	if len(head) > 400 {
		head = head[:400]
	}
	if strings.HasPrefix(head, "PreToolUse:") && strings.Contains(head, "hook") {
		return "batch_hook_block_text"
	}
	if strings.HasPrefix(head, "Permission to use ") && strings.Contains(head, "has been denied") {
		return "batch_permission_denied_text"
	}
	return ""
}

func modelName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s := adapters.AsString(raw); s != "" {
		return s
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"id", "display_name", "name"} {
			if s, ok := m[k].(string); ok {
				return s
			}
		}
	}
	return ""
}
