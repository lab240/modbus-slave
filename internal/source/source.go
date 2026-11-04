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
// v2: the file is re-read only when its mtime or size changes; a value may be negative (-32768..-1 is
// stored as two's complement) or a hex number (0x1F); text after the number ("4523 # T x100") is ignored.
type File struct {
	Path   string
	MaxAge time.Duration

	mu       sync.Mutex
	mtime    time.Time
	size     int64
	cache    []uint16
	lastWarn time.Time
}

func (f *File) Describe() string { return "file " + f.Path }

func (f *File) Values(n int) []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	zeros := make([]uint16, n)
	st, err := os.Stat(f.Path)
	if err != nil {
		f.warn("file %s: %v, returning zeros", f.Path, err)
		return zeros
	}
	if f.MaxAge > 0 {
		if age := time.Since(st.ModTime()); age > f.MaxAge {
			f.warn("file %s is stale (%ds > %ds), returning zeros", f.Path, int(age.Seconds()), int(f.MaxAge.Seconds()))
			return zeros
		}
	}
	if f.cache == nil || !st.ModTime().Equal(f.mtime) || st.Size() != f.size {
		vals, err := readValues(f.Path)
		if err != nil {
			f.warn("file %s: %v, returning zeros", f.Path, err)
			return zeros
		}
		f.cache, f.mtime, f.size = vals, st.ModTime(), st.Size()
	}
	copy(zeros, f.cache)
	return zeros
}

// warn logs at most once a minute, so a script that is down for hours does not flood the log.
func (f *File) warn(format string, args ...any) {
	if time.Since(f.lastWarn) >= time.Minute {
		log.Printf(format, args...)
		f.lastWarn = time.Now()
	}
}

func readValues(path string) ([]uint16, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var vals []uint16
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue // as v1 (fscanf skips white space): empty lines do not count
		}
		v, ok := parseValue(fields[0])
		if !ok {
			break
		}
		vals = append(vals, v)
	}
	return vals, sc.Err()
}

func parseValue(s string) (uint16, bool) {
	if v, err := strconv.ParseInt(s, 0, 32); err == nil && v >= -32768 && v <= 65535 {
		return uint16(v), true
	}
	return 0, false
}
