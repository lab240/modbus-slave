//go:build linux

package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/rtu"
	"github.com/lab240/modbus-slave/internal/serial"
)

// End to end over a pty: the emulator serves the master end, the test is the Modbus master on the link.
func TestServePty(t *testing.T) {
	link := filepath.Join(t.TempDir(), "ttyEMU")
	c, err := serial.OpenPty(link)
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer c.Close()
	l := NewLine(1, link, []*device.Device{device.NewKTH2R(1, 1, 19200, 'N'), device.NewKTH2R(2, 1, 19200, 'N')}, Faults{}, 20*time.Millisecond, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Serve(ctx, c)

	m, err := os.OpenFile(link, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	readAns := func(req []byte, n int) []byte {
		t.Helper()
		m.SetReadDeadline(time.Now().Add(time.Second))
		ans := make([]byte, 0, n)
		buf := make([]byte, 256)
		for len(ans) < n {
			k, err := m.Read(buf)
			if err != nil {
				t.Fatalf("% X: read %d of %d: %v", req, len(ans), n, err)
			}
			ans = append(ans, buf[:k]...)
		}
		if !rtu.CheckCRC(ans) {
			t.Fatalf("bad CRC in % X", ans)
		}
		return ans[:len(ans)-2]
	}
	ask := func(req []byte, n int) []byte {
		t.Helper()
		m.Write(rtu.AppendCRC(req))
		return readAns(req, n)
	}
	if got := ask([]byte{2, 3, 0x10, 0x02, 0, 2}, 9); !bytes.Equal(got, []byte{2, 3, 4, 0, 1, 0, 2}) {
		t.Fatalf("unit 2 line/unit: % X", got)
	}
	// a request in two pieces (5 ms apart, below the gap): still answered
	req := rtu.AppendCRC([]byte{1, 3, 0, 0, 0, 1})
	m.Write(req[:3])
	time.Sleep(5 * time.Millisecond)
	m.Write(req[3:])
	if got := readAns(req, 7); !bytes.Equal(got, []byte{1, 3, 2, 0, 1}) {
		t.Fatalf("split request: % X", got)
	}
	// garbage, a pause, then a good request
	time.Sleep(50 * time.Millisecond)
	m.Write([]byte{0xFF, 0x00, 0x13})
	if got := ask([]byte{1, 3, 0, 1, 0, 1}, 7); !bytes.Equal(got, []byte{1, 3, 2, 0x01, 0x20}) {
		t.Fatalf("after garbage: % X", got)
	}
	// foreign unit: no answer
	m.Write(rtu.AppendCRC([]byte{7, 3, 0, 0, 0, 1}))
	m.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _ := m.Read(make([]byte, 16)); n != 0 {
		t.Fatalf("answer to a foreign unit")
	}
	s := l.Stats.Snapshot()
	if s.Foreign != 1 || s.Ans < 2 {
		t.Fatalf("stats %+v", s)
	}
}
