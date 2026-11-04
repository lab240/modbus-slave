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

func TestKTH2R(t *testing.T) {
	d := NewKTH2R(3, 2, 115200, 'N')
	got := d.Handle(read(3, 0, 5))
	if !bytes.Equal(got[2:], []byte{0, 1, 1, 0x20, 'K', 'T', 'H', '2', '-', 'R'}) {
		t.Fatalf("passport: % X", got)
	}
	if got := d.Handle(read(3, 0x000C, 3)); !bytes.Equal(got[2:], []byte{0, 3, 0, 7, 0, 0}) {
		t.Fatalf("unit/speed/parity: % X", got)
	}
	tr := d.Handle(read(4, 0x0300, 2))
	T, RH := int16(binary.BigEndian.Uint16(tr[2:])), int16(binary.BigEndian.Uint16(tr[4:]))
	if T < 225 || T > 235 || RH < 310 || RH > 330 { // line 2: T 230+-5, RH 320+-10
		t.Fatalf("T %d RH %d out of range for line 2", T, RH)
	}
	if got := d.Handle(read(3, 0x1002, 2)); !bytes.Equal(got[2:], []byte{0, 2, 0, 3}) {
		t.Fatalf("line/unit: % X", got)
	}
	if got := d.Handle(read(3, 0x2000, 125)); len(got) != 252 || got[2] != 0x33 || got[3] != 0x00 {
		t.Fatalf("block: len %d % X", len(got), got[:4])
	}
	// correction write (FC 16) then read back
	if got := d.Handle([]byte{16, 0x04, 0xFF, 0, 2, 4, 0xFF, 0xFE, 0, 3}); !bytes.Equal(got, []byte{16, 0x04, 0xFF, 0, 2}) {
		t.Fatalf("FC16: % X", got)
	}
	if got := d.Handle(read(3, 0x04FF, 2)); !bytes.Equal(got[2:], []byte{0xFF, 0xFE, 0, 3}) {
		t.Fatalf("corr: % X", got)
	}
	// FC 6 to 0x000C changes the address
	d.Handle([]byte{6, 0, 0x0C, 0, 9})
	if d.Unit() != 9 {
		t.Fatalf("unit %d after FC6, want 9", d.Unit())
	}
	if got := d.Handle(read(3, 0x0100, 1)); !bytes.Equal(got, []byte{0x83, 2}) {
		t.Fatalf("unmapped: % X", got)
	}
	if got := d.Handle([]byte{8, 0, 0, 0xA5, 0x37}); !bytes.Equal(got, []byte{8, 0, 0, 0xA5, 0x37}) {
		t.Fatalf("echo: % X", got)
	}
	if got := d.Handle(read(3, 0, 126)); !bytes.Equal(got, []byte{0x83, 3}) {
		t.Fatalf("count 126: % X", got)
	}
}
