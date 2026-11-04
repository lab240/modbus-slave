package device

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lab240/modbus-slave/internal/source"
)

func read(fc byte, addr, count uint16) []byte {
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16([]byte{fc}, addr), count)
}

func TestGenericFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s1.dat")
	os.WriteFile(p, []byte("4523\n1013\n\n-5\n0x1F # hex\n"), 0o644)
	d := NewGeneric(1, 20, &source.File{Path: p, MaxAge: 10 * time.Second})
	got := d.Handle(read(3, 0, 5))
	want := []byte{3, 10, 0x11, 0xAB, 0x03, 0xF5, 0xFF, 0xFB, 0x00, 0x1F, 0, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("FC3: got % X, want % X", got, want)
	}
	if got := d.Handle(read(3, 18, 3)); !bytes.Equal(got, []byte{0x83, 2}) {
		t.Fatalf("out of range: got % X, want exception 2", got)
	}
	if got := d.Handle([]byte{6, 0, 0, 0, 1}); !bytes.Equal(got, []byte{0x86, 1}) {
		t.Fatalf("FC6 on generic: got % X, want exception 1 (v1 answers FC3 only)", got)
	}
	// stale file -> zeros
	old := time.Now().Add(-time.Minute)
	os.Chtimes(p, old, old)
	if got := d.Handle(read(3, 0, 2)); !bytes.Equal(got, []byte{3, 4, 0, 0, 0, 0}) {
		t.Fatalf("stale: got % X, want zeros", got)
	}
	// missing file -> zeros
	os.Remove(p)
	if got := d.Handle(read(3, 0, 1)); !bytes.Equal(got, []byte{3, 2, 0, 0}) {
		t.Fatalf("missing: got % X, want zeros", got)
	}
}
