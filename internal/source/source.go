// Package source provides register values: random, or from a text file written by an external script.
package source

import (
	"bufio"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Block gives n register values at once.
type Block interface {
	Values(n int) []uint16
	Describe() string
}

// Random returns fresh random values on every read (bus load / master timeout tests), as v1.
type Random struct {
	mu  sync.Mutex
	rnd *rand.Rand
}

func NewRandom(seed uint64) *Random {
	return &Random{rnd: rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))}
}

func (r *Random) Values(n int) []uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := make([]uint16, n)
	for i := range v {
		v[i] = uint16(r.rnd.Uint32())
	}
	return v
}

func (r *Random) Describe() string { return "random" }

// File reads values from a text file, one per line, written by an external script (sensor, sysfs, ...).
// As in v1: a missing file gives zeros; with MaxAge > 0 a file not modified for longer than MaxAge gives
// zeros too, so a dead data script shows up at the master instead of frozen values.
// v2: the file is re-read only when its mtime or size changes; a value may be negative, hex (0x1F) or
// fractional (45.23, for profiles with a scale); text after the number ("4523 # T x100") is ignored.
type File struct {
	Path   string
	MaxAge time.Duration

	mu       sync.Mutex
	mtime    time.Time
	size     int64
	cache    []float64
	lastWarn time.Time
}

func (f *File) Describe() string { return "file " + f.Path }

// Read returns the numbers in the file; ok = false when the file is missing, unreadable or stale.
func (f *File) Read() (vals []float64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(f.Path)
	if err != nil {
		f.warn("file %s: %v", f.Path, err)
		return nil, false
	}
	if f.MaxAge > 0 {
		if age := time.Since(st.ModTime()); age > f.MaxAge {
			f.warn("file %s is stale (%ds > %ds)", f.Path, int(age.Seconds()), int(f.MaxAge.Seconds()))
			return nil, false
		}
	}
	if f.cache == nil || !st.ModTime().Equal(f.mtime) || st.Size() != f.size {
		vals, err := readValues(f.Path)
		if err != nil {
			f.warn("file %s: %v", f.Path, err)
			return nil, false
		}
		f.cache, f.mtime, f.size = vals, st.ModTime(), st.Size()
	}
	return f.cache, true
}

// Values is the v1 behaviour: n registers, integers as uint16 (negative = two's complement), zeros on failure.
func (f *File) Values(n int) []uint16 {
	out := make([]uint16, n)
	vals, ok := f.Read()
	if !ok {
		return out
	}
	for i := 0; i < n && i < len(vals); i++ {
		out[i] = uint16(int64(vals[i]))
	}
	return out
}

// warn logs at most once a minute, so a script that is down for hours does not flood the log.
func (f *File) warn(format string, args ...any) {
	if time.Since(f.lastWarn) >= time.Minute {
		log.Printf(format, args...)
		f.lastWarn = time.Now()
	}
}

func readValues(path string) ([]float64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var vals []float64
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue // as v1 (fscanf skips white space): empty lines do not count
		}
		v, ok := ParseNumber(fields[0])
		if !ok {
			break // as v1: the data ends at the first thing that is not a number
		}
		vals = append(vals, v)
	}
	return vals, sc.Err()
}

// ParseNumber accepts 4523, -5, 0x1F, 45.23.
func ParseNumber(s string) (float64, bool) {
	if v, err := strconv.ParseInt(s, 0, 64); err == nil {
		return float64(v), true
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v, true
	}
	return 0, false
}
