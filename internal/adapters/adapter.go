// Package adapters defines the capability-driven harness adapter interface
// (design §4.1) and helpers shared by the Claude Code and Codex adapters.
package adapters

import (
	"time"

	"agentatlas/internal/event"
)

// Meta is transport metadata attached by the shim.
type Meta struct {
	EnvelopeID string
	ReceivedAt time.Time
	HostID     string
}

// Adapter converts one native hook payload into zero or more normalized events.
// Implementations must not retain raw payload data beyond the call.
type Adapter interface {
	Name() string
	Normalize(raw []byte, meta Meta) ([]event.Event, error)
	Capabilities() Capabilities
}

// Capabilities describe which signals an adapter can produce when the
// corresponding native hooks are configured (design §4.2).
type Capabilities struct {
	SessionLifecycle bool `json:"session_lifecycle"`
	TurnLifecycle    bool `json:"turn_lifecycle"`
	ToolStart        bool `json:"tool_start"`
	ToolFinish       bool `json:"tool_finish"`
	ToolFailure      bool `json:"tool_failure"`
	ToolDenied       bool `json:"tool_denied"`
	Subagents        bool `json:"subagents"`
	MCPCalls         bool `json:"mcp_calls"`
	// Notes documents caveats discovered in the Phase 0 spike.
	Notes []string `json:"notes,omitempty"`
	// HookEvents lists native hook event names the installer should register.
	HookEvents []string `json:"hook_events"`
	// ToolMatcherEvents lists hook events that take a tool matcher.
	ToolMatcherEvents []string `json:"tool_matcher_events"`
	// Observed reports whether this adapter's payload shapes were captured from a
	// real run on this machine ("observed") or written from source/docs ("documented").
	PayloadBasis string `json:"payload_basis"`
}
