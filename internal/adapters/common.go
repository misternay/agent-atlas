package adapters

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"agentatlas/internal/event"
	"agentatlas/internal/security"
)

// ToolInfo is the safe, classified view of a tool invocation.
type ToolInfo struct {
	Tool      event.Tool
	Target    *event.Target
	Resources []event.Resource
	InputKeys string
}

// AsObject decodes a tool_input value into a map when it is a JSON object.
func AsObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	// Some harnesses double-encode tool input as a JSON string.
	var s string
	if json.Unmarshal(raw, &s) == nil && strings.HasPrefix(strings.TrimSpace(s), "{") {
		if json.Unmarshal([]byte(s), &m) == nil {
			return m
		}
	}
	return nil
}

// AsString decodes a JSON string value.
func AsString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			case []any: // e.g. codex shell {"command": ["bash","-lc","..."]}
				parts := make([]string, 0, len(t))
				for _, p := range t {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				if len(parts) > 0 {
					return strings.Join(parts, " ")
				}
			}
		}
	}
	return ""
}

func keysOf(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		if len(k) <= 32 {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	if len(ks) > 8 {
		ks = ks[:8]
	}
	return strings.Join(ks, ",")
}

// ParseMCPName splits "mcp__server__tool" into server and tool.
func ParseMCPName(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	rest := name[len("mcp__"):]
	i := strings.Index(rest, "__")
	if i <= 0 {
		return rest, "", true
	}
	return rest[:i], rest[i+2:], true
}

// ClassifyTool builds a ToolInfo from a tool name and its raw input. Raw input
// values are only read in memory to derive bounded, sanitized labels.
func ClassifyTool(name string, rawInput json.RawMessage, cwd string) ToolInfo {
	in := AsObject(rawInput)
	ti := ToolInfo{Tool: event.Tool{Name: name, Category: event.CategoryOther, Operation: "invoke"}}
	if in != nil {
		ti.InputKeys = keysOf(in)
	}
	addFile := func(path, rel, evidence string) {
		if l := security.FileLabel(path, cwd); l != "" {
			ti.Resources = append(ti.Resources, event.Resource{Kind: event.ResourceFile, Label: l, Relation: rel, Evidence: evidence})
		}
	}
	setTarget := func(kind, label string) {
		if label != "" {
			ti.Target = &event.Target{Kind: kind, Label: label}
		}
	}

	if server, tool, ok := ParseMCPName(name); ok {
		ti.Tool.Category = event.CategoryMCP
		ti.Tool.Operation = "call"
		ti.Tool.MCPServer = server
		ti.Tool.MCPTool = tool
		label := server
		ti.Resources = append(ti.Resources, event.Resource{Kind: event.ResourceMCP, Label: label, Relation: event.RelConnectsTo, Evidence: event.EvidenceObserved})
		setTarget(event.ResourceMCP, server+" › "+tool)
		return ti
	}

	switch strings.ToLower(name) {
	case "bash", "shell", "exec_command", "local_shell", "unified_exec", "container.exec", "shell_command":
		ti.Tool.Category = event.CategoryShell
		ti.Tool.Operation = "execute"
		cmd := str(in, "command", "cmd", "script")
		if cmd == "" {
			cmd = AsString(rawInput)
		}
		label, res := AnalyzeCommand(cmd, cwd)
		setTarget(event.ResourceProcess, label)
		if label != "" {
			ti.Resources = append(ti.Resources, event.Resource{Kind: event.ResourceProcess, Label: label, Relation: event.RelInvokes, Evidence: event.EvidenceObserved})
		}
		ti.Resources = append(ti.Resources, res...)
	case "bashoutput", "write_stdin":
		ti.Tool.Category = event.CategoryShell
		ti.Tool.Operation = "read_output"
	case "killshell", "killbash":
		ti.Tool.Category = event.CategoryShell
		ti.Tool.Operation = "kill"
	case "read", "notebookread", "read_file", "view_image":
		ti.Tool.Category = event.CategoryFile
		ti.Tool.Operation = "read"
		p := str(in, "file_path", "notebook_path", "path")
		addFile(p, event.RelReads, event.EvidenceObserved)
		setTarget(event.ResourceFile, security.FileLabel(p, cwd))
	case "write", "write_file", "create_file":
		ti.Tool.Category = event.CategoryFile
		ti.Tool.Operation = "write"
		p := str(in, "file_path", "path")
		addFile(p, event.RelWrites, event.EvidenceObserved)
		setTarget(event.ResourceFile, security.FileLabel(p, cwd))
	case "edit", "multiedit", "notebookedit", "str_replace_based_edit_tool":
		ti.Tool.Category = event.CategoryFile
		ti.Tool.Operation = "edit"
		p := str(in, "file_path", "notebook_path", "path")
		addFile(p, event.RelWrites, event.EvidenceObserved)
		setTarget(event.ResourceFile, security.FileLabel(p, cwd))
	case "apply_patch":
		ti.Tool.Category = event.CategoryFile
		ti.Tool.Operation = "patch"
		patch := str(in, "input", "patch", "command")
		if patch == "" {
			patch = AsString(rawInput)
		}
		files := PatchFiles(patch)
		for _, f := range files {
			addFile(f, event.RelWrites, event.EvidenceObserved)
		}
		switch len(files) {
		case 0:
		case 1:
			setTarget(event.ResourceFile, security.FileLabel(files[0], cwd))
		default:
			setTarget(event.ResourceFile, security.FileLabel(files[0], cwd)+" (+"+itoaInt(len(files)-1)+" files)")
		}
	case "glob", "grep", "ls", "list_dir", "search_files", "toolsearch":
		ti.Tool.Category = event.CategorySearch
		ti.Tool.Operation = "search"
		if p := str(in, "path", "dir_path"); p != "" {
			addFile(p, event.RelReads, event.EvidenceObserved)
			setTarget(event.ResourceFile, security.FileLabel(p, cwd))
		} else if cwd != "" {
			setTarget(event.ResourceFile, ".")
		}
	case "webfetch", "web_fetch":
		ti.Tool.Category = event.CategoryNetwork
		ti.Tool.Operation = "fetch"
		if h := security.URLHost(str(in, "url")); h != "" {
			ti.Resources = append(ti.Resources, event.Resource{Kind: event.ResourceNetwork, Label: h, Relation: event.RelConnectsTo, Evidence: event.EvidenceObserved})
			setTarget(event.ResourceNetwork, h)
		}
	case "websearch", "web_search":
		ti.Tool.Category = event.CategoryNetwork
		ti.Tool.Operation = "search"
		setTarget(event.ResourceNetwork, "web search")
	case "task", "agent", "spawn_agent":
		ti.Tool.Category = event.CategoryAgent
		ti.Tool.Operation = "delegate"
		setTarget("agent", security.CleanLabel(str(in, "subagent_type", "agent_type")))
	case "todowrite", "update_plan", "exitplanmode", "enterplanmode":
		ti.Tool.Category = event.CategoryOther
		ti.Tool.Operation = "plan"
	case "skill":
		ti.Tool.Category = event.CategoryOther
		ti.Tool.Operation = "skill"
		setTarget("skill", security.CleanLabel(str(in, "skill")))
	}
	return ti
}

var patchHeader = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$|^\*\*\* Move to: (.+)$`)

// PatchFiles extracts file paths from an apply_patch body.
func PatchFiles(patch string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range patchHeader.FindAllStringSubmatch(patch, 64) {
		f := strings.TrimSpace(m[1])
		if f == "" {
			f = strings.TrimSpace(m[2])
		}
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
var subcommandRe = regexp.MustCompile(`^[a-z][a-z0-9:_\-]{0,23}$`)

// wrappers are skipped when finding the program name.
var wrappers = map[string]bool{"sudo": true, "env": true, "time": true, "nice": true, "nohup": true, "rtk": true, "command": true, "exec": true, "xargs": true}

// programs whose second token is a meaningful subcommand.
var subcommandPrograms = map[string]bool{
	"npm": true, "pnpm": true, "yarn": true, "bun": true, "npx": true, "go": true, "git": true, "cargo": true,
	"docker": true, "kubectl": true, "make": true, "pip": true, "uv": true, "poetry": true, "swift": true,
	"xcodebuild": true, "gh": true, "brew": true, "terraform": true, "helm": true, "python": false,
	"pytest": false, "gradle": true, "mvn": true, "dotnet": true, "deno": true, "composer": true, "bundle": true,
	"rails": true, "mix": true, "flutter": true, "dart": true, "claude": true, "codex": true, "atlas": true,
}

// readers take file operands that are plausibly read.
var readers = map[string]bool{"cat": true, "head": true, "tail": true, "less": true, "more": true, "wc": true, "bat": true, "nl": true, "od": true, "xxd": true, "file": true, "stat": true, "diff": true, "md5": true, "shasum": true, "sha256sum": true}

// AnalyzeCommand derives a safe process label (program + optional subcommand)
// and heuristic, explicitly *inferred* file resources from a shell command.
// It never returns arguments other than plausible file operands.
func AnalyzeCommand(cmd, cwd string) (label string, resources []event.Resource) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", nil
	}
	// Strip common "shell -lc '<script>'" wrappers (codex style).
	for _, sh := range []string{"bash -lc ", "/bin/bash -lc ", "zsh -lc ", "/bin/zsh -lc ", "sh -c ", "/bin/sh -c ", "bash -c ", "zsh -c "} {
		if strings.HasPrefix(cmd, sh) {
			cmd = unquote(strings.TrimSpace(cmd[len(sh):]))
			break
		}
	}
	segments := splitSegments(stripHeredocs(cmd))
	multi := len(segments) > 1
	seen := map[string]bool{}
	add := func(p, rel string) {
		if !looksLikePath(p) || strings.HasPrefix(p, "/dev/") {
			return
		}
		l := security.FileLabel(p, cwd)
		k := rel + l
		if l == "" || seen[k] || len(resources) >= 8 {
			return
		}
		seen[k] = true
		resources = append(resources, event.Resource{Kind: event.ResourceFile, Label: l, Relation: rel, Evidence: event.EvidenceInferred})
	}
	for i, seg := range segments {
		toks := tokenize(seg)
		// redirections
		for j := 0; j < len(toks); j++ {
			t := toks[j]
			switch {
			case t == ">" || t == ">>" || t == "1>" || t == "2>" || t == "&>":
				if j+1 < len(toks) && !strings.HasPrefix(t, "2") {
					add(toks[j+1], event.RelWrites)
				}
				j++
			case strings.HasPrefix(t, ">>") && len(t) > 2:
				add(t[2:], event.RelWrites)
			case strings.HasPrefix(t, ">") && len(t) > 1 && t[1] != '&':
				add(t[1:], event.RelWrites)
			case t == "<<" || t == "<<-" || t == "<<<":
				j++ // heredoc delimiter / here-string word: not a file
			case strings.HasPrefix(t, "<<"):
			case t == "<":
				if j+1 < len(toks) {
					add(toks[j+1], event.RelReads)
				}
				j++
			}
		}
		k := 0
		for k < len(toks) && (envAssign.MatchString(toks[k]) || wrappers[filepath.Base(toks[k])]) {
			k++
		}
		if k >= len(toks) {
			continue
		}
		prog := filepath.Base(toks[k])
		if i == 0 {
			label = prog
			if subcommandPrograms[prog] && k+1 < len(toks) && subcommandRe.MatchString(toks[k+1]) {
				label = prog + " " + toks[k+1]
				if (prog == "go" || prog == "npm" || prog == "pnpm" || prog == "yarn") && toks[k+1] == "run" && k+2 < len(toks) && subcommandRe.MatchString(toks[k+2]) {
					label += " " + toks[k+2]
				}
			}
		}
		var args []string
		for j := k + 1; j < len(toks); j++ {
			t := toks[j]
			if t == ">" || t == ">>" || t == "1>" || t == "2>" || t == "&>" || t == "<" || t == "<<" || t == "<<-" || t == "<<<" {
				j++
				continue
			}
			if strings.HasPrefix(t, ">") || strings.HasPrefix(t, "2>") || strings.HasPrefix(t, "1>") || strings.HasPrefix(t, "&>") || strings.HasPrefix(t, "<<") || t == "" {
				continue
			}
			args = append(args, t)
		}
		switch {
		case readers[prog]:
			for _, a := range positionals(args, valueFlags[prog]) {
				if strongPath(a) {
					add(a, event.RelReads)
				}
			}
		case prog == "sed" || prog == "awk" || prog == "grep" || prog == "rg" || prog == "egrep" || prog == "fgrep":
			// The first positional is the pattern/script unless it was given via
			// -e/-f (or --regexp/--file); only later positionals can be files.
			pos := positionals(args, valueFlags[prog])
			if !hasPatternFlag(args) {
				if len(pos) == 0 {
					break
				}
				pos = pos[1:]
			}
			for _, a := range pos {
				if strongPath(a) {
					add(a, event.RelReads)
				}
			}
		case prog == "tee" || prog == "touch" || prog == "mkdir":
			for _, a := range positionals(args, nil) {
				if looksLikePath(a) {
					add(a, event.RelWrites)
				}
			}
		}
	}
	if label != "" {
		label = security.CleanLabel(label)
		if multi {
			label += " …"
		}
	}
	return label, resources
}

// pathish allows only characters typical of file paths: no spaces, quotes,
// '@', '=', ':', '$', globs or brackets. Arbitrary argument values (search
// terms, emails, JSON filters, URLs) therefore never become file labels.
var pathish = regexp.MustCompile(`^[A-Za-z0-9_./~+-]+$`)

func looksLikePath(a string) bool {
	if a == "" || len(a) > 256 || strings.HasPrefix(a, "-") || a == "." || a == ".." {
		return false
	}
	return pathish.MatchString(a)
}

var extRe = regexp.MustCompile(`\.[A-Za-z0-9]{1,10}$`)

// strongPath requires a '/' or a file extension in addition to looksLikePath,
// so misaligned flag values or search terms are not mistaken for files.
func strongPath(a string) bool {
	return looksLikePath(a) && (strings.Contains(a, "/") || extRe.MatchString(a))
}

func init() {
	extra := map[string][]string{
		"grep":   {"-d", "-D", "--exclude-dir", "--color", "--colour", "--label"},
		"rg":     {"-M", "-j", "-E", "--iglob", "--type-not", "--type-add", "--max-columns", "--color", "--colors", "--sort", "--sortr", "--threads", "--max-depth", "--encoding"},
		"shasum": {"-a", "--algorithm"},
		"nl":     {"-b", "-v", "-i", "-w", "-s"},
		"stat":   {"-f", "-c", "--format", "-t"},
		"sed":    {"-l"},
	}
	for prog, flags := range extra {
		if valueFlags[prog] == nil {
			valueFlags[prog] = map[string]bool{}
		}
		for _, f := range flags {
			valueFlags[prog][f] = true
		}
	}
}

// valueFlags lists flags (per program) whose following token is a value, not an operand.
var valueFlags = map[string]map[string]bool{
	"head":  {"-n": true, "-c": true, "--lines": true, "--bytes": true},
	"tail":  {"-n": true, "-c": true, "--lines": true, "--bytes": true},
	"grep":  {"-e": true, "-f": true, "-m": true, "-A": true, "-B": true, "-C": true, "--include": true, "--exclude": true, "--regexp": true, "--file": true, "--max-count": true},
	"egrep": {"-e": true, "-f": true, "-m": true, "-A": true, "-B": true, "-C": true},
	"fgrep": {"-e": true, "-f": true, "-m": true, "-A": true, "-B": true, "-C": true},
	"rg":    {"-e": true, "-f": true, "-g": true, "-t": true, "-T": true, "-m": true, "-A": true, "-B": true, "-C": true, "--glob": true, "--type": true, "--regexp": true, "--file": true, "--max-count": true},
	"sed":   {"-e": true, "-f": true, "--expression": true, "--file": true},
	"awk":   {"-f": true, "-v": true, "-F": true},
	"diff":  {"-U": true, "-C": true, "--label": true},
	"od":    {"-A": true, "-t": true, "-N": true, "-j": true},
	"xxd":   {"-l": true, "-s": true, "-c": true, "-g": true},
}

// positionals drops flags and the values of flags that take one.
func positionals(args []string, vf map[string]bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			if vf[a] {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasPatternFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-e" || a == "-f" || a == "--regexp" || a == "--file" || a == "--expression":
			return true
		case strings.HasPrefix(a, "--regexp=") || strings.HasPrefix(a, "--file=") || strings.HasPrefix(a, "--expression="):
			return true
		}
	}
	return false
}

var heredocWord = regexp.MustCompile(`^[ \t]*('[-A-Za-z0-9_]+'|"[-A-Za-z0-9_]+"|[-A-Za-z0-9_]+)(?:[ \t;|&<>)]|$)`)

// stripHeredocs removes heredoc bodies so their content is never parsed as commands.
func stripHeredocs(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	var out []string
	type delimiter struct {
		word string
		tabs bool
	}
	var pending []delimiter
	var quote byte
	for _, l := range lines {
		if len(pending) > 0 {
			if pending[0].tabs {
				l = strings.TrimLeft(l, "\t")
			}
			if l == pending[0].word {
				pending = pending[1:]
			}
			continue
		}
		if strings.Contains(l, "$(") || strings.ContainsRune(l, '`') {
			return strings.Join(out, "\n") // nested shell grammar is ambiguous here
		}
		out = append(out, l)
		for i := 0; i < len(l); i++ {
			ch := l[i]
			switch {
			case ch == '\\' && quote != '\'':
				i++
			case ch == quote:
				quote = 0
			case quote != 0:
			case ch == '\'' || ch == '"':
				quote = ch
			case strings.HasPrefix(l[i:], "<<<"):
				i += 2
			case strings.HasPrefix(l[i:], "<<"):
				i += 2
				tabs := i < len(l) && l[i] == '-'
				if tabs {
					i++
				}
				m := heredocWord.FindStringSubmatchIndex(l[i:])
				if m == nil {
					return strings.Join(out, "\n") // do not inspect a possible body
				}
				pending = append(pending, delimiter{word: unquote(l[i+m[2] : i+m[3]]), tabs: tabs})
				i += m[3] - 1
			}
		}
		if len(pending) > 0 && (quote != 0 || strings.HasSuffix(l, `\`)) {
			return strings.Join(out, "\n")
		}
	}
	return strings.Join(out, "\n")
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// splitSegments splits on unquoted ;, &&, ||, | and newlines.
func splitSegments(s string) []string {
	var segs []string
	var cur strings.Builder
	var q rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if q != 0 {
			cur.WriteRune(c)
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			q = c
			cur.WriteRune(c)
		case ';', '\n', '|', '&':
			if c == '&' && (i+1 >= len(rs) || rs[i+1] != '&') {
				// lone & (background) or >& redirection: keep as part of token
				if i > 0 && rs[i-1] == '>' {
					cur.WriteRune(c)
					continue
				}
			}
			if strings.TrimSpace(cur.String()) != "" {
				segs = append(segs, strings.TrimSpace(cur.String()))
			}
			cur.Reset()
			if i+1 < len(rs) && (rs[i+1] == '&' || rs[i+1] == '|') && (c == '&' || c == '|') {
				i++
			}
		default:
			cur.WriteRune(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		segs = append(segs, strings.TrimSpace(cur.String()))
	}
	if len(segs) > 16 {
		segs = segs[:16]
	}
	return segs
}

// tokenize splits a command segment into words honoring simple quotes.
func tokenize(s string) []string {
	var toks []string
	var cur strings.Builder
	var q rune
	has := false
	for _, c := range s {
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			q = c
			has = true
		case c == ' ' || c == '\t':
			if has || cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(c)
		}
		if len(toks) > 64 {
			break
		}
	}
	if has || cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

// Base builds an event skeleton with shared fields.
func Base(meta Meta, harness, sourceEvent, sessionID, cwd string, idx int) event.Event {
	id := "evt_" + meta.EnvelopeID
	if idx > 0 {
		id += "-" + itoaInt(idx)
	}
	return event.Event{
		SchemaVersion: event.SchemaVersion,
		EventID:       id,
		Timestamp:     meta.ReceivedAt.UTC(),
		HostID:        meta.HostID,
		Harness:       harness,
		SessionID:     sessionID,
		SourceEvent:   sourceEvent,
		Workspace:     security.Workspace(cwd),
		Evidence:      event.EvidenceObserved,
		Privacy:       event.PrivacyMetadataOnly,
	}
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// ApplyToolInfo copies classified tool info onto an event.
func ApplyToolInfo(e *event.Event, ti ToolInfo) {
	t := ti.Tool
	e.Tool = &t
	e.Target = ti.Target
	e.Resources = ti.Resources
	if ti.InputKeys != "" {
		if e.Attrs == nil {
			e.Attrs = map[string]string{}
		}
		e.Attrs["input_keys"] = ti.InputKeys
	}
}

// DurationPtr returns a pointer for a positive duration.
func DurationPtr(ms float64) *int64 {
	if ms <= 0 {
		return nil
	}
	v := int64(ms)
	return &v
}
