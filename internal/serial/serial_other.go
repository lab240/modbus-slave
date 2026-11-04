//go:build !linux && !windows

package serial

import (
	"errors"
	"runtime"
)

var errOS = errors.New("serial ports are supported on Linux and Windows only, not " + runtime.GOOS)

func Open(dev string, cfg Config) (Conn, error) { return nil, errOS }
func Resolve(spec string) string                { return spec }
func SetLatency(dev string, ms int)             {}
func OpenPty(link string) (Conn, error)         { return nil, errOS }
func List() []PortInfo                          { return nil }
