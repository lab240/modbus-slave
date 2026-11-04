// Package profile describes a device in YAML: the function codes it answers and its registers, each
// register block with one value source. Built-in profiles are embedded; any file can be given instead.
//
//	name: my-sensor
//	functions: [3, 4, 6, 16]
//	holding:
//	  - {addr: 0, string: "MY-SENSOR"}                        # const text, 2 chars per register
//	  - {addr: 10, special: unit, rw: true}                   # address; writing 1..247 changes it
//	  - {addr: 100, type: int16, wave: {shape: sine, base: 215, amp: 30, period: 600}}
//	  - {addr: 200, file: /run/sensor{unit}.dat, count: 2, type: float32, max_age: 10}
//	  - {addr: 300, count: 4, value: 0, rw: true}             # set points the master writes
//	input: [...]                                               # FC 4 map; absent = FC 4 reads `holding`
package profile

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed builtin/*.yaml
var builtin embed.FS

// Profile is one device type.
type Profile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Functions   []int  `yaml:"functions"`
	Holding     []Reg  `yaml:"holding"`
	Input       []Reg  `yaml:"input"`
}

// Reg is a block of registers with one value source.
type Reg struct {
	Addr  int    `yaml:"addr"`
	Name  string `yaml:"name"`  // shown by the API and the web page
	Count int    `yaml:"count"` // number of values (a 32-bit value takes 2 registers); default 1

	// const: value / values / string; base + line*line_step + unit*unit_step + i*index_step is added to it
	Value     *float64  `yaml:"value"`
	Values    []float64 `yaml:"values"`
	String    *string   `yaml:"string"`
	Base      float64   `yaml:"base"`
	LineStep  float64   `yaml:"line_step"`
	UnitStep  float64   `yaml:"unit_step"`
	IndexStep float64   `yaml:"index_step"`

	Wave     *Wave    `yaml:"wave"`
	Random   bool     `yaml:"random"`
	File     string   `yaml:"file"`      // {line} and {unit} are replaced
	FileLine int      `yaml:"file_line"` // first line of the file used by this block (0-based)
	MaxAge   *float64 `yaml:"max_age"`   // s; default 10, 0 = no check
	OnStale  string   `yaml:"on_stale"`  // zero (default, as v1) | exception (exception 4)
	Special  string   `yaml:"special"`
	Specials []string `yaml:"specials"`

	Scale     float64 `yaml:"scale"`      // value * scale before it goes into the register(s); default 1
	Type      string  `yaml:"type"`       // uint16 (default) | int16 | uint32 | int32 | float32
	WordOrder string  `yaml:"word_order"` // big (default: high word first) | little
	RW        bool    `yaml:"rw"`         // the master may write it (FC 6 / FC 16); the value is then kept
	Add       *int    `yaml:"add"`        // + the value of this register (a correction the master writes)
}

// Wave is a synthetic signal: base + line*line_step + unit*unit_step + shape(t) * amp (+ noise).
type Wave struct {
	Shape    string  `yaml:"shape"` // saw | triangle | sine | square | const
	Base     float64 `yaml:"base"`
	LineStep float64 `yaml:"line_step"`
	UnitStep float64 `yaml:"unit_step"`
	Amp      float64 `yaml:"amp"`
	Period   float64 `yaml:"period"` // s
	Phase    float64 `yaml:"phase"`  // 0..1 of the period
	Noise    float64 `yaml:"noise"`  // + uniform random -noise..+noise
}

var specials = map[string]bool{
	"unit": true, "line": true, "baud100": true, "speed_code": true, "parity_code": true,
	"served_hi": true, "served_lo": true,
}

// Builtin lists the embedded profiles.
func Builtin() []string {
	ents, _ := builtin.ReadDir("builtin")
	var out []string
	for _, e := range ents {
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(out)
	return out
}

// Load reads a built-in profile by name, or a YAML file by path.
func Load(nameOrPath string) (*Profile, error) {
	var data []byte
	var err error
	if strings.ContainsAny(nameOrPath, "/\\") || strings.HasSuffix(nameOrPath, ".yaml") || strings.HasSuffix(nameOrPath, ".yml") {
		data, err = os.ReadFile(nameOrPath)
	} else {
		data, err = builtin.ReadFile("builtin/" + nameOrPath + ".yaml")
		if err != nil {
			return nil, fmt.Errorf("profile %q: not built in (have: %s) and not a file", nameOrPath, strings.Join(Builtin(), ", "))
		}
	}
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", nameOrPath, err)
	}
	if p.Name == "" {
		p.Name = strings.TrimSuffix(filepath.Base(nameOrPath), filepath.Ext(nameOrPath))
	}
	return p, nil
}

// Parse decodes and checks a profile; unknown keys are an error (a typo must not pass silently).
func Parse(data []byte) (*Profile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	return &p, p.check()
}

// Kind tells where the values of the block come from, for the API and the web page.
func (r *Reg) Kind() string {
	switch {
	case r.String != nil:
		return "string"
	case r.Wave != nil:
		return "wave " + r.Wave.Shape
	case r.Random:
		return "random"
	case r.File != "":
		return "file " + r.File
	case r.Special != "":
		return "special " + r.Special
	case len(r.Specials) > 0:
		return "specials " + strings.Join(r.Specials, ",")
	}
	return "const"
}

func (r *Reg) width() int {
	switch r.Type {
	case "uint32", "int32", "float32":
		return 2
	}
	return 1
}

// Regs is the number of registers the block takes.
func (r *Reg) Regs() int {
	if r.String != nil {
		if r.Count > 0 {
			return r.Count
		}
		return (len(*r.String) + 1) / 2
	}
	if len(r.Specials) > 0 {
		return len(r.Specials)
	}
	return r.count() * r.width()
}

func (r *Reg) count() int {
	if len(r.Values) > 0 {
		return len(r.Values)
	}
	if r.Count > 0 {
		return r.Count
	}
	return 1
}

func (p *Profile) check() error {
	if len(p.Functions) == 0 {
		p.Functions = []int{3, 4, 6, 16}
	}
	for _, f := range p.Functions {
		if f != 3 && f != 4 && f != 6 && f != 8 && f != 16 {
			return fmt.Errorf("functions: %d not supported (3, 4, 6, 8, 16)", f)
		}
	}
	if len(p.Holding) == 0 && len(p.Input) == 0 {
		return fmt.Errorf("no registers")
	}
	for _, m := range []struct {
		name string
		regs []Reg
	}{{"holding", p.Holding}, {"input", p.Input}} {
		used := map[int]int{}
		for i := range m.regs {
			r := &m.regs[i]
			where := fmt.Sprintf("%s[%d] addr 0x%04X", m.name, i, r.Addr)
			if err := r.check(); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			if r.Addr < 0 || r.Addr+r.Regs() > 0x10000 {
				return fmt.Errorf("%s: address out of 0..0xFFFF", where)
			}
			for a := r.Addr; a < r.Addr+r.Regs(); a++ {
				if j, dup := used[a]; dup {
					return fmt.Errorf("%s: register 0x%04X is already in %s[%d]", where, a, m.name, j)
				}
				used[a] = i
			}
		}
		for i, r := range m.regs {
			if r.Add != nil {
				if _, ok := used[*r.Add]; !ok {
					return fmt.Errorf("%s[%d]: add 0x%04X is not a register of this map", m.name, i, *r.Add)
				}
			}
		}
	}
	return nil
}

func (r *Reg) check() error {
	n := 0
	for _, set := range []bool{r.Value != nil || len(r.Values) > 0, r.String != nil, r.Wave != nil, r.Random,
		r.File != "", r.Special != "" || len(r.Specials) > 0} {
		if set {
			n++
		}
	}
	if n == 0 && (r.Base != 0 || r.IndexStep != 0 || r.LineStep != 0 || r.UnitStep != 0) {
		n = 1 // a pure base/step pattern is a const
		zero := 0.0
		r.Value = &zero
	}
	if n != 1 {
		return fmt.Errorf("give exactly one source: value(s) | string | wave | random | file | special(s)")
	}
	switch r.Type {
	case "", "uint16", "int16", "uint32", "int32", "float32":
	default:
		return fmt.Errorf("type %q: want uint16, int16, uint32, int32 or float32", r.Type)
	}
	switch r.WordOrder {
	case "", "big", "little":
	default:
		return fmt.Errorf("word_order %q: want big or little", r.WordOrder)
	}
	switch r.OnStale {
	case "", "zero", "exception":
	default:
		return fmt.Errorf("on_stale %q: want zero or exception", r.OnStale)
	}
	if r.Special != "" {
		r.Specials = []string{r.Special}
	}
	for _, s := range r.Specials {
		if !specials[s] {
			return fmt.Errorf("special %q: want one of unit, line, baud100, speed_code, parity_code, served_hi, served_lo", s)
		}
	}
	if r.RW && (r.Wave != nil || r.Random || r.File != "") {
		return fmt.Errorf("rw works with a const or special unit, not with wave/random/file")
	}
	if r.RW && len(r.Specials) > 0 && (len(r.Specials) != 1 || r.Specials[0] != "unit") {
		return fmt.Errorf("rw: of the specials only unit can be written")
	}
	if r.Wave != nil {
		switch r.Wave.Shape {
		case "saw", "triangle", "sine", "square", "const":
		default:
			return fmt.Errorf("wave shape %q: want saw, triangle, sine, square or const", r.Wave.Shape)
		}
		if r.Wave.Shape != "const" && r.Wave.Period <= 0 {
			return fmt.Errorf("wave: period must be > 0 s")
		}
	}
	if r.Scale == 0 {
		r.Scale = 1
	}
	return nil
}
