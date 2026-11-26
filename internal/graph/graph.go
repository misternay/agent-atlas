// Package graph projects a live snapshot into Agent → Tool → Resource nodes
// and edges (design §8.2). Every resource edge carries its evidence level and
// the source event sequence that established it.
package graph

import (
	"sort"

	"agentatlas/internal/correlate"
	"agentatlas/internal/storage/sqlite"
)

// Node is a graph node.
type Node struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"` // session | tool | resource | group
	Label  string            `json:"label"`
	Status string            `json:"status,omitempty"`
	Data   map[string]any    `json:"data,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

// Edge is a graph edge.
type Edge struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Relation string `json:"relation"` // invokes | reads | writes | connects_to | belongs_to
	Evidence string `json:"evidence"`
	EventSeq int64  `json:"event_seq,omitempty"`
}

// Graph is the projected graph.
type Graph struct {
	Cursor int64  `json:"cursor"`
	Nodes  []Node `json:"nodes"`
	Edges  []Edge `json:"edges"`
	// Truncated reports tool nodes that were grouped to keep the graph readable.
	Truncated map[string]int `json:"truncated,omitempty"`
}

// Build projects a snapshot. At most maxToolsPerSession tool nodes are kept per
// session (all running/stale ones plus the most recent finished ones); the
// rest are folded into one group node per session.
func Build(snap *sqlite.Snapshot, maxToolsPerSession int) Graph {
	if maxToolsPerSession <= 0 {
		maxToolsPerSession = 40
	}
	g := Graph{Cursor: snap.Cursor, Nodes: []Node{}, Edges: []Edge{}}
	sessions := map[string]bool{}
	for _, s := range snap.Sessions {
		sessions[s.ID] = true
		g.Nodes = append(g.Nodes, Node{ID: "session:" + s.ID, Type: "session", Label: s.Harness + " · " + s.Workspace, Status: s.Status,
			Data: map[string]any{"harness": s.Harness, "workspace": s.Workspace, "last_event_at_us": s.LastEventAt, "tool_count": s.ToolCount}})
	}
	bySession := map[string][]correlate.Span{}
	for _, sp := range snap.Spans {
		if sp.Kind == correlate.KindTool && sessions[sp.SessionID] {
			bySession[sp.SessionID] = append(bySession[sp.SessionID], sp)
		}
	}
	kept := map[string]bool{}
	for sid, spans := range bySession {
		sort.Slice(spans, func(i, j int) bool {
			oi, oj := open(spans[i].Status), open(spans[j].Status)
			if oi != oj {
				return oi
			}
			return spans[i].StartedAt > spans[j].StartedAt
		})
		folded := 0
		for i, sp := range spans {
			if i < maxToolsPerSession || open(sp.Status) {
				kept[sp.ID] = true
				label := sp.Name
				if sp.TargetLabel != "" {
					label += " · " + sp.TargetLabel
				}
				g.Nodes = append(g.Nodes, Node{ID: "tool:" + sp.ID, Type: "tool", Label: label, Status: sp.Status,
					Data: map[string]any{"span_id": sp.ID, "trace_id": sp.TraceID, "tool": sp.Name, "category": sp.ToolCategory, "evidence": sp.StatusEvidence, "started_at_us": sp.StartedAt, "duration_us": sp.DurationUS}})
				g.Edges = append(g.Edges, Edge{ID: "e:inv:" + sp.ID, Source: "session:" + sid, Target: "tool:" + sp.ID, Relation: "invokes", Evidence: sp.StatusEvidence, EventSeq: sp.StartSeq})
			} else {
				folded++
			}
		}
		if folded > 0 {
			if g.Truncated == nil {
				g.Truncated = map[string]int{}
			}
			g.Truncated[sid] = folded
			gid := "group:" + sid
			g.Nodes = append(g.Nodes, Node{ID: gid, Type: "group", Label: "+" + itoa(folded) + " earlier calls", Data: map[string]any{"count": folded}})
			g.Edges = append(g.Edges, Edge{ID: "e:grp:" + sid, Source: "session:" + sid, Target: gid, Relation: "belongs_to", Evidence: "derived"})
		}
	}
	resSeen := map[string]bool{}
	for _, l := range snap.Links {
		if !kept[l.SpanID] {
			continue
		}
		rid := "res:" + l.ResourceID
		if !resSeen[rid] {
			resSeen[rid] = true
			g.Nodes = append(g.Nodes, Node{ID: rid, Type: "resource", Label: l.Label, Data: map[string]any{"kind": l.Kind}})
		}
		g.Edges = append(g.Edges, Edge{ID: "e:" + l.SpanID + ":" + l.ResourceID + ":" + l.Relation, Source: "tool:" + l.SpanID, Target: rid, Relation: l.Relation, Evidence: l.Evidence, EventSeq: l.EventSeq})
	}
	return g
}

func open(status string) bool { return status == "running" || status == "stale" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
