// Package fleet is the set of emulated sensors. Every sensor has its own template (profile), address,
// connection (Modbus RTU on a serial port or Modbus TCP) and mode (normal / crazy). Sensors with the same
// endpoint share one line: one serial port = one RS-485 bus, one TCP port = one server.
// Changing a sensor restarts only the lines it touches; the devices keep their state.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/profile"
	"github.com/lab240/modbus-slave/internal/serial"
	"github.com/lab240/modbus-slave/internal/server"
	"github.com/lab240/modbus-slave/internal/source"
)

// Generic is the template name of the v1 device: N holding registers, random or from a file.
const Generic = "generic"

// Sensor is one emulated device, as the user sets it up.
type Sensor struct {
	ID        int    `json:"id"`
	Name      string `json:"name,omitempty"`
	Profile   string `json:"profile"` // generic, a built-in profile, or a YAML file
	Unit      int    `json:"unit"`
	Transport string `json:"transport"` // rtu | tcp
	Port      string `json:"port,omitempty"`
	Baud      int    `json:"baud,omitempty"`
	Mode      string `json:"mode,omitempty"`    // 8N1 ...
	Listen    string `json:"listen,omitempty"`  // tcp: ":502"
	Crazy     bool   `json:"crazy"`             // answers with random faults
	Stopped   bool   `json:"stopped,omitempty"` // not on the bus: does not answer
	File      string `json:"file,omitempty"`    // generic: register values from this file
	Pty       bool   `json:"pty,omitempty"`     // the port is a pty link made by us (tests)
	// Line is the bus number the template sees ({line} in file paths, line_step, the "line" register);
	// sensors on one port share it. Set by the fleet when a sensor is added.
	Line int `json:"line"`
}

func (s *Sensor) settings() server.Settings {
	return server.Settings{Transport: s.Transport, Port: s.Port, Baud: s.Baud, Mode: s.Mode, Listen: s.Listen, Pty: s.Pty}
}

// key is the endpoint: sensors with the same key share a line.
func (s *Sensor) key() string {
	if s.Transport == "tcp" {
		_, p, _ := net.SplitHostPort(s.Listen)
		return "tcp:" + p
	}
	return "rtu:" + s.Port
}

// normalize fills defaults and checks the fields of one sensor.
func (s *Sensor) normalize() error {
	if s.Profile == "" {
		s.Profile = Generic
	}
	if s.Unit < 1 || s.Unit > 247 {
		return fmt.Errorf("address %d: want 1..247", s.Unit)
	}
	if s.Baud == 0 {
		s.Baud = 9600
	}
	if s.Listen == "" {
		s.Listen = ":502"
	}
	st := s.settings()
	if err := st.Check(); err != nil {
		return err
	}
	s.Transport, s.Mode, s.Listen = st.Transport, st.Mode, st.Listen
	return nil
}

// Options are the same for all sensors.
type Options struct {
	Normal      server.Faults // the normal mode (from the command line; usually no faults)
	Crazy       server.Faults // the crazy mode
	Gap         time.Duration // 0 = t3.5 of the baud rate
	Latency     int           // FTDI latency_timer, ms
	Regs        int           // generic: number of registers
	MaxAge      time.Duration // generic: max age of a file
	Seed        uint64
	ProfileDirs []string // more templates: *.yaml in these directories
}

// DefaultCrazy is the crazy mode unless set otherwise: slow and unreliable, but still mostly answering.
var DefaultCrazy = server.Faults{Delay: 20 * time.Millisecond, Jitter: 400 * time.Millisecond,
	Drop: 0.10, Exc: 0.05, BadCRC: 0.05, Frag: 0, FragGap: 5 * time.Millisecond}

type sensorRT struct {
	cfg   atomic.Pointer[Sensor]
	dev   *device.Device
	prof  *profile.Profile // nil = generic
	built string           // profile|unit|file the device was built from
}

type runLine struct {
	l      *server.Line
	set    server.Settings
	devs   []*device.Device
	ids    []int
	cancel context.CancelFunc
	done   chan struct{}
}

// Fleet runs the sensors.
type Fleet struct {
	opt   Options
	ctx   context.Context
	start time.Time
	files *profile.Files

	mu      sync.Mutex
	order   []int // sensor ids as shown
	rt      map[int]*sensorRT
	lines   map[string]*runLine
	nextID  int
	lineSeq int
	crazy   server.Faults
	stopped bool // the whole emulation is stopped (the API keeps running)
	// Save is called after every change (to keep the sensors over a restart); may be nil.
	Save func(sensors []Sensor, crazy server.Faults, stopped bool) error
}

// New starts the sensors; they run until ctx is done.
func New(ctx context.Context, opt Options, sensors []Sensor) (*Fleet, error) {
	return NewStopped(ctx, opt, sensors, false)
}

// NewStopped is New with the whole emulation stopped or running from the start.
func NewStopped(ctx context.Context, opt Options, sensors []Sensor, stopped bool) (*Fleet, error) {
	if opt.Regs == 0 {
		opt.Regs = 20
	}
	f := &Fleet{opt: opt, ctx: ctx, start: time.Now(), files: &profile.Files{}, rt: map[int]*sensorRT{},
		lines: map[string]*runLine{}, nextID: 1, crazy: opt.Crazy, stopped: stopped}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range sensors {
		if s.ID == 0 || f.rt[s.ID] != nil {
			s.ID = f.nextID
		}
		if s.ID >= f.nextID {
			f.nextID = s.ID + 1
		}
		if err := f.put(s); err != nil {
			return nil, fmt.Errorf("sensor %d: %w", s.ID, err)
		}
	}
	if err := f.check(); err != nil {
		return nil, err
	}
	f.apply()
	return f, nil
}

// put adds or replaces a sensor (mu held); the device is rebuilt only when its template, address or file changed.
func (f *Fleet) put(s Sensor) error {
	if err := s.normalize(); err != nil {
		return err
	}
	r := f.rt[s.ID]
	if r == nil {
		r = &sensorRT{}
		f.rt[s.ID] = r
		f.order = append(f.order, s.ID)
	}
	key := fmt.Sprintf("%s|%d|%s|%d", s.Profile, s.Unit, s.File, s.Line)
	if r.dev == nil || r.built != key {
		var prof *profile.Profile
		if s.Profile != Generic {
			p, err := f.loadProfile(s.Profile)
			if err != nil {
				return err
			}
			prof = p
		}
		r.prof, r.built = prof, key
		r.dev = f.build(r, s)
	}
	cp := s
	r.cfg.Store(&cp)
	return nil
}

func (f *Fleet) build(r *sensorRT, s Sensor) *device.Device {
	unit := byte(s.Unit)
	if r.prof == nil {
		var src source.Block = source.NewRandom(f.opt.Seed + uint64(s.ID)*1000 + uint64(unit))
		if s.File != "" {
			src = &source.File{Path: s.File, MaxAge: f.opt.MaxAge}
		}
		return device.NewGeneric(unit, f.opt.Regs, src)
	}
	env := profile.Env{Line: s.Line, Unit: unit, Seed: f.opt.Seed, Start: f.start, Files: f.files,
		Serial: func() (int, byte) { // speed_code, parity_code follow the sensor's settings
			c, _ := r.cfg.Load().settings().Serial()
			return c.Baud, c.Parity
		}}
	return profile.Build(r.prof, env)
}

// check: one address once per endpoint, one character format per serial port, a pty keeps its port.
func (f *Fleet) check() error {
	type ep struct {
		units map[int]int
		set   server.Settings
		id    int
	}
	eps := map[string]*ep{}
	for _, id := range f.order {
		s := f.rt[id].cfg.Load()
		if s.Stopped {
			continue
		}
		e := eps[s.key()]
		if e == nil {
			e = &ep{units: map[int]int{}, set: s.settings(), id: id}
			eps[s.key()] = e
		}
		if other, dup := e.units[s.Unit]; dup {
			return fmt.Errorf("sensors %d and %d have the same address %d on %s", other, id, s.Unit, s.settings().Where())
		}
		e.units[s.Unit] = id
		if s.Transport == "rtu" && (s.Baud != e.set.Baud || s.Mode != e.set.Mode) {
			return fmt.Errorf("sensors %d and %d are on one port %s with different speed/parity", e.id, id, s.Port)
		}
	}
	return nil
}

// apply starts, restarts and stops the lines to match the sensors (mu held).
func (f *Fleet) apply() {
	groups := map[string][]int{}
	var keys []string
	for _, id := range f.order {
		s := f.rt[id].cfg.Load()
		if s.Stopped || f.stopped {
			continue
		}
		k := s.key()
		if groups[k] == nil {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], id)
	}
	for k, rl := range f.lines {
		if groups[k] == nil {
			f.stop(k, rl)
		}
	}
	for _, k := range keys {
		ids := groups[k]
		first := f.rt[ids[0]].cfg.Load()
		set := first.settings()
		devs := make([]*device.Device, len(ids))
		uf := map[byte]server.Faults{}
		for i, id := range ids {
			r := f.rt[id]
			devs[i] = r.dev
			if r.cfg.Load().Crazy {
				uf[r.dev.Unit()] = f.crazy
			}
		}
		if rl := f.lines[k]; rl != nil {
			if rl.set == set && sameDevs(rl.devs, devs) {
				rl.ids = ids
				rl.l.SetUnitFaults(uf)
				continue
			}
			f.stop(k, rl)
		}
		l := server.NewLine(f.lineSeq, set.Where(), devs, f.opt.Normal, f.opt.Gap, f.opt.Seed)
		f.lineSeq++
		l.InitSettings(set)
		l.SetUnitFaults(uf)
		ctx, cancel := context.WithCancel(f.ctx)
		rl := &runLine{l: l, set: set, devs: devs, ids: ids, cancel: cancel, done: make(chan struct{})}
		f.lines[k] = rl
		go func() { defer close(rl.done); keepServing(ctx, l, f.opt.Latency) }()
	}
}

func (f *Fleet) stop(k string, rl *runLine) {
	rl.cancel()
	<-rl.done // the port is closed before it is opened again
	delete(f.lines, k)
}

func sameDevs(a, b []*device.Device) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// change runs fn on a copy of the sensors; if the result is valid it is applied and saved, else nothing changes.
func (f *Fleet) change(fn func() error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	type devState struct {
		dev   *device.Device
		prof  *profile.Profile
		built string
	}
	saved := map[int]Sensor{}
	savedDev := map[int]devState{}
	for id, r := range f.rt {
		saved[id] = *r.cfg.Load()
		savedDev[id] = devState{r.dev, r.prof, r.built}
	}
	savedOrder := append([]int(nil), f.order...)
	err := fn()
	if err == nil {
		err = f.check()
	}
	if err != nil { // roll back
		f.order = savedOrder
		for id := range f.rt {
			if _, ok := saved[id]; !ok {
				delete(f.rt, id)
			}
		}
		for id, s := range saved {
			r := f.rt[id]
			sd := savedDev[id]
			r.dev, r.prof, r.built = sd.dev, sd.prof, sd.built
			cp := s
			r.cfg.Store(&cp)
		}
		return err
	}
	f.apply()
	f.save()
	return nil
}

func (f *Fleet) save() {
	if f.Save == nil {
		return
	}
	if err := f.Save(f.list(), f.crazy, f.stopped); err != nil {
		log.Printf("saving the sensors: %v", err)
	}
}

// Add adds a sensor; the id is given by the fleet.
func (f *Fleet) Add(s Sensor) (Sensor, error) {
	err := f.change(func() error {
		s.ID = 1 // the smallest free number: after removing every sensor a new one is 1 again
		for f.rt[s.ID] != nil {
			s.ID++
		}
		if err := s.normalize(); err != nil {
			return err
		}
		s.Line = -1
		top := -1
		for _, id := range f.order { // the bus number: that of a sensor on the same port, or a new one
			o := f.rt[id].cfg.Load()
			if o.key() == s.key() {
				s.Line = o.Line
			}
			top = max(top, o.Line)
		}
		if s.Line < 0 {
			s.Line = top + 1
		}
		return f.put(s)
	})
	if err == nil {
		log.Printf("sensor %d added: %s", s.ID, describe(f.Get(s.ID)))
	}
	return f.Get(s.ID), err
}

// Update changes a sensor. A new speed / parity is given to every sensor on the same serial port
// (it is one bus); the ids of those are returned.
func (f *Fleet) Update(id int, fn func(*Sensor)) (Sensor, []int, error) {
	var also []int
	err := f.change(func() error {
		r := f.rt[id]
		if r == nil {
			return ErrNotFound
		}
		old := *r.cfg.Load()
		s := old
		fn(&s)
		s.ID, s.Pty = id, old.Pty
		if old.Pty && s.Transport == "rtu" && s.Port != old.Port {
			return fmt.Errorf("sensor %d is on a pty: its port cannot be changed", id)
		}
		if err := f.put(s); err != nil {
			return err
		}
		ns := *f.rt[id].cfg.Load()
		if ns.Transport != "rtu" {
			return nil
		}
		joined := ns.Port != old.Port || old.Transport != "rtu"
		for _, oid := range f.order {
			o := *f.rt[oid].cfg.Load()
			if oid == id || o.Transport != "rtu" || o.Port != ns.Port || (o.Baud == ns.Baud && o.Mode == ns.Mode) {
				continue
			}
			if joined { // moved onto a bus that is already there: take its speed / parity
				ns.Baud, ns.Mode = o.Baud, o.Mode
				return f.put(ns)
			}
			o.Baud, o.Mode = ns.Baud, ns.Mode // a new speed / parity of the bus: for every sensor on it
			if err := f.put(o); err != nil {
				return err
			}
			also = append(also, oid)
		}
		return nil
	})
	if err == nil {
		log.Printf("sensor %d: %s", id, describe(f.Get(id)))
	}
	return f.Get(id), also, err
}

// Remove deletes a sensor.
func (f *Fleet) Remove(id int) error {
	err := f.change(func() error {
		if f.rt[id] == nil {
			return ErrNotFound
		}
		delete(f.rt, id)
		for i, x := range f.order {
			if x == id {
				f.order = append(f.order[:i], f.order[i+1:]...)
				break
			}
		}
		return nil
	})
	if err == nil {
		log.Printf("sensor %d removed", id)
	}
	return err
}

// ErrNotFound: no sensor with this id.
var ErrNotFound = errors.New("no such sensor")

func describe(s Sensor) string {
	m := "normal"
	if s.Crazy {
		m = "crazy"
	}
	if s.Stopped {
		m += ", stopped"
	}
	return fmt.Sprintf("%s, address %d, %s, %s", s.Profile, s.Unit, s.settings(), m)
}

// List is the sensors in order. An address the master changed over Modbus is taken over.
func (f *Fleet) List() []Sensor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list()
}

func (f *Fleet) list() []Sensor {
	out := make([]Sensor, 0, len(f.order))
	for _, id := range f.order {
		r := f.rt[id]
		s := *r.cfg.Load()
		if u := int(r.dev.Unit()); u != s.Unit { // FC 6 to the address register
			s.Unit = u
			r.built = fmt.Sprintf("%s|%d|%s|%d", s.Profile, s.Unit, s.File, s.Line)
			cp := s
			r.cfg.Store(&cp)
		}
		out = append(out, s)
	}
	return out
}

// Get is one sensor (zero Sensor if there is none).
func (f *Fleet) Get(id int) Sensor {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rt[id]; r != nil {
		return *r.cfg.Load()
	}
	return Sensor{}
}

// Live is the running state of a sensor.
type Live struct {
	Open     bool    `json:"open"`            // its port / TCP server is open
	Error    string  `json:"error,omitempty"` // why not
	Where    string  `json:"where"`           // the device in use
	Served   uint64  `json:"served"`          // requests answered or dropped
	LastAgoS float64 `json:"last_ago_s"`      // since the last request, s; -1 = none yet
	Unit     int     `json:"unit"`
	Line     int     `json:"line"` // the shared line (bus) it is on
}

// Device is the device and template of a sensor, for reading registers.
func (f *Fleet) Device(id int) (*device.Device, *profile.Profile, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rt[id]
	if r == nil {
		return nil, nil, false
	}
	return r.dev, r.prof, true
}

// LiveOf is the state of every sensor by id.
func (f *Fleet) LiveOf() map[int]Live {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]Live{}
	for _, rl := range f.lines {
		for _, id := range rl.ids {
			r := f.rt[id]
			if r == nil {
				continue
			}
			lv := Live{Open: rl.l.Open(), Error: rl.l.Err(), Where: rl.l.Device(), Served: r.dev.Served.Load(),
				LastAgoS: -1, Unit: int(r.dev.Unit()), Line: rl.l.Index}
			if t := r.dev.Last.Load(); t > 0 {
				lv.LastAgoS = time.Since(time.Unix(0, t)).Seconds()
			}
			if lv.Where == "" {
				lv.Where = rl.set.Where()
			}
			out[id] = lv
		}
	}
	return out
}

// Lines are the running lines (buses), for statistics.
func (f *Fleet) Lines() []*server.Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*server.Line, 0, len(f.lines))
	for _, rl := range f.lines {
		out = append(out, rl.l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// Running tells whether the emulation runs (false: every port closed, the sensors are kept).
func (f *Fleet) Running() bool { f.mu.Lock(); defer f.mu.Unlock(); return !f.stopped }

// SetRunning starts or stops the whole emulation.
func (f *Fleet) SetRunning(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if on && f.stopped {
		f.stopped = false
		if err := f.check(); err != nil {
			f.stopped = true
			return err
		}
	}
	f.stopped = !on
	f.apply()
	f.save()
	if on {
		log.Printf("emulation started")
	} else {
		log.Printf("emulation stopped")
	}
	return nil
}

// Crazy is the faults of the crazy mode.
func (f *Fleet) Crazy() server.Faults { f.mu.Lock(); defer f.mu.Unlock(); return f.crazy }

// SetCrazy changes the crazy mode of all crazy sensors.
func (f *Fleet) SetCrazy(c server.Faults) {
	f.mu.Lock()
	f.crazy = c
	f.apply()
	f.save()
	f.mu.Unlock()
}

// Template is a profile the user can pick for a sensor.
type Template struct {
	Name        string `json:"name"`  // what goes into Sensor.Profile
	Title       string `json:"title"` // short name to show
	Description string `json:"description"`
}

// Templates: generic, the built-in profiles and the YAML files of Options.ProfileDirs.
func (f *Fleet) Templates() []Template {
	out := []Template{{Generic, "Virtual (random)", fmt.Sprintf("%d holding registers, random values (or from a file)", f.opt.Regs)}}
	seen := map[string]bool{Generic: true}
	for _, n := range profile.Builtin() {
		if p, err := profile.Load(n); err == nil {
			out = append(out, Template{n, title(p), p.Description})
			seen[p.Name] = true
		}
	}
	for _, d := range f.opt.ProfileDirs {
		paths, _ := filepath.Glob(filepath.Join(d, "*.yaml"))
		for _, path := range paths {
			p, err := profile.Load(path)
			if err != nil || seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			out = append(out, Template{path, title(p), p.Description})
		}
	}
	return out
}

func title(p *profile.Profile) string {
	if t, _, ok := strings.Cut(p.Description, " "); ok && t != "" && strings.ToUpper(t) == t {
		return t // "KTH2-R T/RH sensor ..." -> KTH2-R
	}
	return p.Name
}

func (f *Fleet) loadProfile(name string) (*profile.Profile, error) {
	p, err := profile.Load(name)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", name, err)
	}
	return p, nil
}

// keepServing opens the line and serves it; when the port fails (USB re-plug) it waits and reopens.
func keepServing(ctx context.Context, l *server.Line, latency int) {
	lastErr := ""
	for ctx.Err() == nil {
		st := l.Settings()
		err := serveOnce(ctx, l, st, latency, func() {
			l.SetErr("")
			if lastErr != "" {
				log.Printf("line %d: back, %s", l.Index, st)
			} else {
				log.Printf("line %d: serving, %s", l.Index, st)
			}
			lastErr = ""
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("closed")
		}
		l.SetErr(err.Error())
		if err.Error() != lastErr { // log once per new error, not every second
			log.Printf("line %d (%s): %v, waiting", l.Index, st.Where(), err)
			lastErr = err.Error()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func serveOnce(ctx context.Context, l *server.Line, st server.Settings, latency int, opened func()) error {
	if st.Transport == "tcp" {
		ln, err := net.Listen("tcp", st.Listen)
		if err != nil {
			return fmt.Errorf("tcp %s: %w", st.Listen, err)
		}
		opened()
		return l.ServeTCP(ctx, ln)
	}
	cfg, err := st.Serial()
	if err != nil {
		return err
	}
	var c serial.Conn
	if st.Pty {
		c, err = serial.OpenPty(st.Port)
	} else if dev := serial.Resolve(st.Port); dev == "" {
		err = fmt.Errorf("channel %s not present", st.Port)
	} else {
		if latency > 0 {
			serial.SetLatency(dev, latency) // a re-plugged FTDI starts again at 16 ms
		}
		if c, err = serial.Open(dev, cfg); err != nil {
			err = fmt.Errorf("%s: %w", dev, err)
		}
	}
	if err != nil {
		return err
	}
	defer c.Close()
	opened()
	return l.Serve(ctx, c)
}

// Wait waits until every line has stopped (after ctx is done); pty links are removed.
func (f *Fleet) Wait() {
	f.mu.Lock()
	lines := make([]*runLine, 0, len(f.lines))
	for _, rl := range f.lines {
		lines = append(lines, rl)
	}
	f.mu.Unlock()
	for _, rl := range lines {
		<-rl.done
		if rl.set.Pty {
			os.Remove(rl.set.Port)
		}
	}
}
