// Package sse serves Server-Sent Events correctly: the headers that stop
// proxies and browsers buffering, a flush after every event, heartbeats so
// idle streams survive load balancer idle timeouts, Last-Event-ID resume from
// a replay buffer, and bounded buffering so a slow client cannot hold
// unbounded memory.
//
// Shape taken from tmaxmax/go-sse (Session.Upgrade sets Content-Type and
// flushes; FiniteReplayer keeps a ring of recent messages and replays after
// Last-Event-ID) and from net/http/httputil, which flushes text/event-stream
// immediately when proxying. The MCP Go SDK's StreamableHTTP handler uses the
// same keep-alive and EventStore ideas.
package sse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one SSE message.
type Event struct {
	ID    string
	Type  string
	Data  []byte
	Retry time.Duration
}

// WriteTo renders the event in wire format. Data is split on newlines into
// multiple data: lines as the spec requires.
func (e Event) WriteTo(w io.Writer) (int64, error) {
	var b bytes.Buffer
	if e.ID != "" {
		b.WriteString("id: ")
		b.WriteString(e.ID)
		b.WriteByte('\n')
	}
	if e.Type != "" {
		b.WriteString("event: ")
		b.WriteString(e.Type)
		b.WriteByte('\n')
	}
	if e.Retry > 0 {
		b.WriteString("retry: ")
		b.WriteString(strconv.FormatInt(e.Retry.Milliseconds(), 10))
		b.WriteByte('\n')
	}
	for _, line := range bytes.Split(e.Data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	n, err := w.Write(b.Bytes())
	return int64(n), err
}

// LastEventID returns the client's resume point, if any.
func LastEventID(r *http.Request) string { return r.Header.Get("Last-Event-ID") }

// Stream is an upgraded response.
type Stream struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	mu  sync.Mutex
	err error
}

// Upgrade sets the SSE headers and sends them. It fails if the ResponseWriter
// cannot flush (which http.ResponseController resolves through Unwrap).
func Upgrade(w http.ResponseWriter, r *http.Request) (*Stream, error) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx
	if r.ProtoMajor == 1 {
		h.Set("Connection", "keep-alive")
	}
	// Streams outlive WriteTimeout; clear per-connection deadlines.
	_ = rc.SetWriteDeadline(time.Time{})
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return nil, fmt.Errorf("sse: response writer cannot flush: %w", err)
	}
	return &Stream{w: w, rc: rc}, nil
}

// Send writes and flushes one event.
func (s *Stream) Send(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, err := e.WriteTo(s.w); err != nil {
		s.err = err
		return err
	}
	if err := s.rc.Flush(); err != nil {
		s.err = err
		return err
	}
	return nil
}

// Comment writes a comment line (": text"), the standard keep-alive.
func (s *Stream) Comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, err := io.WriteString(s.w, ": "+text+"\n\n"); err != nil {
		s.err = err
		return err
	}
	return s.rc.Flush()
}

// ErrResumeExpired means the Last-Event-ID is older than the replay buffer.
var ErrResumeExpired = errors.New("sse: resume point no longer retained")

// Ring is a replay buffer with monotonically increasing numeric IDs rendered
// as 20-digit zero-padded strings so that string comparison equals numeric
// comparison (clients never parse them).
type Ring struct {
	mu     sync.Mutex
	events []Event
	next   uint64 // id of the next event
	size   int
}

// NewRing retains the last size events.
func NewRing(size int) *Ring { return &Ring{size: size, next: 1} }

// FormatID renders an id.
func FormatID(n uint64) string { return fmt.Sprintf("%020d", n) }

// Put assigns the next ID (unless e.ID is already set by the caller) and
// stores the event.
func (r *Ring) Put(e Event) Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.ID == "" {
		e.ID = FormatID(r.next)
	}
	r.next++
	r.events = append(r.events, e)
	if len(r.events) > r.size {
		r.events = r.events[len(r.events)-r.size:]
	}
	return e
}

// After returns events with ID greater than lastID, in order. If lastID is
// older than the oldest retained event, ErrResumeExpired is returned together
// with everything retained, so the caller can send a gap marker and continue.
func (r *Ring) After(lastID string) ([]Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return nil, nil
	}
	oldest := r.events[0].ID
	if strings.Compare(lastID, oldest) < 0 && lastID != prevID(oldest) {
		out := make([]Event, len(r.events))
		copy(out, r.events)
		return out, ErrResumeExpired
	}
	var out []Event
	for _, e := range r.events {
		if strings.Compare(e.ID, lastID) > 0 {
			out = append(out, e)
		}
	}
	return out, nil
}

func prevID(id string) string {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	return FormatID(n - 1)
}

// Options for Serve.
type Options struct {
	// Heartbeat interval; 0 disables. 15s keeps most load balancers happy
	// (Azure Front Door and Application Gateway idle timeouts are minutes).
	Heartbeat time.Duration
	// Replay enables Last-Event-ID resume.
	Replay *Ring
	// OnResumeExpired is sent when the client's resume point is gone; nil
	// sends a default "gap" event.
	OnResumeExpired func(lastID string) Event
}

// Serve upgrades the response and forwards events from src until src is
// closed or the client disconnects. It is the loop every SSE handler needs;
// the handler only has to produce events on a channel.
func Serve(w http.ResponseWriter, r *http.Request, src <-chan Event, o Options) error {
	st, err := Upgrade(w, r)
	if err != nil {
		return err
	}
	if o.Replay != nil {
		if last := LastEventID(r); last != "" {
			missed, rerr := o.Replay.After(last)
			if errors.Is(rerr, ErrResumeExpired) {
				gap := Event{Type: "gap", Data: []byte(`{"resumedFrom":"` + last + `","reason":"expired"}`)}
				if o.OnResumeExpired != nil {
					gap = o.OnResumeExpired(last)
				}
				if err := st.Send(gap); err != nil {
					return err
				}
			}
			for _, e := range missed {
				if err := st.Send(e); err != nil {
					return err
				}
			}
		}
	}
	var hb <-chan time.Time
	if o.Heartbeat > 0 {
		t := time.NewTicker(o.Heartbeat)
		defer t.Stop()
		hb = t.C
	}
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return nil // client went away; nothing to write
		case <-hb:
			if err := st.Comment("keepalive"); err != nil {
				return err
			}
		case e, ok := <-src:
			if !ok {
				return nil
			}
			if err := st.Send(e); err != nil {
				return err
			}
		}
	}
}

// ErrClientTooSlow is returned by Sink.Send when the buffer stays full.
var ErrClientTooSlow = errors.New("sse: client too slow")

// Sink is the producer side of a bounded per-client buffer. Producers call
// Send; if the consumer (Serve) does not drain within SlowTimeout the send
// fails instead of growing memory, which is the backpressure rule for SSE.
type Sink struct {
	ch          chan Event
	slowTimeout time.Duration
	once        sync.Once
}

// NewSink creates a sink with the given buffer depth.
func NewSink(depth int, slowTimeout time.Duration) *Sink {
	return &Sink{ch: make(chan Event, depth), slowTimeout: slowTimeout}
}

// C is the channel to pass to Serve.
func (s *Sink) C() <-chan Event { return s.ch }

// Send enqueues or fails with ErrClientTooSlow / ctx error.
func (s *Sink) Send(ctx context.Context, e Event) error {
	select {
	case s.ch <- e:
		return nil
	default:
	}
	t := time.NewTimer(s.slowTimeout)
	defer t.Stop()
	select {
	case s.ch <- e:
		return nil
	case <-t.C:
		return ErrClientTooSlow
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close ends the stream; Serve returns after draining.
func (s *Sink) Close() { s.once.Do(func() { close(s.ch) }) }
