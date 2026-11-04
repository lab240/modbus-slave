package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/profile"
)

func TestServeTCP(t *testing.T) {
	p, err := profile.Load("kth2r")
	if err != nil {
		t.Fatal(err)
	}
	env := profile.Env{Line: 0, Baud: 9600, Parity: 'E'}
	e1, e2 := env, env
	e1.Unit, e2.Unit = 1, 2
	l := NewLine(0, ":0", []*device.Device{profile.Build(p, e1), profile.Build(p, e2)}, Faults{}, 0, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.ServeTCP(ctx, ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ask := func(tid uint16, unit byte, pdu []byte) []byte {
		t.Helper()
		req := append([]byte{byte(tid >> 8), byte(tid), 0, 0, 0, byte(len(pdu) + 1), unit}, pdu...)
		c.Write(req)
		c.SetReadDeadline(time.Now().Add(time.Second))
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(c, hdr); err != nil {
			t.Fatalf("unit %d: %v", unit, err)
		}
		if hdr[0] != byte(tid>>8) || hdr[1] != byte(tid) || hdr[6] != unit {
			t.Fatalf("header % X", hdr)
		}
		ans := make([]byte, int(hdr[5])-1)
		io.ReadFull(c, ans)
		return ans
	}
	if got := ask(7, 2, []byte{3, 0x10, 0x02, 0, 2}); !bytes.Equal(got, []byte{3, 4, 0, 0, 0, 2}) {
		t.Fatalf("unit 2 line/unit: % X", got)
	}
	if got := ask(8, 1, []byte{3, 0, 0x0E, 0, 1}); !bytes.Equal(got, []byte{3, 2, 0, 1}) { // parity code E
		t.Fatalf("parity code: % X", got)
	}
	if got := ask(9, 0xFF, []byte{3, 0x10, 0x03, 0, 1}); !bytes.Equal(got, []byte{3, 2, 0, 1}) { // 255 = first device
		t.Fatalf("unit 255: % X", got)
	}
	if got := ask(10, 9, []byte{3, 0, 0, 0, 1}); !bytes.Equal(got, []byte{0x83, 0x0B}) {
		t.Fatalf("foreign unit: % X", got)
	}
	l.SetFaults(Faults{Frag: 3, FragGap: time.Millisecond})
	if got := ask(11, 1, []byte{3, 0, 0, 0, 2}); !bytes.Equal(got, []byte{3, 4, 0, 1, 1, 0x20}) {
		t.Fatalf("fragmented: % X", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeTCP did not stop")
	}
	if s := l.Stats.Snapshot(); s.Ans != 5 || s.Foreign != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestSettingsCheck(t *testing.T) {
	ok := []Settings{
		{Transport: "rtu", Port: "/dev/ttyUSB0", Baud: 9600, Mode: "8e1"},
		{Transport: "tcp", Listen: "1502", Baud: 9600},
		{Transport: "tcp", Listen: "0.0.0.0:502", Baud: 19200, Mode: "8N1"},
	}
	for i := range ok {
		if err := ok[i].Check(); err != nil {
			t.Errorf("%+v: %v", ok[i], err)
		}
	}
	if s := ok[0]; s.Mode != "8E1" {
		t.Errorf("mode not normalised: %q", s.Mode)
	}
	if s := ok[1]; s.Listen != ":1502" {
		t.Errorf("listen not normalised: %q", s.Listen)
	}
	bad := []Settings{
		{Transport: "rtu", Port: "", Baud: 9600},
		{Transport: "rtu", Port: "/dev/x", Baud: 10},
		{Transport: "rtu", Port: "/dev/x", Baud: 9600, Mode: "9N1"},
		{Transport: "tcp", Listen: "", Baud: 9600},
		{Transport: "tcp", Listen: ":70000", Baud: 9600},
		{Transport: "udp", Port: "/dev/x", Baud: 9600},
	}
	for _, s := range bad {
		if err := s.Check(); err == nil {
			t.Errorf("%+v: no error", s)
		}
	}
}
