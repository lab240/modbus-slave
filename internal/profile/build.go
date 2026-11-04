package profile

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/source"
)

// Env is where a device instance lives; it feeds {line}/{unit}, line_step/unit_step and the specials.
type Env struct {
	Line   int
	Unit   byte
	Baud   int
	Parity byte // 'N', 'E', 'O'
	Seed   uint64
	Start  time.Time // t = 0 of the waves; shared by all devices of a run
	Files  *Files    // shared file sources (one per path)
}

// Files shares one source.File per path, so many devices reading one file read it once per change.
type Files struct {
	mu sync.Mutex
	m  map[string]*source.File
}

func (f *Files) get(path string, maxAge time.Duration) *source.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]*source.File{}
	}
	if s := f.m[path]; s != nil {
		return s
	}
	s := &source.File{Path: path, MaxAge: maxAge}
	f.m[path] = s
	return s
}

var speedCode = map[int]uint16{1200: 0, 2400: 1, 4800: 2, 9600: 3, 19200: 4, 38400: 5, 57600: 6, 115200: 7}

// group is one Reg block of one device instance.
type group struct {
	r     *Reg
	start uint16
	n     int      // registers
	store []uint16 // const blocks: the current value (rw blocks keep what the master wrote)
	eval  func(m *regMap, cache map[*group][]uint16) ([]uint16, error)
}

type regMap struct {
	dev   *device.Device
	env   Env
	cells map[uint16]cell
	rnd   *rand.Rand
	mu    sync.Mutex
}

type cell struct {
	g   *group
	idx int
}

// Build makes a device from a profile.
func Build(p *Profile, env Env) *device.Device {
	if env.Start.IsZero() {
		env.Start = time.Now()
	}
	if env.Files == nil {
		env.Files = &Files{}
	}
	funcs := make([]byte, len(p.Functions))
	for i, f := range p.Functions {
		funcs[i] = byte(f)
	}
	hold := &regMap{env: env, cells: map[uint16]cell{}, rnd: rand.New(rand.NewPCG(env.Seed, uint64(env.Line)<<8|uint64(env.Unit)))}
	var input *regMap
	for i := range p.Holding {
		hold.add(&p.Holding[i])
	}
	if len(p.Input) > 0 {
		input = &regMap{env: env, cells: map[uint16]cell{}, rnd: hold.rnd}
		for i := range p.Input {
			input.add(&p.Input[i])
		}
	}
	regs := &maps{hold: hold, input: input}
	d := device.New(env.Unit, regs, funcs...)
	hold.dev = d
	if input != nil {
		input.dev = d
	}
	return d
}

type maps struct{ hold, input *regMap }

func (m *maps) Read(fc byte, addr, count uint16) ([]uint16, error) {
	if fc == 4 && m.input != nil {
		return m.input.read(addr, count)
	}
	return m.hold.read(addr, count)
}

func (m *maps) Write(addr uint16, vals []uint16) error { return m.hold.write(addr, vals) }

func (m *regMap) add(r *Reg) {
	g := &group{r: r, start: uint16(r.Addr), n: r.Regs()}
	for i := 0; i < g.n; i++ {
		m.cells[uint16(r.Addr+i)] = cell{g, i}
	}
	env := m.env
	switch {
	case r.String != nil:
		g.store = make([]uint16, g.n)
		b := []byte(*r.String)
		for i := range g.store {
			var hi, lo byte
			if 2*i < len(b) {
				hi = b[2*i]
			}
			if 2*i+1 < len(b) {
				lo = b[2*i+1]
			}
			g.store[i] = uint16(hi)<<8 | uint16(lo)
		}
	case r.Value != nil || len(r.Values) > 0:
		vals := make([]float64, r.count())
		for i := range vals {
			v := 0.0
			if len(r.Values) > 0 {
				v = r.Values[i]
			} else {
				v = *r.Value
			}
			vals[i] = v + r.Base + float64(env.Line)*r.LineStep + float64(env.Unit)*r.UnitStep + float64(i)*r.IndexStep
		}
		g.store = encode(r, vals)
	case r.Wave != nil:
		w := r.Wave
		g.eval = func(m *regMap, _ map[*group][]uint16) ([]uint16, error) {
			t := time.Since(m.env.Start).Seconds()
			vals := make([]float64, r.count())
			for i := range vals {
				v := w.Base + float64(m.env.Line)*w.LineStep + float64(m.env.Unit)*w.UnitStep + w.Amp*shape(w, t)
				if w.Noise > 0 {
					m.mu.Lock()
					v += (2*m.rnd.Float64() - 1) * w.Noise
					m.mu.Unlock()
				}
				vals[i] = v
			}
			return encode(r, vals), nil
		}
	case r.Random:
		g.eval = func(m *regMap, _ map[*group][]uint16) ([]uint16, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			out := make([]uint16, g.n)
			for i := range out {
				out[i] = uint16(m.rnd.Uint32())
			}
			return out, nil
		}
	case r.File != "":
		path := strings.NewReplacer("{line}", strconv.Itoa(env.Line), "{unit}", strconv.Itoa(int(env.Unit))).Replace(r.File)
		maxAge := 10 * time.Second
		if r.MaxAge != nil {
			maxAge = time.Duration(*r.MaxAge * float64(time.Second))
		}
		f := env.Files.get(path, maxAge)
		g.eval = func(m *regMap, _ map[*group][]uint16) ([]uint16, error) {
			all, ok := f.Read()
			if !ok {
				if r.OnStale == "exception" {
					return nil, device.ErrDeviceFailure
				}
				return make([]uint16, g.n), nil
			}
			vals := make([]float64, r.count())
			for i := range vals {
				if j := r.FileLine + i; j < len(all) {
					vals[i] = all[j]
				}
			}
			return encode(r, vals), nil
		}
	case len(r.Specials) > 0:
		g.eval = func(m *regMap, _ map[*group][]uint16) ([]uint16, error) {
			out := make([]uint16, len(r.Specials))
			for i, s := range r.Specials {
				out[i] = m.special(s)
			}
			return out, nil
		}
	}
	if r.Add != nil {
		base := g.eval
		if base == nil {
			base = func(*regMap, map[*group][]uint16) ([]uint16, error) { return append([]uint16(nil), g.store...), nil }
		}
		add := uint16(*r.Add)
		g.eval = func(m *regMap, cache map[*group][]uint16) ([]uint16, error) {
			out, err := base(m, cache)
			if err != nil {
				return nil, err
			}
			for i := range out {
				c, ok := m.cells[add+uint16(i)]
				if !ok {
					continue
				}
				v, err := m.value(c, cache)
				if err != nil {
					return nil, err
				}
				out[i] += v // register arithmetic: int16 corrections wrap correctly
			}
			return out, nil
		}
	}
}

func (m *regMap) special(s string) uint16 {
	e := m.env
	switch s {
	case "unit":
		return uint16(m.dev.Unit())
	case "line":
		return uint16(e.Line)
	case "baud100":
		return uint16(e.Baud / 100)
	case "speed_code":
		if c, ok := speedCode[e.Baud]; ok {
			return c
		}
		return 0xFF
	case "parity_code":
		return map[byte]uint16{'N': 0, 'E': 1, 'O': 2}[e.Parity]
	case "served_hi":
		return uint16(m.dev.Served.Load() >> 16)
	case "served_lo":
		return uint16(m.dev.Served.Load())
	}
	return 0
}

// value of one register; a block is evaluated once per request (cache), so the words of a 32-bit value
// and neighbouring registers come from the same moment.
func (m *regMap) value(c cell, cache map[*group][]uint16) (uint16, error) {
	g := c.g
	if g.eval == nil {
		return g.store[c.idx], nil
	}
	vals, ok := cache[g]
	if !ok {
		cache[g] = nil // guards an `add` loop
		var err error
		if vals, err = g.eval(m, cache); err != nil {
			return 0, err
		}
		cache[g] = vals
	}
	if c.idx >= len(vals) {
		return 0, nil
	}
	return vals[c.idx], nil
}

func (m *regMap) read(addr, count uint16) ([]uint16, error) {
	out := make([]uint16, count)
	cache := map[*group][]uint16{}
	for i := range out {
		c, ok := m.cells[addr+uint16(i)]
		if !ok {
			return nil, device.ErrAddress
		}
		v, err := m.value(c, cache)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (m *regMap) write(addr uint16, vals []uint16) error {
	for i := range vals { // all or nothing: check first
		c, ok := m.cells[addr+uint16(i)]
		if !ok || !c.g.r.RW {
			return device.ErrAddress
		}
		if len(c.g.r.Specials) == 1 && (vals[i] < 1 || vals[i] > 247) { // special unit
			return device.ErrAddress
		}
	}
	for i, v := range vals {
		c := m.cells[addr+uint16(i)]
		if len(c.g.r.Specials) == 1 {
			m.dev.SetUnit(byte(v)) // takes effect for the next request, like a real device
			continue
		}
		c.g.store[c.idx] = v
	}
	return nil
}

// shape is -1..+1.
func shape(w *Wave, t float64) float64 {
	if w.Shape == "const" {
		return 0
	}
	x := t/w.Period + w.Phase
	x -= math.Floor(x)
	switch w.Shape {
	case "saw":
		return 2*x - 1
	case "triangle":
		return 1 - 4*math.Abs(x-0.5)
	case "sine":
		return math.Sin(2 * math.Pi * x)
	case "square":
		if x < 0.5 {
			return 1
		}
		return -1
	}
	return 0
}

// encode turns values into registers according to scale, type and word order; integer types are rounded.
func encode(r *Reg, vals []float64) []uint16 {
	out := make([]uint16, 0, len(vals)*r.width())
	for _, v := range vals {
		v *= r.Scale
		switch r.Type {
		case "uint32", "int32":
			out = words(out, uint32(int64(math.Round(v))), r.WordOrder)
		case "float32":
			out = words(out, math.Float32bits(float32(v)), r.WordOrder)
		default: // uint16, int16: two's complement for negatives
			out = append(out, uint16(int64(math.Round(v))))
		}
	}
	return out
}

func words(out []uint16, v uint32, order string) []uint16 {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	hi, lo := binary.BigEndian.Uint16(b[:2]), binary.BigEndian.Uint16(b[2:])
	if order == "little" {
		return append(out, lo, hi)
	}
	return append(out, hi, lo)
}
