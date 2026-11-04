// Package api is the local JSON API: status and counters, registers, faults on the fly.
// It is meant for a local manager (napiconfig) or a small web page; it listens on 127.0.0.1 or a unix
// socket by default, and any other address needs a token.
//
//	GET  /api/v1/status
//	GET  /api/v1/profiles
//	GET  /api/v1/lines/{line}/faults
//	PUT  /api/v1/lines/{line}/faults                          {"delay_ms": 20, "drop": 0.01}  (fields given are changed)
//	GET  /api/v1/lines/{line}/devices/{unit}/registers?addr=0x300&count=2[&fc=4]
//	PUT  /api/v1/lines/{line}/devices/{unit}/registers        {"addr": 768, "values": [215]}  (writable registers only)
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/profile"
	"github.com/lab240/modbus-slave/internal/server"
)

// Info is the static part of the status.
type Info struct {
	Version string `json:"version"`
	Profile string `json:"profile"`
	Mode    string `json:"mode"` // "57600 8N1"
	Baud    int    `json:"baud"`
}

type API struct {
	info  Info
	lines []*server.Line
	token string
	start time.Time
}

func New(info Info, lines []*server.Line, token string) *API {
	return &API{info: info, lines: lines, token: token, start: time.Now()}
}

// Listen opens "unix:/path", "host:port" or ":port". A non-loopback TCP address needs a token.
func Listen(addr, token string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		os.Remove(path)
		l, err := net.Listen("unix", path)
		if err == nil {
			os.Chmod(path, 0o660)
		}
		return l, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--api %q: want 127.0.0.1:PORT or unix:/path", addr)
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback && token == "" {
		return nil, fmt.Errorf("--api %s is reachable from the network: give --api-token too", addr)
	}
	return net.Listen("tcp", addr)
}

// Handler is the HTTP handler of the API.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", a.status)
	mux.HandleFunc("GET /api/v1/profiles", a.profiles)
	mux.HandleFunc("GET /api/v1/lines/{line}/faults", a.getFaults)
	mux.HandleFunc("PUT /api/v1/lines/{line}/faults", a.putFaults)
	mux.HandleFunc("GET /api/v1/lines/{line}/devices/{unit}/registers", a.getRegs)
	mux.HandleFunc("PUT /api/v1/lines/{line}/devices/{unit}/registers", a.putRegs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
				fail(w, http.StatusUnauthorized, "missing or wrong token (Authorization: Bearer ...)")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// Serve runs the API on l until it is closed.
func (a *API) Serve(l net.Listener) {
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(l); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("api: %v", err)
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"detail": msg}) // same shape as napi-api errors
}

type stats struct {
	Req     uint64 `json:"req"`
	Ans     uint64 `json:"ans"`
	Exc     uint64 `json:"exc"`
	Foreign uint64 `json:"foreign"`
	Bcast   uint64 `json:"bcast"`
	Resync  uint64 `json:"resync"`
	Drop    uint64 `json:"drop"`
	BadCRC  uint64 `json:"badcrc"`
}

type devStatus struct {
	Unit   byte   `json:"unit"`
	Served uint64 `json:"served"`
	Source string `json:"source,omitempty"`
}

type lineStatus struct {
	Line    int         `json:"line"`
	Spec    string      `json:"spec"`
	Device  string      `json:"device"`
	Open    bool        `json:"open"`
	Stats   stats       `json:"stats"`
	Faults  faults      `json:"faults"`
	Devices []devStatus `json:"devices"`
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Info
		UptimeS float64      `json:"uptime_s"`
		Lines   []lineStatus `json:"lines"`
	}{Info: a.info, UptimeS: time.Since(a.start).Seconds()}
	for _, l := range a.lines {
		s := l.Stats.Snapshot()
		ls := lineStatus{Line: l.Index, Spec: l.Spec, Device: l.Device(), Open: l.Open(),
			Stats:  stats{s.Req, s.Ans, s.Exc, s.Foreign, s.Bcast, s.Resync, s.Drop, s.BadCRC},
			Faults: toJSON(l.Faults())}
		for _, d := range l.Devices {
			ds := devStatus{Unit: d.Unit(), Served: d.Served.Load()}
			if g, ok := d.Regs.(*device.Generic); ok {
				ds.Source = g.Src.Describe()
			}
			ls.Devices = append(ls.Devices, ds)
		}
		out.Lines = append(out.Lines, ls)
	}
	reply(w, out)
}

func (a *API) profiles(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := []item{{"generic", "v1: N holding registers, random or from a file (-f id:file)"}}
	for _, n := range profile.Builtin() {
		if p, err := profile.Load(n); err == nil {
			out = append(out, item{n, p.Description})
		}
	}
	reply(w, out)
}

// faults in JSON: durations in ms.
type faults struct {
	DelayMs   float64 `json:"delay_ms"`
	JitterMs  float64 `json:"jitter_ms"`
	Drop      float64 `json:"drop"`
	Exc       float64 `json:"exc"`
	BadCRC    float64 `json:"badcrc"`
	Frag      int     `json:"frag"`
	FragGapMs float64 `json:"frag_gap_ms"`
	Echo      bool    `json:"echo"`
}

func ms(d time.Duration) float64   { return float64(d) / float64(time.Millisecond) }
func dur(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }

func toJSON(f server.Faults) faults {
	return faults{ms(f.Delay), ms(f.Jitter), f.Drop, f.Exc, f.BadCRC, f.Frag, ms(f.FragGap), f.Echo}
}

func (a *API) line(w http.ResponseWriter, r *http.Request) *server.Line {
	n, err := strconv.Atoi(r.PathValue("line"))
	if err != nil || n < 0 || n >= len(a.lines) {
		fail(w, http.StatusNotFound, fmt.Sprintf("no line %q (0..%d)", r.PathValue("line"), len(a.lines)-1))
		return nil
	}
	return a.lines[n]
}

func (a *API) getFaults(w http.ResponseWriter, r *http.Request) {
	if l := a.line(w, r); l != nil {
		reply(w, toJSON(l.Faults()))
	}
}

func (a *API) putFaults(w http.ResponseWriter, r *http.Request) {
	l := a.line(w, r)
	if l == nil {
		return
	}
	var in struct {
		DelayMs, JitterMs, Drop, Exc, BadCRC, FragGapMs *float64
		Frag                                            *int
		Echo                                            *bool
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		fail(w, http.StatusUnprocessableEntity, "body: "+err.Error())
		return
	}
	fields := map[string]any{"delay_ms": &in.DelayMs, "jitter_ms": &in.JitterMs, "drop": &in.Drop, "exc": &in.Exc,
		"badcrc": &in.BadCRC, "frag": &in.Frag, "frag_gap_ms": &in.FragGapMs, "echo": &in.Echo}
	for k, v := range raw {
		dst, ok := fields[k]
		if !ok {
			fail(w, http.StatusUnprocessableEntity, "unknown field "+k)
			return
		}
		if err := json.Unmarshal(v, dst); err != nil {
			fail(w, http.StatusUnprocessableEntity, k+": "+err.Error())
			return
		}
	}
	f := l.Faults()
	for _, p := range []struct {
		v    *float64
		name string
	}{{in.Drop, "drop"}, {in.Exc, "exc"}, {in.BadCRC, "badcrc"}} {
		if p.v != nil && (*p.v < 0 || *p.v > 1) {
			fail(w, http.StatusUnprocessableEntity, p.name+": a probability 0..1")
			return
		}
	}
	for _, p := range []*float64{in.DelayMs, in.JitterMs, in.FragGapMs} {
		if p != nil && (*p < 0 || *p > 60000) {
			fail(w, http.StatusUnprocessableEntity, "times: 0..60000 ms")
			return
		}
	}
	set := func(dst *float64, v *float64) {
		if v != nil {
			*dst = *v
		}
	}
	if in.DelayMs != nil {
		f.Delay = dur(*in.DelayMs)
	}
	if in.JitterMs != nil {
		f.Jitter = dur(*in.JitterMs)
	}
	if in.FragGapMs != nil {
		f.FragGap = dur(*in.FragGapMs)
	}
	set(&f.Drop, in.Drop)
	set(&f.Exc, in.Exc)
	set(&f.BadCRC, in.BadCRC)
	if in.Frag != nil {
		f.Frag = *in.Frag
	}
	if in.Echo != nil {
		f.Echo = *in.Echo
	}
	l.SetFaults(f)
	log.Printf("api: line %d faults %+v", l.Index, toJSON(f))
	reply(w, toJSON(f))
}

func (a *API) dev(w http.ResponseWriter, r *http.Request) (*server.Line, *device.Device) {
	l := a.line(w, r)
	if l == nil {
		return nil, nil
	}
	u, err := strconv.Atoi(r.PathValue("unit"))
	if err == nil {
		for _, d := range l.Devices {
			if int(d.Unit()) == u {
				return l, d
			}
		}
	}
	fail(w, http.StatusNotFound, fmt.Sprintf("no unit %q on line %d", r.PathValue("unit"), l.Index))
	return nil, nil
}

func (a *API) getRegs(w http.ResponseWriter, r *http.Request) {
	_, d := a.dev(w, r)
	if d == nil {
		return
	}
	q := r.URL.Query()
	addr, err1 := strconv.ParseUint(q.Get("addr"), 0, 16)
	count, err2 := strconv.ParseUint(cmp(q.Get("count"), "1"), 0, 16)
	fc, err3 := strconv.ParseUint(cmp(q.Get("fc"), "3"), 0, 8)
	if err1 != nil || err2 != nil || err3 != nil || count < 1 || count > 125 || addr+count > 0x10000 || (fc != 3 && fc != 4) {
		fail(w, http.StatusUnprocessableEntity, "want ?addr=N&count=1..125[&fc=3|4]")
		return
	}
	vals, err := d.ReadRegs(byte(fc), uint16(addr), uint16(count))
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	reply(w, map[string]any{"addr": addr, "fc": fc, "values": vals})
}

func cmp(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (a *API) putRegs(w http.ResponseWriter, r *http.Request) {
	l, d := a.dev(w, r)
	if d == nil {
		return
	}
	var in struct {
		Addr   *int  `json:"addr"`
		Values []int `json:"values"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Addr == nil || len(in.Values) == 0 || len(in.Values) > 123 ||
		*in.Addr < 0 || *in.Addr+len(in.Values) > 0x10000 {
		fail(w, http.StatusUnprocessableEntity, `want {"addr": N, "values": [v, ...]} (1..123 values, -32768..65535)`)
		return
	}
	vals := make([]uint16, len(in.Values))
	for i, v := range in.Values {
		if v < -32768 || v > 65535 {
			fail(w, http.StatusUnprocessableEntity, "values: -32768..65535")
			return
		}
		vals[i] = uint16(v)
	}
	if err := d.WriteRegs(uint16(*in.Addr), vals); err != nil {
		fail(w, http.StatusConflict, "not writable: "+err.Error())
		return
	}
	log.Printf("api: line %d unit %d: wrote %d register(s) at 0x%04X", l.Index, d.Unit(), len(vals), *in.Addr)
	reply(w, map[string]any{"addr": *in.Addr, "values": vals})
}
