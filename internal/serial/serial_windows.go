package serial

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: COM ports through the Win32 API (non-overlapped I/O). Wait does a ReadFile with a timeout
// and keeps what it got for the next Read.

type comConn struct {
	h       windows.Handle
	name    string
	pending []byte
	buf     [4096]byte
	tmo     uint32 // the read timeout set now, ms (0 = not set yet)
}

func (c *comConn) Name() string { return c.name }

func (c *comConn) setReadTimeout(ms uint32) error {
	if ms == c.tmo {
		return nil
	}
	// MAXDWORD/MAXDWORD/ms: return at once when bytes are there, else wait up to ms for the first one
	t := windows.CommTimeouts{ReadIntervalTimeout: 0xFFFFFFFF, ReadTotalTimeoutMultiplier: 0xFFFFFFFF, ReadTotalTimeoutConstant: ms}
	if err := windows.SetCommTimeouts(c.h, &t); err != nil {
		return fmt.Errorf("SetCommTimeouts: %w", err)
	}
	c.tmo = ms
	return nil
}

func (c *comConn) Wait(timeout time.Duration) (bool, error) {
	if len(c.pending) > 0 {
		return true, nil
	}
	ms := uint32(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	if err := c.setReadTimeout(ms); err != nil {
		return false, err
	}
	var n uint32
	if err := windows.ReadFile(c.h, c.buf[:], &n, nil); err != nil {
		return false, fmt.Errorf("%w: %v", ErrHangup, err) // a USB adapter that was pulled out
	}
	if n == 0 {
		return false, nil
	}
	c.pending = append(c.pending[:0], c.buf[:n]...)
	return true, nil
}

func (c *comConn) Read(p []byte) (int, error) {
	if len(c.pending) == 0 {
		if ok, err := c.Wait(time.Millisecond); err != nil || !ok {
			return 0, err
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *comConn) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		var n uint32
		if err := windows.WriteFile(c.h, p[done:], &n, nil); err != nil {
			return done, fmt.Errorf("%w: %v", ErrHangup, err)
		}
		done += int(n)
	}
	return done, nil
}

func (c *comConn) Drain() error { return windows.FlushFileBuffers(c.h) }

func (c *comConn) Close() error { return windows.CloseHandle(c.h) }

// Open opens a COM port ("COM3", "\\.\COM10") in binary mode.
func Open(dev string, cfg Config) (Conn, error) {
	path := dev
	if !strings.HasPrefix(path, `\\.\`) {
		path = `\\.\` + path // needed for COM10 and above, harmless below
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dev, err)
	}
	var dcb windows.DCB
	dcb.DCBlength = uint32(unsafe.Sizeof(dcb)) // 28, as sizeof(DCB) in the Win32 API
	if err := windows.GetCommState(h, &dcb); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("GetCommState: %w", err)
	}
	dcb.BaudRate = uint32(cfg.Baud)
	dcb.ByteSize = uint8(cfg.DataBits)
	dcb.Flags = 0x0001 | 0x0010 | 0x1000 // fBinary, DTR on, RTS on (auto-direction RS-485 adapters ignore them)
	switch cfg.Parity {
	case 'E':
		dcb.Parity = windows.EVENPARITY
		dcb.Flags |= 0x0002 // fParity
	case 'O':
		dcb.Parity = windows.ODDPARITY
		dcb.Flags |= 0x0002
	default:
		dcb.Parity = windows.NOPARITY
	}
	if cfg.StopBits == 2 {
		dcb.StopBits = windows.TWOSTOPBITS
	} else {
		dcb.StopBits = windows.ONESTOPBIT
	}
	if err := windows.SetCommState(h, &dcb); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetCommState %s: %w", cfg, err)
	}
	windows.PurgeComm(h, windows.PURGE_RXCLEAR|windows.PURGE_TXCLEAR|windows.PURGE_RXABORT|windows.PURGE_TXABORT)
	return &comConn{h: h, name: dev}, nil
}

// Resolve: on Windows a port is its name (COM3); FT4232H channel numbers need sysfs (Linux only).
func Resolve(spec string) string {
	u := strings.ToUpper(spec)
	if strings.HasPrefix(u, "COM") || strings.HasPrefix(spec, `\\.\`) {
		return spec
	}
	return ""
}

// SetLatency: on Windows the FTDI latency timer is set in the driver (Device Manager -> port -> Advanced).
func SetLatency(dev string, ms int) {}

// OpenPty is not available on Windows (use a com0com pair instead).
func OpenPty(link string) (Conn, error) {
	return nil, fmt.Errorf("--pty is not available on Windows; use a virtual COM pair (com0com)")
}
