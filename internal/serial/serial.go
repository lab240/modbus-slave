// Package serial opens the line: a serial port (any baud rate, FT4232H channel lookup) or a pty for tests.
package serial

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrHangup means the device is gone (USB unplugged): the caller closes it and reopens later.
var ErrHangup = errors.New("port hung up (USB disconnect?)")

// Conn is an open line.
type Conn interface {
	// Wait waits up to timeout for input; false = nothing arrived.
	Wait(timeout time.Duration) (bool, error)
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	// Drain waits until everything written has left the UART.
	Drain() error
	Close() error
	Name() string
}

// Config is the character format of the line.
type Config struct {
	Baud     int
	DataBits int  // 7 or 8
	Parity   byte // 'N', 'E', 'O'
	StopBits int  // 1 or 2
}

// BitsPerChar is start + data + parity + stop bits.
func (c Config) BitsPerChar() int {
	n := 1 + c.DataBits + c.StopBits
	if c.Parity != 'N' {
		n++
	}
	return n
}

func (c Config) String() string {
	return fmt.Sprintf("%d %d%c%d", c.Baud, c.DataBits, c.Parity, c.StopBits)
}

// ParseMode parses "8N1", "8E1", "8O1", "7E1", "8N2" ...
func ParseMode(s string) (Config, error) {
	s = strings.ToUpper(s)
	if len(s) != 3 || (s[0] != '7' && s[0] != '8') || !strings.ContainsRune("NEO", rune(s[1])) || (s[2] != '1' && s[2] != '2') {
		return Config{}, fmt.Errorf("bad port mode %q (want e.g. 8N1, 8E1, 7E1, 8N2)", s)
	}
	return Config{DataBits: int(s[0] - '0'), Parity: s[1], StopBits: int(s[2] - '0')}, nil
}
