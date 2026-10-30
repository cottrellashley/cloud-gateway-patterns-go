package sse

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventWireFormat(t *testing.T) {
	var b strings.Builder
	Event{ID: "1", Type: "item", Data: []byte("line1\nline2"), Retry: 2 * time.Second}.WriteTo(&b)
	want := "id: 1\nevent: item\nretry: 2000\ndata: line1\ndata: line2\n\n"
	if b.String() != want {
		t.Fatalf("%q", b.String())
	}
}

func TestRingReplayAndExpiry(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 5; i++ {
		r.Put(Event{Data: []byte{byte('a' + i)}})
	}
	// retained: ids 3,4,5
	evs, err := r.After(FormatID(3))
	if err != nil || len(evs) != 2 || evs[0].ID != FormatID(4) {
		t.Fatalf("%v %v", err, evs)
	}
	evs, err = r.After(FormatID(2)) // exactly before oldest: no gap
	if err != nil || len(evs) != 3 {
		t.Fatalf("%v %d", err, len(evs))
	}
	evs, err = r.After(FormatID(1)) // older than retained: gap
	if !errors.Is(err, ErrResumeExpired) || len(evs) != 3 {
		t.Fatalf("%v %d", err, len(evs))
	}
	if evs, _ := r.After(FormatID(5)); len(evs) != 0 {
		t.Fatal("nothing after latest")
	}
}

func TestServeStreamsAndResumes(t *testing.T) {
	ring := NewRing(100)
	for i := 0; i < 3; i++ {
		ring.Put(Event{Type: "item", Data: []byte("old")})
	}
	sink := NewSink(8, time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = Serve(w, r, sink.C(), Options{Heartbeat: 20 * time.Millisecond, Replay: ring})
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Last-Event-ID", FormatID(1))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		sink.Send(context.Background(), ring.Put(Event{Type: "item", Data: []byte("new")}))
		time.Sleep(50 * time.Millisecond) // let a heartbeat through
		sink.Close()
	}()
	sc := bufio.NewScanner(resp.Body)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	joined := strings.Join(got, "\n")
	// replayed ids 2 and 3, then the new event with id 4, and a keepalive comment
	if !strings.Contains(joined, "id: "+FormatID(2)) || !strings.Contains(joined, "id: "+FormatID(3)) || !strings.Contains(joined, "data: new") {
		t.Fatalf("stream:\n%s", joined)
	}
	if strings.Contains(joined, "data: old\nid: "+FormatID(1)) {
		t.Fatal("event 1 must not be replayed")
	}
	if !strings.Contains(joined, ": keepalive") {
		t.Fatalf("no heartbeat:\n%s", joined)
	}
}

func TestSinkBackpressure(t *testing.T) {
	s := NewSink(1, 10*time.Millisecond)
	if err := s.Send(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), Event{}); !errors.Is(err, ErrClientTooSlow) {
		t.Fatalf("want ErrClientTooSlow, got %v", err)
	}
}
