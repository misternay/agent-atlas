package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const sessionProtocol = "atlas"
const sessionPrefix = "atlas-session."

// LoadOrCreateToken reads the local API token, creating a random one (0600)
// on first use.
func LoadOrCreateToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		t := strings.TrimSpace(string(b))
		if len(t) >= 32 {
			_ = os.Chmod(path, 0o600)
			return t, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw[:])
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// guard enforces Host/Origin validation (DNS-rebinding and cross-site
// protection), token auth, and CSRF rules for state-changing requests.
type guard struct {
	token    string
	port     string
	mu       sync.Mutex
	codes    map[string]time.Time
	sessions map[string]time.Time // browser session secrets (never the API token)
}

const sessionTTL = 7 * 24 * time.Hour

func (g *guard) newSession() string {
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	sec := hex.EncodeToString(raw[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions == nil {
		g.sessions = map[string]time.Time{}
	}
	now := time.Now()
	for k, exp := range g.sessions {
		if now.After(exp) {
			delete(g.sessions, k)
		}
	}
	g.sessions[sec] = now.Add(sessionTTL)
	return sec
}

func (g *guard) validSession(sec string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, ok := g.sessions[sec]
	return ok && time.Now().Before(exp)
}

func (g *guard) hostAllowed(host string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if p != g.port {
		return false
	}
	return strings.EqualFold(h, "localhost") || net.ParseIP(h).IsLoopback()
}

func (g *guard) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // same-origin navigations and non-browser clients
	}
	for _, scheme := range []string{"http://", "https://"} {
		if strings.HasPrefix(o, scheme) {
			return g.hostAllowed(strings.TrimPrefix(o, scheme)) && strings.EqualFold(strings.TrimPrefix(o, scheme), r.Host)
		}
	}
	return false
}

func (g *guard) authenticated(r *http.Request) bool {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(a, "Bearer ")), []byte(g.token)) == 1 {
			return true
		}
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "AtlasSession ") {
		return g.validSession(strings.TrimPrefix(a, "AtlasSession "))
	}
	// Browsers cannot set Authorization on WebSocket upgrades. Require their
	// origin and read the session from an offered protocol without echoing it.
	if r.URL.Path == "/api/v1/stream" && r.Header.Get("Origin") != "" {
		for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
			for _, value := range strings.Split(line, ",") {
				if sec, ok := strings.CutPrefix(strings.TrimSpace(value), sessionPrefix); ok && g.validSession(sec) {
					return true
				}
			}
		}
	}
	return false
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("Cache-Control", "no-store")
}

// wrap applies the guard. requireAuth=false is used only for static UI assets
// (which contain no telemetry) and the public liveness probe.
func (g *guard) wrap(requireAuth bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		if !g.hostAllowed(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if !g.originAllowed(r) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodOptions {
			// No CORS: preflights are always refused.
			http.Error(w, "cors not allowed", http.StatusForbidden)
			return
		}
		if requireAuth && !g.authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "hint": "run `atlas open` to sign in to the local dashboard"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// CSRF: state-changing requests need a custom header (not sendable
			// cross-site without a CORS preflight, which is refused) and, when a
			// browser sends Origin, it must be this origin (checked above).
			if r.Header.Get("X-Atlas-CSRF") != "1" {
				http.Error(w, "missing csrf header", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// issueCode returns a single-use sign-in code valid for 60 seconds, so the
// long-lived token never appears in a URL or browser history.
func (g *guard) issueCode() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	c := hex.EncodeToString(raw[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.codes == nil {
		g.codes = map[string]time.Time{}
	}
	now := time.Now()
	for k, exp := range g.codes {
		if now.After(exp) {
			delete(g.codes, k)
		}
	}
	g.codes[c] = now.Add(60 * time.Second)
	return c
}

func (g *guard) redeemCode(c string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, ok := g.codes[c]
	if !ok {
		return false
	}
	delete(g.codes, c) // single use
	return time.Now().Before(exp)
}

func (g *guard) authRedirect(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	g.mu.Lock()
	valid := time.Now().Before(g.codes[code])
	g.mu.Unlock()
	if !valid {
		http.Error(w, "invalid or expired sign-in code; run `atlas open` again", http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/#signin="+code, http.StatusFound)
}

// authExchange redeems the one-time code on the dashboard's origin.
func (g *guard) authExchange(w http.ResponseWriter, r *http.Request) {
	var body struct{ Code string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil || !g.redeemCode(body.Code) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired sign-in code"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session": g.newSession()})
}
