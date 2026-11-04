package server

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/lab240/modbus-slave/internal/serial"
)

// Settings say how a line reaches the master: a serial port (Modbus RTU) or a TCP port (Modbus TCP).
// The serial fields are kept while the line is on TCP, so switching back restores them.
type Settings struct {
	Transport string `json:"transport"`     // "rtu" | "tcp"
	Port      string `json:"port"`          // rtu: /dev/ttyUSB0, an FT4232H channel 0..3 / A..D, or the pty link
	Baud      int    `json:"baud"`          // rtu
	Mode      string `json:"mode"`          // rtu: 8N1, 8E1, 8O1, 7E1, 8N2 ...
	Listen    string `json:"listen"`        // tcp: ":502", "0.0.0.0:1502", "127.0.0.1:502"
	Pty       bool   `json:"pty,omitempty"` // a pty line (tests): the port cannot be changed
}

// Serial is the character format of the serial port.
func (s Settings) Serial() (serial.Config, error) {
	c, err := serial.ParseMode(s.Mode)
	c.Baud = s.Baud
	return c, err
}

// Where is the endpoint in use: the port or the TCP address.
func (s Settings) Where() string {
	if s.Transport == "tcp" {
		return "tcp " + s.Listen
	}
	return s.Port
}

func (s Settings) String() string {
	if s.Transport == "tcp" {
		return "Modbus TCP on " + s.Listen
	}
	return fmt.Sprintf("Modbus RTU on %s %d %s", s.Port, s.Baud, strings.ToUpper(s.Mode))
}

// Check validates the settings; it normalises the mode and a bare TCP port ("502" -> ":502").
func (s *Settings) Check() error {
	switch s.Transport {
	case "rtu", "tcp":
	case "":
		s.Transport = "rtu"
	default:
		return fmt.Errorf("transport %q: want rtu or tcp", s.Transport)
	}
	s.Mode = strings.ToUpper(s.Mode)
	if s.Mode == "" {
		s.Mode = "8N1"
	}
	if _, err := serial.ParseMode(s.Mode); err != nil {
		return err
	}
	if s.Baud < 300 || s.Baud > 4000000 {
		return fmt.Errorf("baud %d: want 300..4000000", s.Baud)
	}
	if s.Transport == "rtu" && strings.TrimSpace(s.Port) == "" {
		return fmt.Errorf("port: give a serial port")
	}
	if s.Listen != "" {
		if _, err := strconv.Atoi(s.Listen); err == nil {
			s.Listen = ":" + s.Listen
		}
		host, port, err := net.SplitHostPort(s.Listen)
		if err != nil {
			return fmt.Errorf("listen %q: want :502 or 0.0.0.0:502", s.Listen)
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("listen %q: port 1..65535", s.Listen)
		}
		if host != "" && net.ParseIP(host) == nil && host != "localhost" {
			return fmt.Errorf("listen %q: host must be an IP address", s.Listen)
		}
	}
	if s.Transport == "tcp" && s.Listen == "" {
		return fmt.Errorf("listen: give a TCP port, e.g. :502")
	}
	return nil
}
