package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func capture(f func()) string {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

// bytes pattern: 03 -> 07 and 06 -> 0E are 0->1 flips.
func TestReportBytes(t *testing.T) {
	g := []byte{0x00, 0x01, 0x02, 0x07, 0x04, 0x05, 0x0E, 0x07}
	out := capture(func() { report([][]byte{g}, []string{"x"}, "bytes") })
	if !strings.Contains(out, "2 bytes off the counter; first 8 bytes: bits 0->1 2, 1->0 0") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestReportTokens(t *testing.T) {
	clean := payload(1, 100, "tokens")
	crossed := payload(3, 50, "tokens")
	out := capture(func() {
		if rc := report([][]byte{clean, crossed, nil}, []string{"a", "b", "c"}, "tokens"); rc != 1 {
			t.Errorf("rc %d, want 1 (port c got nothing)", rc)
		}
	})
	for _, want := range []string{"clean                        from [P1 x10]", "from [P3 x5]", "nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}
