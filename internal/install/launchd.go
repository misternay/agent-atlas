package install

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// LaunchdLabel is the LaunchAgent label for atlasd.
const LaunchdLabel = "com.agentatlas.atlasd"

// DefaultLaunchAgentsDir returns ~/Library/LaunchAgents.
func DefaultLaunchAgentsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Plist renders the LaunchAgent property list.
func Plist(atlasdBin, home, logPath string) []byte {
	args := []string{atlasdBin, "--home", home, "--log", logPath}
	var a bytes.Buffer
	for _, s := range args {
		a.WriteString("\t\t<string>" + xmlEscape(s) + "</string>\n")
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + LaunchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + a.String() + `	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>LowPriorityIO</key>
	<true/>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`)
}

// LaunchdInstall writes the plist and (unless load=false) bootstraps it.
func LaunchdInstall(dir, atlasdBin, home, logPath string, load bool) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, LaunchdLabel+".plist")
	if err := os.WriteFile(p, Plist(atlasdBin, home, logPath), 0o644); err != nil {
		return "", err
	}
	if load {
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = exec.Command("launchctl", "bootout", domain+"/"+LaunchdLabel).Run()
		if out, err := exec.Command("launchctl", "bootstrap", domain, p).CombinedOutput(); err != nil {
			return p, fmt.Errorf("launchctl bootstrap: %v: %s", err, bytes.TrimSpace(out))
		}
	}
	return p, nil
}

// LaunchdUninstall boots out the agent (unless unload=false) and removes the plist.
func LaunchdUninstall(dir string, unload bool) (string, error) {
	p := filepath.Join(dir, LaunchdLabel+".plist")
	if unload {
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+LaunchdLabel).Run()
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	return p, nil
}
