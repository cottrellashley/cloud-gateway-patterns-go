package pagination

import (
	"errors"
	"testing"
	"time"
)

type query struct {
	Filter string
	Sort   string
}

func TestCursorRoundTrip(t *testing.T) {
	c := NewCodec([]byte("0123456789abcdef0123456789abcdef"))
	cur, err := c.Encode(query{"a=1", "id"}, map[string]any{"offset": 100}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var pos struct{ Offset int }
	if err := c.Decode(cur, query{"a=1", "id"}, &pos); err != nil || pos.Offset != 100 {
		t.Fatalf("%v %+v", err, pos)
	}
	if err := c.Decode(cur, query{"a=2", "id"}, &pos); !errors.Is(err, ErrCursorMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := c.Decode(cur[:len(cur)-2]+"zz", query{"a=1", "id"}, &pos); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("forged: %v", err)
	}
	other := NewCodec([]byte("another-key-another-key-another-"))
	if err := other.Decode(cur, query{"a=1", "id"}, &pos); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("wrong key: %v", err)
	}
	c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := c.Decode(cur, query{"a=1", "id"}, &pos); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestClamp(t *testing.T) {
	if ClampPageSize(0, 100, 500) != 100 || ClampPageSize(9999, 100, 500) != 500 || ClampPageSize(7, 100, 500) != 7 {
		t.Fatal("clamp")
	}
}
