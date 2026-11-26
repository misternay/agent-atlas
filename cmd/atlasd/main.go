// Command atlasd is the Agent Atlas local daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"agentatlas/internal/api"
	"agentatlas/internal/config"
	"agentatlas/internal/daemon"
	"agentatlas/web"
)

func main() {
	home := flag.String("home", config.Home(), "Atlas data directory")
	addr := flag.String("addr", "", "loopback HTTP address (default from config, 127.0.0.1:7878)")
	logFile := flag.String("log", "", "log file (default stderr)")
	version := flag.Bool("version", false, "print version")
	flag.Parse()
	if *version {
		fmt.Println("atlasd", api.Version)
		return
	}
	var w io.Writer = os.Stderr
	if *logFile != "" {
		if err := os.MkdirAll(filepath.Dir(*logFile), 0o700); err == nil {
			if f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				defer f.Close()
				w = f
			}
		}
	}
	log := slog.New(slog.NewTextHandler(w, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, daemon.Options{Home: *home, Addr: *addr, UI: web.Dist(), Logger: log}); err != nil {
		log.Error("atlasd failed", "err", err)
		os.Exit(1)
	}
}
