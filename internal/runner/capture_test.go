package runner

import (
	"bytes"
	"strings"
	"testing"
)

func TestCappedWriter_KeepsUpToCapDropsRest(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{Cap: 10}
	n, err := w.Write([]byte("0123456789ABCDE")) // 15 bytes
	if err != nil {
		t.Fatalf("Write returned err: %v", err)
	}
	if n != 15 {
		t.Fatalf("Write returned n=%d, want 15", n)
	}
	if got := w.Bytes(); !bytes.Equal(got, []byte("0123456789")) {
		t.Fatalf("Bytes()=%q, want %q", got, "0123456789")
	}
	if got, want := w.Dropped(), int64(5); got != want {
		t.Fatalf("Dropped()=%d, want %d", got, want)
	}
}

func TestCappedWriter_MultipleWritesAccumulate(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{Cap: 10}
	w.Write([]byte("hello "))
	w.Write([]byte("world!!"))
	if got := w.Bytes(); string(got) != "hello worl" {
		t.Fatalf("Bytes()=%q, want %q", got, "hello worl")
	}
	if w.Dropped() != 3 {
		t.Fatalf("Dropped()=%d, want 3", w.Dropped())
	}
}

func TestCappedWriter_ZeroCapDropsEverything(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{Cap: 0}
	w.Write([]byte("anything"))
	if len(w.Bytes()) != 0 {
		t.Fatalf("Bytes() not empty: %q", w.Bytes())
	}
	if w.Dropped() != 8 {
		t.Fatalf("Dropped()=%d, want 8", w.Dropped())
	}
}

func TestHeadTail_BelowCapReturnsHeadOnly(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat("x", 100))
	head, tail := headTail(body, 4096)
	if string(head) != string(body) {
		t.Fatalf("head should equal body when len < cap")
	}
	if tail != "" {
		t.Fatalf("tail should be empty when body fits in head; got %q", tail)
	}
}

func TestHeadTail_AboveCapSplits(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat("a", 4000) + strings.Repeat("b", 4000) + strings.Repeat("c", 4000))
	head, tail := headTail(body, 4096)
	if len(head) != 4096 {
		t.Fatalf("head len=%d, want 4096", len(head))
	}
	if len(tail) != 4096 {
		t.Fatalf("tail len=%d, want 4096", len(tail))
	}
	if !strings.HasPrefix(string(head), strings.Repeat("a", 4000)) {
		t.Fatalf("head should start with 'a's, got prefix %q", head[:8])
	}
	if !strings.HasSuffix(string(tail), strings.Repeat("c", 4000)) {
		t.Fatalf("tail should end with 'c's")
	}
}
