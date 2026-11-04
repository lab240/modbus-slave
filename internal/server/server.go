// Package server answers Modbus requests on one line for a set of emulated devices:
// Modbus RTU on a serial port (Serve) or Modbus TCP (ServeTCP).
package server

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/rtu"
	"github.com/lab240/modbus-slave/internal/serial"
)

// Faults are injected into the answers, to test the master side.
// They can be changed while the line runs (Line.SetFaults).
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
	Spec    string // what the user gave at start: path, FT4232H channel, pty link, TCP address
	Devices []*device.Device
	faults  atomic.Pointer[Faults]
	Gap     time.Duration // silence that discards a partial frame; 0 = t3.5 of the baud rate
	Stats   Stats

	unitFaults atomic.Pointer[map[byte]Faults] // per-unit faults instead of the line's ones (a "crazy" sensor)
	settings   atomic.Pointer[Settings]
	changed    chan struct{} // a new Settings: the serving loop reopens the line

	mu      sync.Mutex
	rnd     *rand.Rand
	dev     string // device in use right now, for the report
	lastErr string // why the line is not open
	open    atomic.Bool
}

// Open reports whether the port is open and served right now.
func (l *Line) Open() bool { return l.open.Load() }

func NewLine(index int, spec string, devs []*device.Device, f Faults, gap time.Duration, seed uint64) *Line {
	l := &Line{Index: index, Spec: spec, Devices: devs, Gap: gap, rnd: rand.New(rand.NewPCG(seed, uint64(index)+1)),
		changed: make(chan struct{}, 1)}
	l.settings.Store(&Settings{Transport: "rtu", Port: spec, Baud: 19200, Mode: "8N1"})
	l.SetFaults(f)
	return l
}

// Faults returns the current fault settings.
func (l *Line) Faults() Faults { return *l.faults.Load() }

// SetFaults changes the fault settings; the next request uses them.
func (l *Line) SetFaults(f Faults) { l.faults.Store(&f) }

// SetUnitFaults gives some units their own faults; the others keep the line's. nil = none.
func (l *Line) SetUnitFaults(m map[byte]Faults) { l.unitFaults.Store(&m) }

// FaultsFor is what applies to a request for unit.
func (l *Line) FaultsFor(unit byte) Faults {
	if m := l.unitFaults.Load(); m != nil && *m != nil {
		if f, ok := (*m)[unit]; ok {
			return f
		}
	}
	return l.Faults()
}

func (l *Line) Device() string     { l.mu.Lock(); defer l.mu.Unlock(); return l.dev }
func (l *Line) setDevice(d string) { l.mu.Lock(); l.dev = d; l.mu.Unlock() }

// Settings is how the line is served now.
func (l *Line) Settings() Settings { return *l.settings.Load() }

// InitSettings sets the settings before the line is served.
func (l *Line) InitSettings(s Settings) error {
	if err := s.Check(); err != nil {
		return err
	}
	l.settings.Store(&s)
	return nil
}

// SetSettings changes the settings of a running line: it is closed and opened again with them.
// A pty line keeps its port.
func (l *Line) SetSettings(s Settings) (Settings, error) {
	old := l.Settings()
	s.Pty = old.Pty
	if old.Pty && s.Transport != "tcp" && s.Port != old.Port {
		return old, fmt.Errorf("line %d is a pty (%s): its port cannot be changed", l.Index, old.Port)
	}
	if err := s.Check(); err != nil {
		return old, err
	}
	l.settings.Store(&s)
	select {
	case l.changed <- struct{}{}:
	default:
	}
	return s, nil
}

// Changed is signalled when SetSettings was called.
func (l *Line) Changed() <-chan struct{} { return l.changed }

// Err is why the line is not open now ("" when it is, or before the first attempt).
func (l *Line) Err() string        { l.mu.Lock(); defer l.mu.Unlock(); return l.lastErr }
func (l *Line) SetErr(e string)    { l.mu.Lock(); l.lastErr = e; l.mu.Unlock() }
func (l *Line) SetDevice(d string) { l.setDevice(d) }

// gap is the silence that ends a partial frame.
func (l *Line) gap() time.Duration {
	if l.Gap > 0 {
		return l.Gap
	}
	s := l.Settings()
	c, err := s.Serial()
	if err != nil {
		return 20 * time.Millisecond
	}
	return rtu.T35(c.Baud, c.BitsPerChar())
}

func (l *Line) chance(p float64) bool {
	if p <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rnd.Float64() < p
}

func (l *Line) jitter(j time.Duration) time.Duration {
	if j <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Duration(l.rnd.Int64N(int64(j)))
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
	l.open.Store(true)
	defer l.open.Store(false)
	gap := l.gap()
	buf := make([]byte, 0, 512)
	rd := make([]byte, 512)
	var last time.Time
	for ctx.Err() == nil {
		timeout := 200 * time.Millisecond
		if len(buf) > 0 {
			timeout = gap
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
		silence := !ok && len(buf) > 0 && now.Sub(last) >= gap
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

// handle runs one request for the unit; nil = no answer (broadcast, a unit not on this line, an injected drop).
// tcp: a unit that is not here gets exception 0x0B, as from a gateway, instead of silence.
func (l *Line) handle(unit byte, pdu []byte, tcp bool) []byte {
	if unit == 0 && !tcp { // broadcast: writes are executed, nobody answers
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
	if d == nil && tcp && (unit == 0 || unit == 0xFF) && len(l.Devices) > 0 {
		d = l.Devices[0] // Modbus TCP: unit 0 / 255 = "the server itself"
	}
	if d == nil {
		l.Stats.Foreign.Add(1)
		if tcp {
			l.Stats.Exc.Add(1)
			return rtu.Exception(pdu[0], 0x0B) // gateway target device failed to respond
		}
		return nil
	}
	l.Stats.Req.Add(1)
	d.Served.Add(1)
	d.Last.Store(time.Now().UnixNano())
	f := l.FaultsFor(unit)
	if l.chance(f.Drop) {
		l.Stats.Drop.Add(1)
		return nil
	}
	var ans []byte
	if l.chance(f.Exc) {
		ans = rtu.Exception(pdu[0], rtu.ExcDeviceFailure)
	} else {
		ans = d.Handle(pdu)
	}
	if ans[0]&0x80 != 0 {
		l.Stats.Exc.Add(1)
	}
	return ans
}

func (l *Line) answer(c serial.Conn, frame []byte) error {
	unit, pdu := frame[0], frame[1:len(frame)-2]
	ans := l.handle(unit, pdu, false)
	if ans == nil {
		return nil
	}
	f := l.FaultsFor(unit)
	f.Echo = l.Faults().Echo // the echo is the transceiver's, not the device's
	out := rtu.AppendCRC(append([]byte{unit}, ans...))
	if l.chance(f.BadCRC) {
		out[len(out)-1] ^= 0x5A
		out[len(out)-2] ^= 0x5A
		l.Stats.BadCRC.Add(1)
	}
	if d := f.Delay + l.jitter(f.Jitter); d > 0 {
		time.Sleep(d)
	}
	if err := l.send(c, out, f); err != nil {
		return err
	}
	l.Stats.Ans.Add(1)
	return nil
}

func (l *Line) send(c serial.Conn, out []byte, fl Faults) error {
	if f := fl.Frag; f > 1 { // answer in pieces with a pause: tests the master's t3.5 handling
		step := (len(out) + f - 1) / f
		for i := 0; i < len(out); i += step {
			if _, err := c.Write(out[i:min(i+step, len(out))]); err != nil {
				return err
			}
			c.Drain()
			time.Sleep(fl.FragGap)
		}
	} else if _, err := c.Write(out); err != nil {
		return err
	}
	if fl.Echo { // eat our own bytes
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
