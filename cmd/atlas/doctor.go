package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentatlas/internal/config"
	"agentatlas/internal/install"
	"agentatlas/internal/spool"
)

type check struct {
	ok     bool
	warn   bool
	name   string
	detail string
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	claudePath := fs.String("claude-settings", "", "Claude Code settings.json to inspect")
	codexPath := fs.String("codex-hooks", "", "Codex hooks.json to inspect")
	_ = fs.Parse(args)
	p := paths()
	var checks []check
	add := func(c check) { checks = append(checks, c) }

	if fi, err := os.Stat(p.Home); err != nil {
		add(check{name: "data directory", detail: p.Home + " missing (created by `atlas start`)", warn: true})
	} else {
		add(check{ok: fi.Mode().Perm() == 0o700, name: "data directory", detail: fmt.Sprintf("%s (%v)", p.Home, fi.Mode().Perm())})
	}
	if err := config.CheckPrivateSocket(p.Socket); os.IsNotExist(err) {
		add(check{name: "ingest socket", detail: "not present: hooks will spool locally until atlasd runs", warn: true})
	} else if err != nil {
		add(check{name: "ingest socket", detail: err.Error()})
	} else {
		c, derr := net.DialTimeout("unix", p.Socket, 200*time.Millisecond)
		if derr == nil {
			c.Close()
		}
		add(check{ok: derr == nil, name: "ingest socket", detail: fmt.Sprintf("%s (0600, reachable=%v)", p.Socket, derr == nil)})
	}
	var health map[string]any
	herr := apiCall(http.MethodGet, "/api/v1/health", nil, &health)
	if herr != nil {
		add(check{name: "daemon", detail: "not reachable (run `atlas start`): " + herr.Error()})
	} else {
		u, _ := dashboardURL()
		ing, _ := health["ingest"].(map[string]any)
		add(check{ok: health["version"] != nil, name: "daemon", detail: fmt.Sprintf("%s version=%v paused=%v committed=%v rejected=%v dropped=%v", u, health["version"], health["paused"], ing["events_committed"], ing["rejected"], ing["dropped"])})
	}
	if n := spool.Size(p.Spool); n > 0 {
		add(check{warn: true, name: "offline spool", detail: fmt.Sprintf("%d bytes pending (ingested automatically when atlasd runs)", n)})
	} else {
		add(check{ok: true, name: "offline spool", detail: "empty"})
	}
	for _, bin := range []string{"claude", "codex"} {
		out, err := exec.Command(bin, "--version").Output()
		if err != nil {
			add(check{warn: true, name: bin + " version", detail: "not found on PATH"})
			continue
		}
		add(check{ok: true, name: bin + " version", detail: strings.TrimSpace(string(out))})
	}
	targets := install.DefaultTargets()
	for i := range targets {
		if targets[i].Harness == "claude" && *claudePath != "" {
			targets[i].Path = *claudePath
		}
		if targets[i].Harness == "codex" && *codexPath != "" {
			targets[i].Path = *codexPath
		}
		t := targets[i]
		evs, err := install.Installed(t, selfPath(), filepath.Join(p.Home, "install-state.json"))
		switch {
		case err != nil:
			add(check{warn: true, name: t.Harness + " hooks", detail: t.Path + ": " + err.Error()})
		case len(evs) == 0:
			add(check{warn: true, name: t.Harness + " hooks", detail: t.Path + ": Atlas hooks not installed (`atlas install-hooks --harness " + t.Harness + "`)"})
		default:
			d := t.Path + ": " + strings.Join(evs, ", ")
			if t.Harness == "codex" {
				d += " — Codex runs hooks only after you trust them in Codex"
			}
			add(check{ok: true, name: t.Harness + " hooks", detail: d})
		}
	}
	if herr == nil {
		var caps struct {
			Harnesses []struct {
				Harness        string           `json:"harness"`
				Observed       map[string]int64 `json:"observed_hooks"`
				NotYetObserved []string         `json:"not_yet_observed"`
			} `json:"harnesses"`
		}
		if apiCall(http.MethodGet, "/api/v1/capabilities", nil, &caps) == nil {
			for _, h := range caps.Harnesses {
				if len(h.Observed) == 0 {
					add(check{warn: true, name: h.Harness + " observed", detail: "no events received yet"})
					continue
				}
				var latest int64
				var observed []string
				for name, ts := range h.Observed {
					observed = append(observed, name)
					if ts > latest {
						latest = ts
					}
				}
				sort.Strings(observed)
				detail := fmt.Sprintf("%s (last %s)", strings.Join(observed, ", "), time.UnixMicro(latest).Format(time.RFC3339))
				if len(h.NotYetObserved) > 0 {
					detail += "; supported but not yet seen: " + strings.Join(h.NotYetObserved, ", ")
				}
				add(check{ok: true, name: h.Harness + " observed", detail: detail})
			}
		}
	}
	failed := 0
	for _, c := range checks {
		mark := "✔"
		switch {
		case c.ok:
		case c.warn:
			mark = "!"
		default:
			mark = "✘"
			failed++
		}
		fmt.Printf(" %s %-16s %s\n", mark, c.name, c.detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}
