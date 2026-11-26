// Command atlas is the Agent Atlas CLI and hook shim.
//
//	atlas hook --harness claude|codex   (invoked by harness hooks; reads stdin)
//	atlas install-hooks / uninstall-hooks / doctor / start / stop / status / open
//	atlas pause / resume / clear-history / capabilities / launchd install|uninstall
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentatlas/internal/config"
	"agentatlas/internal/install"
	"agentatlas/internal/normalize"
	"agentatlas/internal/shim"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "hook" {
		runHook(args) // never returns non-zero
		return
	}
	var err error
	switch cmd {
	case "install-hooks":
		err = cmdInstall(args, false)
	case "uninstall-hooks":
		err = cmdInstall(args, true)
	case "doctor":
		err = cmdDoctor(args)
	case "start":
		err = cmdStart(args)
	case "stop":
		err = cmdStop()
	case "status":
		err = cmdStatus()
	case "open":
		err = cmdOpen(args)
	case "pause":
		err = cmdPause(true)
	case "resume":
		err = cmdPause(false)
	case "clear-history":
		err = cmdClear(args)
	case "capabilities":
		err = cmdCapabilities()
	case "launchd":
		err = cmdLaunchd(args)
	case "version", "--version", "-v":
		fmt.Println("atlas", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "atlas:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Agent Atlas — local observability for AI coding agents

Usage: atlas <command> [flags]

  install-hooks    Merge Atlas hooks into Claude Code / Codex config (backs up first)
  uninstall-hooks  Remove Atlas hooks (restores the backup if the file is unchanged)
  doctor           Check daemon, socket, hooks, versions and observed signals
  start            Start atlasd in the background (use --foreground to run inline)
  stop             Stop atlasd
  status           Print daemon health
  open             Open the dashboard in your browser (signs in with the local token)
  pause | resume   Pause or resume collection
  clear-history    Delete all stored history (requires --yes)
  capabilities     Print the per-harness capability matrix
  launchd install|uninstall   Manage the macOS LaunchAgent for atlasd
  hook --harness H Hook entry point (called by the harness; reads JSON on stdin)

Environment: ATLAS_HOME overrides the data directory (default ~/Library/Application Support/AgentAtlas).
`)
}

// runHook must never fail the agent: no stdout, exit 0 on every path.
func runHook(args []string) {
	defer func() { _ = recover(); os.Exit(0) }()
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	harness := fs.String("harness", "", "")
	_ = fs.Parse(args)
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover(); close(done) }()
		shim.Run(*harness, os.Stdin, config.Home())
	}()
	select {
	case <-done:
	case <-time.After(2500 * time.Millisecond): // hard ceiling
	}
	os.Exit(0)
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "atlas"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func atlasdPath() string {
	cand := filepath.Join(filepath.Dir(selfPath()), "atlasd")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	if p, err := exec.LookPath("atlasd"); err == nil {
		return p
	}
	return cand
}

func cmdInstall(args []string, uninstall bool) error {
	name := "install-hooks"
	if uninstall {
		name = "uninstall-hooks"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	harness := fs.String("harness", "claude,codex", "comma-separated harnesses")
	claudePath := fs.String("claude-settings", "", "Claude Code settings.json (default ~/.claude/settings.json)")
	codexPath := fs.String("codex-hooks", "", "Codex hooks.json (default ~/.codex/hooks.json)")
	bin := fs.String("atlas-bin", selfPath(), "atlas binary path to call from hooks")
	_ = fs.Parse(args)
	targets := install.DefaultTargets()
	for i := range targets {
		if targets[i].Harness == "claude" && *claudePath != "" {
			targets[i].Path = *claudePath
		}
		if targets[i].Harness == "codex" && *codexPath != "" {
			targets[i].Path = *codexPath
		}
	}
	want := map[string]bool{}
	for _, h := range strings.Split(*harness, ",") {
		h = strings.TrimSpace(h)
		if h != "claude" && h != "codex" {
			return fmt.Errorf("unknown harness %q (expected claude or codex)", h)
		}
		want[h] = true
	}
	statePath := filepath.Join(config.Home(), "install-state.json")
	if err := config.EnsureHome(config.Home()); err != nil {
		return err
	}
	var failures []error
	for _, t := range targets {
		if !want[t.Harness] {
			continue
		}
		var res install.Result
		var err error
		if uninstall {
			res, err = install.Uninstall(t, *bin, statePath)
		} else {
			res, err = install.Install(t, *bin, statePath, time.Now())
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s %s: %w", t.Harness, t.Path, err))
			continue
		}
		fmt.Printf("%-6s %s: %s", t.Harness, t.Path, res.Action)
		if res.Backup != "" {
			fmt.Printf(" (backup: %s)", res.Backup)
		}
		if len(res.Events) > 0 {
			fmt.Printf("\n       events: %s", strings.Join(res.Events, ", "))
		}
		fmt.Println()
		for _, n := range res.Notes {
			fmt.Println("       note:", n)
		}
	}
	return errors.Join(failures...)
}

// --- daemon control ---

func paths() config.Paths { return config.PathsFor(config.Home()) }

func dashboardURL() (string, error) {
	var address struct{ URL string }
	err := apiCall(http.MethodGet, "/control/address", nil, &address)
	return address.URL, err
}

func token() string {
	b, err := os.ReadFile(paths().Token)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func apiCall(method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	socket := paths().Control
	if err := config.CheckPrivateSocket(socket); err != nil {
		return err
	}
	req, err := http.NewRequest(method, "http://atlas.local"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token())
	if method != http.MethodGet {
		req.Header.Set("X-Atlas-CSRF", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func healthy() bool {
	var h map[string]any
	return apiCall(http.MethodGet, "/api/v1/health", nil, &h) == nil && h["ok"] == true && h["version"] != nil
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	fg := fs.Bool("foreground", false, "run in the foreground")
	_ = fs.Parse(args)
	if healthy() {
		u, _ := dashboardURL()
		fmt.Println("atlasd already running at", u)
		return nil
	}
	p := paths()
	if err := config.EnsureHome(p.Home); err != nil {
		return err
	}
	bin := atlasdPath()
	if *fg {
		c := exec.Command(bin, "--home", p.Home)
		c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
		return c.Run()
	}
	c := exec.Command(bin, "--home", p.Home, "--log", p.Log)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return fmt.Errorf("start %s: %w", bin, err)
	}
	_ = c.Process.Release()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if healthy() {
			u, _ := dashboardURL()
			fmt.Println("atlasd started at", u)
			return nil
		}
	}
	return fmt.Errorf("atlasd did not become healthy; see %s", p.Log)
}

func cmdStop() error {
	if err := apiCall(http.MethodPost, "/control/stop", nil, nil); err != nil {
		return fmt.Errorf("request atlasd shutdown: %w", err)
	}
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if lock, err := config.TryDaemonLock(paths().Home); err == nil {
			lock.Close()
			fmt.Println("atlasd stopped")
			return nil
		}
	}
	return errors.New("atlasd did not stop within 5s")
}

func cmdStatus() error {
	var h map[string]any
	if err := apiCall(http.MethodGet, "/api/v1/health", nil, &h); err != nil {
		return fmt.Errorf("atlasd not reachable: %w", err)
	}
	if h["version"] == nil {
		return errors.New("atlasd rejected the local API token")
	}
	b, _ := json.MarshalIndent(h, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	printOnly := fs.Bool("print", false, "print the sign-in URL instead of opening a browser")
	_ = fs.Parse(args)
	if !healthy() {
		return errors.New("atlasd is not running; run `atlas start` first")
	}
	var code struct {
		Code string `json:"code"`
	}
	if err := apiCall(http.MethodPost, "/api/v1/admin/signin-code", map[string]string{}, &code); err != nil {
		return err
	}
	base, err := dashboardURL()
	if err != nil {
		return err
	}
	u := base + "/#signin=" + code.Code // single-use, expires in 60s
	if *printOnly {
		fmt.Println(u)
		return nil
	}
	return exec.Command("open", u).Run()
}

func cmdPause(p bool) error {
	var out map[string]any
	if err := apiCall(http.MethodPost, "/api/v1/admin/pause", map[string]bool{"paused": p}, &out); err != nil {
		return err
	}
	if p {
		fmt.Println("collection paused (hooks will drop events until `atlas resume`)")
	} else {
		fmt.Println("collection resumed")
	}
	return nil
}

func cmdClear(args []string) error {
	fs := flag.NewFlagSet("clear-history", flag.ExitOnError)
	yes := fs.Bool("yes", false, "confirm permanent deletion of all stored history")
	_ = fs.Parse(args)
	if !*yes {
		return errors.New("this permanently deletes all stored sessions and events; re-run with --yes")
	}
	if err := apiCall(http.MethodPost, "/api/v1/admin/clear-history", map[string]string{"confirm": "delete all history"}, nil); err != nil {
		return err
	}
	fmt.Println("history cleared")
	return nil
}

func cmdCapabilities() error {
	for _, a := range normalize.Adapters() {
		c := a.Capabilities()
		fmt.Printf("%s (payload basis: %s)\n", a.Name(), c.PayloadBasis)
		fmt.Printf("  session=%v turn=%v tool_start=%v tool_finish=%v tool_failure=%v denied=%v subagents=%v mcp=%v\n",
			c.SessionLifecycle, c.TurnLifecycle, c.ToolStart, c.ToolFinish, c.ToolFailure, c.ToolDenied, c.Subagents, c.MCPCalls)
		fmt.Printf("  hooks: %s\n", strings.Join(c.HookEvents, ", "))
		for _, n := range c.Notes {
			fmt.Println("  -", n)
		}
	}
	return nil
}

func cmdLaunchd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: atlas launchd install|uninstall [--dir DIR] [--no-load]")
	}
	fs := flag.NewFlagSet("launchd", flag.ExitOnError)
	dir := fs.String("dir", install.DefaultLaunchAgentsDir(), "LaunchAgents directory")
	noLoad := fs.Bool("no-load", false, "write/remove the plist without calling launchctl")
	_ = fs.Parse(args[1:])
	p := paths()
	switch args[0] {
	case "install":
		if err := config.EnsureHome(p.Home); err != nil {
			return err
		}
		path, err := install.LaunchdInstall(*dir, atlasdPath(), p.Home, p.Log, !*noLoad)
		if err != nil {
			return err
		}
		fmt.Println("LaunchAgent written:", path)
	case "uninstall":
		path, err := install.LaunchdUninstall(*dir, !*noLoad)
		if err != nil {
			return err
		}
		fmt.Println("LaunchAgent removed:", path)
	default:
		return fmt.Errorf("unknown launchd action %q", args[0])
	}
	return nil
}
