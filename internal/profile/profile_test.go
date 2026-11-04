package profile

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func read(fc byte, addr, count uint16) []byte {
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16([]byte{fc}, addr), count)
}

func TestKTH2R(t *testing.T) {
	p, err := Load("kth2r")
	if err != nil {
		t.Fatal(err)
	}
	d := Build(p, Env{Line: 2, Unit: 3, Baud: 115200, Parity: 'N'})
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
	d.Served.Add(70000)
	if got := d.Handle(read(3, 0x1000, 5)); !bytes.Equal(got[2:], []byte{0, 1, 0x11, 0x70, 0, 2, 0, 3, 0x04, 0x80}) {
		t.Fatalf("served/line/unit/baud: % X", got)
	}
	if got := d.Handle(read(3, 0x2000, 125)); len(got) != 252 || !bytes.Equal(got[2:6], []byte{0x33, 0x00, 0x33, 0x01}) {
		t.Fatalf("block: len %d % X", len(got), got[:6])
	}
	// correction (FC 16) is added to T / RH
	if got := d.Handle([]byte{16, 0x04, 0xFF, 0, 2, 4, 0xFF, 0x9C, 0, 100}); !bytes.Equal(got, []byte{16, 0x04, 0xFF, 0, 2}) {
		t.Fatalf("FC16: % X", got)
	}
	tr2 := d.Handle(read(3, 0x0300, 2))
	T2, RH2 := int16(binary.BigEndian.Uint16(tr2[2:])), int16(binary.BigEndian.Uint16(tr2[4:]))
	if d := int(T2) - int(T) + 100; d < -1 || d > 1 {
		t.Fatalf("T %d -> %d with correction -100", T, T2)
	}
	if d := int(RH2) - int(RH) - 100; d < -1 || d > 1 {
		t.Fatalf("RH %d -> %d with correction +100", RH, RH2)
	}
	// FC 6 to 0x000C changes the address; out of 1..247 -> exception 2
	if got := d.Handle([]byte{6, 0, 0x0C, 0, 250}); !bytes.Equal(got, []byte{0x86, 2}) {
		t.Fatalf("unit 250: % X", got)
	}
	d.Handle([]byte{6, 0, 0x0C, 0, 9})
	if d.Unit() != 9 {
		t.Fatalf("unit %d after FC6, want 9", d.Unit())
	}
	if got := d.Handle([]byte{6, 0, 0, 0, 1}); !bytes.Equal(got, []byte{0x86, 2}) {
		t.Fatalf("write to the read-only model: % X", got)
	}
	if got := d.Handle(read(3, 0x0100, 1)); !bytes.Equal(got, []byte{0x83, 2}) {
		t.Fatalf("unmapped: % X", got)
	}
	if got := d.Handle([]byte{8, 0, 0, 0xA5, 0x37}); !bytes.Equal(got, []byte{8, 0, 0, 0xA5, 0x37}) {
		t.Fatalf("echo: % X", got)
	}
}

func TestWaveShapes(t *testing.T) {
	w := &Wave{Period: 100}
	for _, c := range []struct {
		shape string
		t     float64
		want  float64
	}{
		{"saw", 0, -1}, {"saw", 50, 0}, {"saw", 75, 0.5},
		{"triangle", 0, -1}, {"triangle", 50, 1}, {"triangle", 25, 0},
		{"sine", 25, 1}, {"sine", 75, -1},
		{"square", 10, 1}, {"square", 60, -1},
	} {
		w.Shape = c.shape
		if got := shape(w, c.t); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s(%v) = %v, want %v", c.shape, c.t, got, c.want)
		}
	}
}

func TestSoftSensorFile(t *testing.T) {
	dir := t.TempDir()
	cpu, up := filepath.Join(dir, "cpu.dat"), filepath.Join(dir, "up.dat")
	os.WriteFile(cpu, []byte("4523\n"), 0o644)
	os.WriteFile(up, []byte("100000\n"), 0o644)
	data, err := os.ReadFile("../../examples/soft-sensor.yaml")
	if err != nil {
		t.Fatal(err)
	}
	y := strings.NewReplacer("/tmp/cpu.dat", cpu, "/tmp/up.dat", up).Replace(string(data))
	p, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	d := Build(p, Env{Unit: 1})
	got := d.Handle(read(3, 20, 3))
	f := math.Float32frombits(binary.BigEndian.Uint32(got[2:]))
	if math.Abs(float64(f)-45.23) > 1e-4 || binary.BigEndian.Uint16(got[6:]) != 452 {
		t.Fatalf("float32 %v, int16 %d (want 45.23, 452): % X", f, binary.BigEndian.Uint16(got[6:]), got)
	}
	if got := d.Handle(read(3, 30, 2)); binary.BigEndian.Uint32(got[2:]) != 100000 {
		t.Fatalf("uint32: % X", got)
	}
	if got := d.Handle(read(3, 0, 5)); string(got[2:]) != "NAPI SOFT\x00" {
		t.Fatalf("string: %q", got[2:])
	}
	// FC 6 is not in functions: exception 1
	if got := d.Handle([]byte{6, 0, 40, 0, 1}); !bytes.Equal(got, []byte{0x86, 1}) {
		t.Fatalf("FC6: % X", got)
	}
	// stale file: on_stale exception -> exception 4; default -> zeros
	old := time.Now().Add(-time.Minute)
	os.Chtimes(cpu, old, old)
	if got := d.Handle(read(3, 20, 2)); !bytes.Equal(got, []byte{0x83, 4}) {
		t.Fatalf("stale float32: % X, want exception 4", got)
	}
	if got := d.Handle(read(3, 22, 1)); !bytes.Equal(got, []byte{3, 2, 0, 0}) {
		t.Fatalf("stale int16: % X, want zeros", got)
	}
}

func TestRWAndWordOrder(t *testing.T) {
	p, err := Parse([]byte(`
functions: [3, 16]
holding:
  - {addr: 0, count: 2, type: int32, values: [-2, 70000], word_order: little, rw: true}
  - {addr: 10, file: "/nonexistent/{line}-{unit}.dat"}
`))
	if err != nil {
		t.Fatal(err)
	}
	d := Build(p, Env{Line: 1, Unit: 5})
	if got := d.Handle(read(3, 0, 4)); !bytes.Equal(got[2:], []byte{0xFF, 0xFE, 0xFF, 0xFF, 0x11, 0x70, 0x00, 0x01}) {
		t.Fatalf("int32 little: % X", got)
	}
	if got := d.Handle([]byte{16, 0, 1, 0, 2, 4, 0xAA, 0xBB, 0xCC, 0xDD}); !bytes.Equal(got, []byte{16, 0, 1, 0, 2}) {
		t.Fatalf("FC16: % X", got)
	}
	if got := d.Handle(read(3, 0, 3)); !bytes.Equal(got[2:], []byte{0xFF, 0xFE, 0xAA, 0xBB, 0xCC, 0xDD}) {
		t.Fatalf("after write: % X", got)
	}
	// writing into a block that ends in a read-only register writes nothing
	if got := d.Handle([]byte{16, 0, 3, 0, 2, 4, 1, 2, 3, 4}); !bytes.Equal(got, []byte{0x90, 2}) {
		t.Fatalf("partly read-only: % X", got)
	}
	if got := d.Handle(read(3, 3, 1)); !bytes.Equal(got[2:], []byte{0x00, 0x01}) {
		t.Fatalf("partial write happened: % X", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"holding: [{addr: 0, valu: 1}]", "field valu not found"},
		{"holding: [{addr: 0, value: 1, random: true}]", "exactly one source"},
		{"holding: [{addr: 0, count: 2, value: 1}, {addr: 1, value: 2}]", "already in"},
		{"holding: [{addr: 0, value: 1, type: int64}]", "type"},
		{"holding: [{addr: 0, wave: {shape: saw, amp: 1}}]", "period"},
		{"holding: [{addr: 0, random: true, rw: true}]", "rw works"},
		{"holding: [{addr: 0, value: 1, add: 5}]", "add 0x0005"},
		{"functions: [5]\nholding: [{addr: 0, value: 1}]", "not supported"},
		{"holding: [{addr: 0xFFFF, count: 2, value: 1}]", "out of"},
	} {
		_, err := Parse([]byte(c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.yaml, err, c.want)
		}
	}
}
