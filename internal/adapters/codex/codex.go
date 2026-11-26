// Package codex adapts Codex CLI hook payloads. Payload shapes follow
// codex-rs/hooks/src/schema.rs (openai/codex main). Live capture on Codex CLI
// 0.170.0 was not possible in the Phase 0 spike because Codex requires each
// hook to be trusted before it runs; see docs/compatibility.md.
package codex

import (
	"encoding/json"
	"errors"
	"strconv"

	"agentatlas/internal/adapters"
	"agentatlas/internal/event"
)

type payload struct {
	SessionID      string          `json:"session_id"`
	TurnID         string          `json:"turn_id"`
	AgentID        string          `json:"agent_id"`
	AgentType      string          `json:"agent_type"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	Model          string          `json:"model"`
	PermissionMode string          `json:"permission_mode"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	ToolUseID      string          `json:"tool_use_id"`
	Source         string          `json:"source"`
	Reason         string          `json:"reason"`
	Trigger        string          `json:"trigger"`
}

// Adapter implements adapters.Adapter for Codex.
type Adapter struct{}

// New returns a Codex adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapters.Adapter.
func (*Adapter) Name() string { return event.HarnessCodex }

// Capabilities implements adapters.Adapter.
func (*Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		SessionLifecycle: true, TurnLifecycle: true, ToolStart: true, ToolFinish: true,
		ToolFailure: false, ToolDenied: false, Subagents: true, MCPCalls: true,
		HookEvents:        []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionRequest", "Stop", "SubagentStart", "SubagentStop", "Interrupt"},
		ToolMatcherEvents: []string{"PreToolUse", "PostToolUse", "PermissionRequest"},
		PayloadBasis:      "documented",
		Notes: []string{
			"Codex requires each configured hook to be trusted (reviewed) before it runs; after `atlas install-hooks` approve the Atlas hooks in Codex.",
			"Codex has no PostToolUseFailure hook: failure is marked only when tool_response carries an explicit non-zero exit_code or success=false.",
			"Live tool start and completion were verified in Codex CLI 0.172.0 and Codex Desktop 26.1120.50957.",
			"With code mode enabled Codex wraps tools in an exec host; per-tool hooks fire for inner tools when the runtime dispatches them.",
		},
	}
}

var errNoSession = errors.New("codex payload missing session_id")

// Normalize implements adapters.Adapter.
func (a *Adapter) Normalize(raw []byte, meta adapters.Meta) ([]event.Event, error) {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.SessionID == "" {
		return nil, errNoSession
	}
	e := adapters.Base(meta, event.HarnessCodex, p.HookEventName, p.SessionID, p.Cwd, 0)
	e.TurnID = p.TurnID
	e.AgentID = p.AgentID
	attrs := map[string]string{}
	if p.Model != "" {
		attrs["model"] = p.Model
	}
	if p.PermissionMode != "" {
		attrs["permission_mode"] = p.PermissionMode
	}
	e.Attrs = attrs
	tool := func() {
		adapters.ApplyToolInfo(&e, adapters.ClassifyTool(p.ToolName, p.ToolInput, p.Cwd))
		e.ToolCallID = p.ToolUseID
	}
	switch p.HookEventName {
	case "SessionStart":
		e.Type = event.TypeSessionStarted
		attrs["source"] = p.Source
	case "SessionEnd":
		e.Type = event.TypeSessionEnded
		attrs["reason"] = p.Reason
	case "UserPromptSubmit":
		e.Type = event.TypeTurnStarted
	case "Stop":
		e.Type = event.TypeTurnEnded
	case "Interrupt":
		e.Type = event.TypeTurnEnded
		attrs["interrupted"] = "true"
		attrs["outcome_basis"] = "interrupt_hook"
	case "PreToolUse":
		e.Type = event.TypeToolStarted
		e.Status = event.StatusRunning
		tool()
	case "PostToolUse":
		tool()
		e.Type = event.TypeToolCompleted
		e.Status = event.StatusCompleted
		if code, ok := exitCode(p.ToolResponse); ok {
			attrs["exit_code"] = strconv.Itoa(code)
			if code != 0 {
				e.Type = event.TypeToolFailed
				e.Status = event.StatusFailed
				e.Evidence = event.EvidenceDerived
				attrs["outcome_basis"] = "exit_code"
			}
		} else if explicitFailure(p.ToolResponse) {
			e.Type = event.TypeToolFailed
			e.Status = event.StatusFailed
			e.Evidence = event.EvidenceDerived
			attrs["outcome_basis"] = "success_false"
		}
	case "PermissionRequest":
		e.Type = event.TypePermissionRequested
		tool()
	case "SubagentStart":
		e.Type = event.TypeSubagentStarted
		attrs["agent_type"] = p.AgentType
	case "SubagentStop":
		e.Type = event.TypeSubagentStopped
		attrs["agent_type"] = p.AgentType
	default:
		return nil, nil
	}
	return []event.Event{e}, nil
}

// exitCode looks for an explicit numeric exit code in a tool response object.
func exitCode(raw json.RawMessage) (int, bool) {
	m := adapters.AsObject(raw)
	if m == nil {
		return 0, false
	}
	for _, k := range []string{"exit_code", "exitCode"} {
		if v, ok := m[k].(float64); ok {
			return int(v), true
		}
	}
	if md, ok := m["metadata"].(map[string]any); ok {
		if v, ok := md["exit_code"].(float64); ok {
			return int(v), true
		}
	}
	return 0, false
}

func explicitFailure(raw json.RawMessage) bool {
	m := adapters.AsObject(raw)
	if m == nil {
		return false
	}
	if v, ok := m["success"].(bool); ok && !v {
		return true
	}
	return false
}
