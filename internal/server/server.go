// Package server answers Modbus RTU requests on one line for a set of emulated devices.
package server

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/rtu"
	"github.com/lab240/modbus-slave/internal/serial"
)

// Faults are injected into the answers, to test the master side.
type Faults struct {
	Delay   time.Duration // answer delay
	Jitter  time.Duration // + random 0..Jitter
	Drop    float64       // probability of no answer
	Exc     float64       // probability of exception 4 instead of the answer
	BadCRC  float64       // probability of a broken CRC
	Frag    int           // send the answer in N pieces ...
	FragGap time.Duration // ... with this pause between them
	Echo    bool          // the transceiver hears itself: discard our own bytes after sending
}

// Stats are the counters of one line.
type Stats struct {
	Req, Ans, Foreign, Bcast, Resync, Exc, Drop, BadCRC atomic.Uint64
}

// Snapshot is a plain copy of Stats.
type Snapshot struct {
	Req, Ans, Foreign, Bcast, Resync, Exc, Drop, BadCRC uint64
}

func (s *Stats) Snapshot() Snapshot {
	return Snapshot{s.Req.Load(), s.Ans.Load(), s.Foreign.Load(), s.Bcast.Load(), s.Resync.Load(), s.Exc.Load(), s.Drop.Load(), s.BadCRC.Load()}
}

// Line is one serial line with its devices. The devices outlive a reopen of the port (USB re-plug):
// a sensor does not reboot when the master's adapter is re-plugged.
type Line struct {
	Index   int
	Spec    string // what the user gave: path, FT4232H channel, pty link
	Devices []*device.Device
	Faults  Faults
	Gap     time.Duration // silence that discards a partial frame
	Stats   Stats

	mu  sync.Mutex
	rnd *rand.Rand
	dev string // device in use right now, for the report
}

func NewLine(index int, spec string, devs []*device.Device, f Faults, gap time.Duration, seed uint64) *Line {
	return &Line{Index: index, Spec: spec, Devices: devs, Faults: f, Gap: gap,
		rnd: rand.New(rand.NewPCG(seed, uint64(index)+1))}
}

func (l *Line) Device() string     { l.mu.Lock(); defer l.mu.Unlock(); return l.dev }
func (l *Line) setDevice(d string) { l.mu.Lock(); l.dev = d; l.mu.Unlock() }

func (l *Line) chance(p float64) bool {
	if p <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rnd.Float64() < p
}

func (l *Line) jitter() time.Duration {
	if l.Faults.Jitter <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Duration(l.rnd.Int64N(int64(l.Faults.Jitter)))
}

func (l *Line) find(unit byte) *device.Device {
	for _, d := range l.Devices {
		if d.Unit() == unit {
			return d
		}
	}
	return nil
}

// Serve answers requests on c until ctx is done or the port fails (the error is returned, c is not closed).
// Framing goes by the request length of the function code, so an answer can start right after the
// last byte; silence (Gap) only throws away a partial frame.
func (l *Line) Serve(ctx context.Context, c serial.Conn) error {
	l.setDevice(c.Name())
	buf := make([]byte, 0, 512)
	rd := make([]byte, 512)
	var last time.Time
	for ctx.Err() == nil {
		timeout := 200 * time.Millisecond
		if len(buf) > 0 {
			timeout = l.Gap
		}
		ok, err := c.Wait(timeout)
		if err != nil {
			return err
		}
		now := time.Now()
		var frame []byte
		if ok {
			n, err := c.Read(rd)
			if err != nil {
				return err
			}
			buf = append(buf, rd[:n]...)
			last = now
		}
		silence := !ok && len(buf) > 0 && now.Sub(last) >= l.Gap
		if silence && rtu.RequestLen(buf) == 0 && rtu.CheckCRC(buf) {
			// an unknown function code with a valid CRC: answer exception 1
			frame = append([]byte(nil), buf...)
			buf = buf[:0]
		}
		for len(buf) > 0 || frame != nil {
			if frame == nil {
				n := rtu.RequestLen(buf)
				if n <= 0 || len(buf) < n {
					if !silence {
						break // wait for the rest
					}
					// silence and no whole request here: skip a byte and look for one further on
					// (garbage or a broken frame in front of a good request must not swallow it)
					l.Stats.Resync.Add(1)
					buf = append(buf[:0], buf[1:]...)
					continue
				}
				if !rtu.CheckCRC(buf[:n]) { // lost sync or line noise: drop a byte, look again
					l.Stats.Resync.Add(1)
					buf = append(buf[:0], buf[1:]...)
					continue
				}
				frame = append([]byte(nil), buf[:n]...)
				buf = append(buf[:0], buf[n:]...)
			}
			if err := l.answer(c, frame); err != nil {
				return err
			}
			frame = nil
		}
	}
	return ctx.Err()
}

func (l *Line) answer(c serial.Conn, frame []byte) error {
	unit, pdu := frame[0], frame[1:len(frame)-2]
	if unit == 0 { // broadcast: writes are executed, nobody answers
		l.Stats.Bcast.Add(1)
		if pdu[0] == 6 || pdu[0] == 16 {
			for _, d := range l.Devices {
				if d.Supports(pdu[0]) {
					d.Handle(pdu)
				}
			}
		}
		return nil
	}
	d := l.find(unit)
	if d == nil {
		l.Stats.Foreign.Add(1)
		return nil
	}
	l.Stats.Req.Add(1)
	d.Served.Add(1)
	if l.chance(l.Faults.Drop) {
		l.Stats.Drop.Add(1)
		return nil
	}
	var ans []byte
	if l.chance(l.Faults.Exc) {
		ans = rtu.Exception(pdu[0], rtu.ExcDeviceFailure)
	} else {
		ans = d.Handle(pdu)
	}
	if ans[0]&0x80 != 0 {
		l.Stats.Exc.Add(1)
	}
	out := rtu.AppendCRC(append([]byte{unit}, ans...))
	if l.chance(l.Faults.BadCRC) {
		out[len(out)-1] ^= 0x5A
		out[len(out)-2] ^= 0x5A
		l.Stats.BadCRC.Add(1)
	}
	if d := l.Faults.Delay + l.jitter(); d > 0 {
		time.Sleep(d)
	}
	if err := l.send(c, out); err != nil {
		return err
	}
	l.Stats.Ans.Add(1)
	return nil
}

func (l *Line) send(c serial.Conn, out []byte) error {
	if f := l.Faults.Frag; f > 1 { // answer in pieces with a pause: tests the master's t3.5 handling
		step := (len(out) + f - 1) / f
		for i := 0; i < len(out); i += step {
			if _, err := c.Write(out[i:min(i+step, len(out))]); err != nil {
				return err
			}
			c.Drain()
			time.Sleep(l.Faults.FragGap)
		}
	} else if _, err := c.Write(out); err != nil {
		return err
	}
	if l.Faults.Echo { // eat our own bytes
		c.Drain()
		need, end, rd := len(out), time.Now().Add(500*time.Millisecond), make([]byte, len(out))
		for need > 0 && time.Now().Before(end) {
			if ok, err := c.Wait(time.Until(end)); err != nil {
				return err
			} else if ok {
				n, err := c.Read(rd[:need])
				if err != nil {
					return err
				}
				need -= n
			}
		}
	}
	return nil
}
