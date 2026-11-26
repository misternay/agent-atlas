// Package security implements the allowlist + redaction pipeline (design §10.3).
// Every event passes through SanitizeEvent before it is persisted, broadcast or
// spooled. Helpers here are also used by adapters while building labels so that
// raw values never leave the adapter.
package security

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"agentatlas/internal/event"
)

// Bounds for stored strings.
const (
	MaxLabelLen     = 160
	MaxIDLen        = 80
	MaxAttrValueLen = 120
	MaxResources    = 32
)

// AllowedAttrs is the allowlist of attribute keys that may be stored.
var AllowedAttrs = map[string]bool{
	"source":          true, // session start source (startup/resume/…)
	"reason":          true, // session end reason
	"permission_mode": true,
	"model":           true,
	"interrupted":     true,
	"hook_blocked":    true,
	"agent_type":      true,
	"trigger":         true,
	"gap_count":       true,
	"gap_reason":      true,
	"stale_after_s":   true,
	"exit_code":       true,
	"outcome_basis":   true, // how a status was established
	"start_missing":   true,
	"effort":          true,
	"input_keys":      true, // names (not values) of tool input fields
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{8,}`),                                             // Anthropic
	regexp.MustCompile(`(?i)\b(sk|pk|rk)-[A-Za-z0-9_\-]{12,}`),                                    // OpenAI/Stripe-like keys
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),                                            // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),                                          // GitHub fine-grained
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{16,}`),                                             // GitLab
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),                                         // Slack
	regexp.MustCompile(`\b(?:A3T[A-Z0-9]|AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[0-9A-Z]{16}\b`), // AWS key ids
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`),                                                   // HuggingFace
	regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{10,}`),                                  // Stripe
	regexp.MustCompile(`\bya29\.[A-Za-z0-9_\-]{10,}`),                                             // Google OAuth access token
	regexp.MustCompile(`\bSG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}`),                          // SendGrid
	regexp.MustCompile(`(?i)\bAccountKey=[^;\s]+`),                                                // Azure storage
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}`),                                                  // npm
	regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*\S+(?:\s+[A-Za-z0-9._~+/=\-]{6,})?`),         // whole Authorization value (scheme + credential)
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,}`),                                               // Google API key
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{4,}`),         // JWT
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=\-]{8,}`),
	regexp.MustCompile(`(?i)\bbasic\s+[A-Za-z0-9+/=]{12,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*:/{1,2})[^\s:/@]+:[^\s/@]+@`), // credentials in URLs (also after filepath.Clean turned :// into :/)
}

// key=value / --flag value / header style secrets.
var kvSecret = regexp.MustCompile(`(?i)(\b|--?)([A-Za-z0-9_\-]*(?:pass(?:word|wd)?|secret|token|api[_\-]?key|apikey|authorization|credential|private[_\-]?key|session[_\-]?key|access[_\-]?key|cookie)[A-Za-z0-9_\-]*)(\s*[=:]\s*|\s+)("[^"]*"|'[^']*'|[^\s&;|]+)`)

// Long high-entropy-looking blobs (>=40 chars of base64/hex-ish alphabet).
var longBlob = regexp.MustCompile(`[A-Za-z0-9+/_\-]{40,}={0,2}`)

var controlChars = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)

// RedactSecrets replaces anything that looks like a credential with [REDACTED].
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	for i, re := range secretPatterns {
		if i == len(secretPatterns)-1 {
			s = re.ReplaceAllString(s, "${1}[REDACTED]@")
			continue
		}
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	s = kvSecret.ReplaceAllString(s, "$1$2$3[REDACTED]")
	s = longBlob.ReplaceAllStringFunc(s, func(m string) string {
		// Paths are not secrets: keep things containing a slash with dots/segments.
		if strings.Count(m, "/") >= 2 {
			return m
		}
		return "[REDACTED]"
	})
	return s
}

// Bound truncates to max runes, appending an ellipsis marker.
func Bound(s string, max int) string {
	if max <= 1 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// CleanLabel normalizes whitespace, strips control chars, redacts secrets and bounds.
func CleanLabel(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	s = controlChars.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = RedactSecrets(s)
	return Bound(s, MaxLabelLen)
}

// CleanID strips whitespace and hashes oversized or credential-like IDs so
// they remain stable for correlation without persisting the sensitive value.
func CleanID(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = controlChars.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), "")
	if len(s) > MaxIDLen || (len(s) >= 12 && RedactSecrets(s) != s) {
		h := sha256.Sum256([]byte(s))
		return "id_" + hex.EncodeToString(h[:16])
	}
	return s
}

var homeDir = func() string {
	h, _ := os.UserHomeDir()
	return filepath.Clean(h)
}()

var userName = func() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return filepath.Base(homeDir)
}()

// SetHomeForTest overrides the home dir used for path collapsing.
func SetHomeForTest(h string) func() {
	old, oldU := homeDir, userName
	homeDir = h
	userName = filepath.Base(h)
	return func() { homeDir, userName = old, oldU }
}

// CollapseHome replaces the user's home directory with "~" anywhere in s,
// case-insensitively (macOS volumes are case-insensitive by default),
// including path-encoded variants such as "-Users-name-…" used by some tools.
func CollapseHome(p string) string {
	if homeDir == "" || homeDir == "/" || homeDir == "." {
		return p
	}
	res := homeRes()
	for _, re := range res {
		p = re.ReplaceAllString(p, "${1}~${3}")
	}
	return p
}

var homeReCache struct {
	sync.Mutex
	home string
	res  []*regexp.Regexp
}

// homeRes builds (once per home) the anchored patterns for the home path and
// its "-Users-name" encoded form. Matches must sit on path boundaries, so
// /Users/bobby, /Volumes/x/Users/bob or "my-Users-bob-notes" are left alone.
func homeRes() []*regexp.Regexp {
	homeReCache.Lock()
	defer homeReCache.Unlock()
	if homeReCache.home == homeDir && homeReCache.res != nil {
		return homeReCache.res
	}
	ci := ""
	if runtime.GOOS == "darwin" { // default APFS volumes are case-insensitive
		ci = "(?i)"
	}
	res := []*regexp.Regexp{
		regexp.MustCompile(ci + `(^|[^A-Za-z0-9._\-])(` + regexp.QuoteMeta(homeDir) + `)(/|$|[^A-Za-z0-9._\-])`),
	}
	if userName != "" && len(userName) >= 3 {
		enc := regexp.QuoteMeta(strings.ReplaceAll(homeDir, "/", "-"))
		res = append(res, regexp.MustCompile(ci+`(^|[^A-Za-z0-9._])(`+enc+`)(-|$|[^A-Za-z0-9._])`))
	}
	homeReCache.home, homeReCache.res = homeDir, res
	return res
}

// hasPathPrefix reports whether p is dir or inside dir, case-insensitively.
func hasPathPrefix(p, dir string) (rest string, ok bool) {
	if strings.EqualFold(p, dir) {
		return "", true
	}
	if len(p) > len(dir) && p[len(dir)] == '/' && strings.EqualFold(p[:len(dir)], dir) {
		return p[len(dir)+1:], true
	}
	return "", false
}

// Workspace returns a safe workspace label from a cwd: home collapsed, bounded.
func Workspace(cwd string) string {
	if cwd == "" {
		return ""
	}
	return CleanLabel(CollapseHome(filepath.Clean(cwd)))
}

// FileLabel returns a safe label for a file path: workspace-relative when the
// path is inside cwd, otherwise home-collapsed. Never returns raw home paths.
func FileLabel(p, cwd string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.Contains(p, "://") {
		return "" // URLs are not files (and may carry credentials)
	}
	// Relativize/collapse the raw path first; the resulting label is redacted
	// by CleanLabel (redacting first would break the workspace prefix match).
	if strings.HasPrefix(p, "~/") && homeDir != "" {
		p = filepath.Join(homeDir, p[2:])
	}
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	if cwd != "" {
		if rest, ok := hasPathPrefix(p, filepath.Clean(cwd)); ok {
			if rest == "" {
				return "."
			}
			return CleanLabel(rest)
		}
	}
	return CleanLabel(CollapseHome(p))
}

// URLHost returns only the host of a URL (no path, query or credentials).
func URLHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return CleanLabel(strings.ToLower(u.Hostname()))
}

// HashID returns a short stable hash for identifiers derived from sensitive input.
func HashID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SanitizeEvent is the final allowlist/redaction pass. It mutates and returns e.
func SanitizeEvent(e *event.Event) *event.Event {
	e.EventID = CleanID(e.EventID)
	e.HostID = CleanID(e.HostID)
	e.SessionID = CleanID(e.SessionID)
	e.TurnID = CleanID(e.TurnID)
	e.ToolCallID = CleanID(e.ToolCallID)
	e.AgentID = CleanID(e.AgentID)
	e.SourceEvent = Bound(CleanID(e.SourceEvent), 48)
	e.Workspace = CleanLabel(CollapseHome(e.Workspace))
	e.Privacy = event.PrivacyMetadataOnly
	e.Timestamp = e.Timestamp.UTC()
	e.Status = Bound(CleanID(e.Status), 16)
	if e.Tool != nil {
		e.Tool.Name = Bound(CleanLabel(e.Tool.Name), 96)
		e.Tool.Category = Bound(CleanLabel(e.Tool.Category), 24)
		e.Tool.Operation = Bound(CleanLabel(e.Tool.Operation), 48)
		e.Tool.MCPServer = Bound(CleanLabel(e.Tool.MCPServer), 64)
		e.Tool.MCPTool = Bound(CleanLabel(e.Tool.MCPTool), 96)
		if e.Tool.Name == "" {
			e.Tool = nil
		}
	}
	if e.Target != nil {
		e.Target.Kind = Bound(CleanLabel(e.Target.Kind), 24)
		e.Target.Label = CleanLabel(CollapseHome(e.Target.Label))
		if e.Target.Label == "" {
			e.Target = nil
		}
	}
	if len(e.Resources) > 0 {
		seen := map[string]bool{}
		out := make([]event.Resource, 0, len(e.Resources))
		for _, r := range e.Resources {
			r.Kind = Bound(CleanLabel(r.Kind), 24)
			r.Label = CleanLabel(CollapseHome(r.Label))
			r.Relation = Bound(CleanLabel(r.Relation), 24)
			if !event.ValidEvidence(r.Evidence) {
				r.Evidence = event.EvidenceUnknown
			}
			k := r.Kind + "|" + r.Label + "|" + r.Relation
			if r.Label == "" || r.Kind == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
			if len(out) >= MaxResources {
				break
			}
		}
		if len(out) == 0 {
			out = nil
		}
		e.Resources = out
	}
	if len(e.Attrs) > 0 {
		clean := map[string]string{}
		keys := make([]string, 0, len(e.Attrs))
		for k := range e.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !AllowedAttrs[k] {
				continue
			}
			v := Bound(CleanLabel(CollapseHome(e.Attrs[k])), MaxAttrValueLen)
			if v != "" {
				clean[k] = v
			}
		}
		if len(clean) == 0 {
			clean = nil
		}
		e.Attrs = clean
	}
	if !event.ValidEvidence(e.Evidence) {
		e.Evidence = event.EvidenceUnknown
	}
	return e
}
