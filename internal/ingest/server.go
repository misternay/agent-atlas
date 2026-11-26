package ingest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"agentatlas/internal/config"
	"agentatlas/internal/event"
	"agentatlas/internal/normalize"
)

// Replies written back to the shim (one line).
const (
	ReplyOK       = "ok"
	ReplyBusy     = "busy"     // shim may spool sanitized events
	ReplyRejected = "rejected" // invalid payload; shim drops
	ReplyPaused   = "paused"   // collection paused; shim drops
)

// maxLine bounds one envelope line (raw payload is JSON-escaped inside it).
const maxLine = 4*event.MaxPayloadBytes + 64<<10

// Server is the Unix-domain-socket ingestion endpoint.
type Server struct {
	pipe *Pipeline
	ln   net.Listener
	wg   sync.WaitGroup
}

// Listen creates the socket at path (0600, inside a 0700 directory). It refuses
// to replace a socket that another live daemon is serving.
func Listen(path string, pipe *Pipeline) (*Server, error) {
	ln, err := ListenUnix(path)
	if err != nil {
		return nil, err
	}
	return &Server{pipe: pipe, ln: ln}, nil
}

// ListenUnix opens a private daemon socket and removes stale socket files only.
func ListenUnix(path string) (net.Listener, error) {
	if err := config.EnsureSocketDir(path); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket %s", path)
		}
		if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
			c.Close()
			return nil, fmt.Errorf("another atlasd is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (s *Server) Close() error { return s.ln.Close() }

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				s.wg.Wait()
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReaderSize(io.LimitReader(c, maxLine), 64<<10)
	line, err := r.ReadBytes('\n')
	if len(line) == 0 && errors.Is(err, io.EOF) {
		return // liveness probe (e.g. `atlas doctor`): connect + close, nothing sent
	}
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		s.pipe.Stats.Rejected.Add(1)
		reply(c, ReplyRejected)
		return
	}
	s.pipe.Stats.EnvelopesReceived.Add(1)
	reply(c, s.Accept(line))
}

// Accept processes one envelope line and returns the reply code. Raw payload
// bytes live only in this call's memory.
func (s *Server) Accept(line []byte) string {
	var env event.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		s.pipe.Stats.Rejected.Add(1)
		return ReplyRejected
	}
	if s.pipe.Paused() {
		return ReplyPaused
	}
	if env.Dropped > 0 {
		s.pipe.RecordGap(env.Dropped, "shim_drop")
	}
	if len(env.Raw) == 0 && len(env.Events) == 0 {
		return ReplyOK
	}
	evs, err := normalize.Envelope(&env)
	if err != nil {
		s.pipe.Stats.Rejected.Add(1)
		return ReplyRejected
	}
	if err := s.pipe.Submit(evs); err != nil {
		return ReplyBusy
	}
	return ReplyOK
}

func reply(c net.Conn, code string) {
	_, _ = c.Write([]byte(code + "\n"))
}
