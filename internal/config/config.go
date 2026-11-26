// Package config resolves Agent Atlas paths and defaults (design §7.3).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Defaults.
const (
	DefaultAddr          = "127.0.0.1:7878"
	DefaultRetention     = 14 * 24 * time.Hour
	DefaultMaxDBBytes    = 1 << 30 // 1 GB
	DefaultSpoolMaxBytes = 32 << 20
	DefaultStaleAfter    = 10 * time.Minute
	DefaultLiveWindow    = 60 * time.Second
)

// Config is persisted as config.json in the Atlas home directory.
type Config struct {
	Addr             string `json:"addr"`
	RetentionDays    int    `json:"retention_days"`
	MaxDBBytes       int64  `json:"max_db_bytes"`
	SpoolMaxBytes    int64  `json:"spool_max_bytes"`
	StaleAfterSecs   int    `json:"stale_after_secs"`
	HostID           string `json:"host_id"`
	ExportEnabled    bool   `json:"export_enabled"`     // OTLP export; off by default
	ExportEndpoint   string `json:"export_endpoint"`    // e.g. http://127.0.0.1:4318/v1/traces
	ExportPreviewAck bool   `json:"export_preview_ack"` // user reviewed the export preview
}

// Default returns the default configuration.
func Default() Config {
	return Config{
		Addr:           DefaultAddr,
		RetentionDays:  int(DefaultRetention / (24 * time.Hour)),
		MaxDBBytes:     DefaultMaxDBBytes,
		SpoolMaxBytes:  DefaultSpoolMaxBytes,
		StaleAfterSecs: int(DefaultStaleAfter / time.Second),
		HostID:         "local",
	}
}

// Home returns the Atlas data directory. ATLAS_HOME overrides the default
// ~/Library/Application Support/AgentAtlas (macOS).
func Home() string {
	if h := os.Getenv("ATLAS_HOME"); h != "" {
		return h
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "AgentAtlas")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".agent-atlas")
}

// Paths under the Atlas home.
type Paths struct {
	Home, DB, Socket, Control, Spool, Token, Config, Log string
}

// PathsFor returns paths rooted at home.
func PathsFor(home string) Paths {
	return Paths{
		Home:    home,
		DB:      filepath.Join(home, "atlas.db"),
		Socket:  socketPath(home, "atlasd.sock", "atlas-"),
		Control: socketPath(home, "control.sock", "atlasctl-"),
		Spool:   filepath.Join(home, "spool"),
		Token:   filepath.Join(home, "token"),
		Config:  filepath.Join(home, "config.json"),
		Log:     filepath.Join(home, "atlasd.log"),
	}
}

// maxSocketPath is below the macOS sun_path limit (104 bytes incl. NUL).
const maxSocketPath = 100

// socketPath uses a private, per-user fallback independent of the process's
// TMPDIR so the daemon, CLI, and harness can run in different environments.
func socketPath(home, name, prefix string) string {
	p := filepath.Join(home, name)
	if len(p) <= maxSocketPath {
		return p
	}
	h := sha256.Sum256([]byte(filepath.Clean(home)))
	return filepath.Join(fmt.Sprintf("/tmp/agentatlas-%d", os.Getuid()), prefix+hex.EncodeToString(h[:12])+".sock")
}

// EnsureSocketDir creates and verifies the owning process's private socket directory.
func EnsureSocketDir(socket string) error {
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkOwnedPath(dir, os.ModeDir, 0o700)
}

// CheckPrivateSocket checks the directory and socket before sending credentials or raw events.
func CheckPrivateSocket(socket string) error {
	if err := checkOwnedPath(filepath.Dir(socket), os.ModeDir, 0o700); err != nil {
		return err
	}
	return checkOwnedPath(socket, os.ModeSocket, 0o600)
}

func checkOwnedPath(path string, kind, mode fs.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Type() != kind || info.Mode().Perm() != mode {
		return fmt.Errorf("%s must be a %s owned by this user with permissions %04o", path, kind, mode)
	}
	return nil
}

// EnsureHome creates the home directory with 0700 permissions.
func EnsureHome(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return os.Chmod(home, 0o700)
}

// TryDaemonLock holds exclusive ownership of an Atlas home for a daemon run.
func TryDaemonLock(home string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(home, "atlasd.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Load reads config.json, falling back to defaults for missing values.
func Load(p Paths) (Config, error) {
	c := Default()
	b, err := os.ReadFile(p.Config)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Default(), err
	}
	d := Default()
	if c.Addr == "" {
		c.Addr = d.Addr
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = d.RetentionDays
	}
	if c.MaxDBBytes <= 0 {
		c.MaxDBBytes = d.MaxDBBytes
	}
	if c.SpoolMaxBytes <= 0 {
		c.SpoolMaxBytes = d.SpoolMaxBytes
	}
	if c.StaleAfterSecs <= 0 {
		c.StaleAfterSecs = d.StaleAfterSecs
	}
	if c.HostID == "" {
		c.HostID = d.HostID
	}
	return c, nil
}

// Save writes config.json with 0600 permissions.
func Save(p Paths, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.Config + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.Config)
}
