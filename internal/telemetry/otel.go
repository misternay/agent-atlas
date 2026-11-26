// Package telemetry maps Atlas spans to OpenTelemetry (OTLP/JSON) and
// implements the optional, off-by-default OTLP/HTTP exporter (design §6.3).
// No OTel SDK dependency is used: the mapping targets the stable OTLP JSON
// encoding so SQLite stays authoritative and export stays a pure projection.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"agentatlas/internal/correlate"
)

// Versions this mapping was written against (pinned, see docs/architecture.md).
const (
	OTLPVersion       = "1.5.0"  // opentelemetry-proto JSON encoding
	SemconvVersion    = "1.37.0" // includes gen_ai.* tool attributes (development status)
	AtlasAttrsVersion = "1"      // version of atlas.* custom attributes
	ScopeName         = "agent-atlas"
)

// Status codes (OTLP).
const (
	statusUnset = 0
	statusOK    = 1
	statusError = 2
)

type kv struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func sv(k, v string) kv { return kv{k, map[string]string{"stringValue": v}} }
func iv(k string, v int64) kv {
	return kv{k, map[string]string{"intValue": strconv.FormatInt(v, 10)}}
}

// Span kinds (OTLP enum): INTERNAL=1, CLIENT=3.
const (
	kindInternal = 1
	kindClient   = 3
)

// OTLPSpan is one OTLP JSON span.
type OTLPSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano,omitempty"`
	Attributes        []kv           `json:"attributes"`
	Status            map[string]any `json:"status"`
}

// ToOTLP converts a session's spans and links into an OTLP ExportTraceServiceRequest JSON object.
func ToOTLP(sess *correlate.Session, spans []correlate.Span, links []correlate.Link) map[string]any {
	byspan := map[string][]correlate.Link{}
	for _, l := range links {
		byspan[l.SpanID] = append(byspan[l.SpanID], l)
	}
	out := make([]OTLPSpan, 0, len(spans))
	for _, sp := range spans {
		o := OTLPSpan{
			TraceID: sp.TraceID, SpanID: sp.ID, ParentSpanID: sp.ParentSpanID, Kind: kindInternal,
			StartTimeUnixNano: strconv.FormatInt(sp.StartedAt*1000, 10),
		}
		if sp.EndedAt != nil {
			o.EndTimeUnixNano = strconv.FormatInt(*sp.EndedAt*1000, 10)
		}
		attrs := []kv{
			sv("atlas.attrs.version", AtlasAttrsVersion),
			sv("atlas.harness", sess.Harness),
			sv("atlas.session.id", sess.SourceSessionID),
			sv("atlas.span.kind", sp.Kind),
			sv("atlas.status", sp.Status),
			sv("atlas.evidence", sp.StatusEvidence),
		}
		if sp.OutcomeBasis != "" {
			attrs = append(attrs, sv("atlas.outcome_basis", sp.OutcomeBasis))
		}
		switch sp.Kind {
		case correlate.KindTool:
			o.Name = "execute_tool " + sp.Name
			attrs = append(attrs, sv("gen_ai.operation.name", "execute_tool"), sv("gen_ai.tool.name", sp.Name))
			if sp.ToolCallID != "" {
				attrs = append(attrs, sv("gen_ai.tool.call.id", sp.ToolCallID))
			}
			if sp.ToolCategory != "" {
				attrs = append(attrs, sv("atlas.tool.category", sp.ToolCategory))
			}
			if sp.ToolCategory == "mcp" {
				o.Kind = kindClient
			}
			if sp.TargetLabel != "" {
				attrs = append(attrs, sv("atlas.target.kind", sp.TargetKind), sv("atlas.target.label", sp.TargetLabel))
			}
			for i, l := range byspan[sp.ID] {
				p := "atlas.resource." + strconv.Itoa(i)
				attrs = append(attrs, sv(p+".kind", l.Kind), sv(p+".label", l.Label), sv(p+".relation", l.Relation), sv(p+".evidence", l.Evidence))
			}
		case correlate.KindTurn:
			o.Name = "turn"
			attrs = append(attrs, sv("gen_ai.operation.name", "invoke_agent"))
		default:
			o.Name = "session " + sess.Harness
			attrs = append(attrs, sv("gen_ai.operation.name", "invoke_agent"), sv("gen_ai.agent.name", sess.Harness))
		}
		if sp.DurationUS != nil {
			attrs = append(attrs, iv("atlas.duration_us", *sp.DurationUS))
		}
		o.Attributes = attrs
		switch sp.Status {
		case "completed":
			o.Status = map[string]any{"code": statusOK}
		case "failed":
			o.Status = map[string]any{"code": statusError, "message": "failed (" + sp.OutcomeBasis + ")"}
		default:
			o.Status = map[string]any{"code": statusUnset}
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTimeUnixNano < out[j].StartTimeUnixNano })
	return map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []kv{
				sv("service.name", "agent-atlas"),
				sv("host.id", sess.HostID),
				sv("atlas.workspace", sess.Workspace),
			}},
			"scopeSpans": []any{map[string]any{
				"scope":     map[string]any{"name": ScopeName, "version": AtlasAttrsVersion},
				"schemaUrl": "https://opentelemetry.io/schemas/" + SemconvVersion,
				"spans":     out,
			}},
		}},
	}
}

// Exporter posts OTLP/JSON to a collector. It is constructed only when the
// user explicitly enabled export and acknowledged the preview.
type Exporter struct {
	Endpoint string
	Client   *http.Client
}

// Export sends one payload.
func (e *Exporter) Export(ctx context.Context, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := http.Client{Timeout: 10 * time.Second}
	if e.Client != nil {
		c = *e.Client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("otlp export: HTTP %d", resp.StatusCode)
	}
	return nil
}
