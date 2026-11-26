package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"agentatlas/internal/correlate"
)

// Message is one WebSocket frame sent to clients.
type Message struct {
	Type    string             `json:"type"` // hello | delta | resync_required | status
	Seq     int64              `json:"seq,omitempty"`
	Cursor  int64              `json:"cursor,omitempty"`
	Reason  string             `json:"reason,omitempty"`
	Paused  *bool              `json:"paused,omitempty"`
	Event   any                `json:"event,omitempty"`
	Session *correlate.Session `json:"session,omitempty"`
	Spans   []correlate.Span   `json:"spans,omitempty"`
	Links   []correlate.Link   `json:"links,omitempty"`
	Replay  bool               `json:"replay,omitempty"` // true during catch-up
	Extra   map[string]any     `json:"extra,omitempty"`
}

// Hub fans committed deltas out to WebSocket subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch     chan Message
	closed bool
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[*subscriber]struct{}{}} }

// Publish sends deltas to every subscriber. A subscriber that cannot keep up
// is disconnected; it reconnects with its cursor and catches up from storage.
func (h *Hub) Publish(deltas []correlate.Delta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range deltas {
		m := deltaMessage(d)
		for s := range h.subs {
			if s.closed {
				continue
			}
			select {
			case s.ch <- m:
			default:
				s.closed = true
				close(s.ch)
				delete(h.subs, s)
			}
		}
	}
}

// Broadcast sends a non-delta message (e.g. pause status, resync after clear).
// A subscriber whose buffer is full is disconnected (it then reconnects and is
// resynced), so control messages are never silently lost.
func (h *Hub) Broadcast(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.closed {
			continue
		}
		select {
		case s.ch <- m:
		default:
			s.closed = true
			close(s.ch)
			delete(h.subs, s)
		}
	}
}

func (h *Hub) subscribe() *subscriber {
	s := &subscriber{ch: make(chan Message, 2048)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *Hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
	h.mu.Unlock()
}

// Subscribers returns the current subscriber count.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func deltaMessage(d correlate.Delta) Message {
	ev := d.Event
	return Message{Type: "delta", Seq: d.Seq, Event: ev, Session: d.Session, Spans: d.Spans, Links: d.Links}
}

// maxCatchUp bounds how many stored events a reconnecting client may replay
// before being told to resync from a snapshot instead.
const maxCatchUp = 5000

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// A present after_seq (including 0) is a cursor to resume from; absent means live-only.
	afterStr := q.Get("after_seq")
	hasCursor := afterStr != ""
	after, _ := strconv.ParseInt(afterStr, 10, 64)
	genStr := q.Get("gen")
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{sessionProtocol}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() { // reader: detect client close; we ignore client messages
		for {
			if _, _, err := c.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	// Subscribe before reading bounds so nothing committed in between is lost.
	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	send := func(m Message) error {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	minSeq, maxSeq, err := s.store.SeqBounds(ctx)
	if err != nil {
		return
	}
	paused := s.pipe.Paused()
	if err := send(Message{Type: "hello", Cursor: maxSeq, Paused: &paused}); err != nil {
		return
	}
	if genStr != "" {
		if g, err := strconv.ParseInt(genStr, 10, 64); err != nil || g != s.store.Generation(ctx) {
			_ = send(Message{Type: "resync_required", Reason: "history_changed", Cursor: maxSeq})
			c.Close(websocket.StatusNormalClosure, "resync_required")
			return
		}
	}
	last := after
	if hasCursor {
		expired := after > maxSeq || (minSeq > 0 && after < minSeq-1) || (minSeq == 0 && after < maxSeq)
		if !expired && maxSeq-after > maxCatchUp {
			expired = true
		}
		if expired {
			_ = send(Message{Type: "resync_required", Reason: "cursor_expired", Cursor: maxSeq})
			c.Close(websocket.StatusNormalClosure, "resync_required")
			return
		}
		// Catch up from storage with current row state (converges by last_seq).
		for last < maxSeq {
			evs, err := s.store.EventsAfter(ctx, last, 500)
			if err != nil || len(evs) == 0 {
				break
			}
			for _, se := range evs {
				m := Message{Type: "delta", Seq: se.Seq, Event: se.Event, Replay: true}
				if se.SessionID != "" {
					m.Session, _ = s.store.GetSession(ctx, se.SessionID)
				}
				// Every span this event touched (e.g. all spans closed by a
				// session.ended), at its current state; clients converge by last_seq.
				seen := map[string]bool{}
				if se.SpanID != "" {
					if sp, _ := s.store.LoadSpan(se.SpanID); sp != nil {
						m.Spans = append(m.Spans, *sp)
						seen[sp.ID] = true
					}
					m.Links, _ = s.store.LoadLinks(se.SpanID)
				}
				if se.SessionID != "" {
					touched, _ := s.store.SpansTouchedAt(ctx, se.SessionID, se.Seq)
					for _, sp := range touched {
						if !seen[sp.ID] {
							m.Spans = append(m.Spans, sp)
							seen[sp.ID] = true
						}
					}
				}
				if err := send(m); err != nil {
					return
				}
				last = se.Seq
			}
		}
	} else {
		last = maxSeq
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-sub.ch:
			if !ok {
				// Too slow: tell the client to reconnect from its cursor.
				_ = send(Message{Type: "resync_required", Reason: "slow_consumer", Cursor: last})
				c.Close(websocket.StatusTryAgainLater, "slow consumer")
				return
			}
			if m.Type == "delta" {
				if m.Seq <= last {
					continue // already delivered during catch-up
				}
				last = m.Seq
			}
			if err := send(m); err != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}
