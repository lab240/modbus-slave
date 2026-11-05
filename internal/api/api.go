// Package api is the local JSON API and the web page: the emulated sensors, their registers, the crazy mode.
// It listens on 127.0.0.1 or a unix socket by default; any other address needs a token.
//
//	GET    /                                   the web page (static; it asks for the token if one is set)
//	GET    /api/v1/status                      version, uptime, the lines (buses) with counters
//	GET    /api/v1/templates                   sensor templates (profiles)
//	GET    /api/v1/ports                       serial ports found in the system
//	GET    /api/v1/sensors                     the sensors with their live state
//	POST   /api/v1/sensors                     add one: {"profile": "kth2r", "unit": 1, "transport": "rtu", "port": "/dev/ttyUSB0", "baud": 9600, "mode": "8N1"}
//	GET    /api/v1/sensors/{id}
//	PUT    /api/v1/sensors/{id}                change the fields given: {"transport": "tcp", "listen": ":502"}, {"crazy": true}
//	DELETE /api/v1/sensors/{id}
//	GET    /api/v1/sensors/{id}/map            the register map of its template
//	GET    /api/v1/sensors/{id}/registers?addr=0x300&count=2[&fc=4]
//	PUT    /api/v1/sensors/{id}/registers      {"addr": 1279, "values": [-50]}  (writable registers only)
//	GET    /api/v1/sensors?values=1            ... and the main values of every sensor (one request for the whole page)
//	GET    /api/v1/service                     {"running": true, "unit": "modbus-slave.service", "enabled": true}
//	PUT    /api/v1/service                     {"running": false} stops the emulation (every port closed; the page stays),
//	                                           {"enabled": true} = systemctl enable (start with the system)
//	GET    /api/v1/crazy                       the faults of the crazy mode
//	PUT    /api/v1/crazy                       change them: {"delay_ms": 20, "jitter_ms": 400, "drop": 0.1, ...}
package api

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/fleet"
	"github.com/lab240/modbus-slave/internal/profile"
	"github.com/lab240/modbus-slave/internal/serial"
	"github.com/lab240/modbus-slave/internal/server"
)

//go:embed web/index.html
var page []byte

// Info is the static part of the status.
type Info struct {
	Version string `json:"version"`
}

type API struct {
	info  Info
	f     *fleet.Fleet
	regs  int // generic: number of registers
	token string
	start time.Time
	unit  string // our systemd unit, "" when not run by systemd
}

func New(info Info, f *fleet.Fleet, genericRegs int, token string) *API {
	return &API{info: info, f: f, regs: genericRegs, token: token, start: time.Now(), unit: unitName()}
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
		return nil, fmt.Errorf("--api %s is reachable from the network: give --api-token or --api-token-file too", addr)
	}
	return net.Listen("tcp", addr)
}

// Handler is the HTTP handler of the API and the page.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", a.status)
	mux.HandleFunc("GET /api/v1/templates", func(w http.ResponseWriter, r *http.Request) { reply(w, a.f.Templates()) })
	mux.HandleFunc("GET /api/v1/ports", func(w http.ResponseWriter, r *http.Request) {
		ps := serial.List()
		if ps == nil {
			ps = []serial.PortInfo{}
		}
		reply(w, ps)
	})
	mux.HandleFunc("GET /api/v1/sensors", a.sensors)
	mux.HandleFunc("POST /api/v1/sensors", a.addSensor)
	mux.HandleFunc("GET /api/v1/sensors/{id}", a.getSensor)
	mux.HandleFunc("PUT /api/v1/sensors/{id}", a.putSensor)
	mux.HandleFunc("DELETE /api/v1/sensors/{id}", a.delSensor)
	mux.HandleFunc("GET /api/v1/sensors/{id}/map", a.getMap)
	mux.HandleFunc("GET /api/v1/sensors/{id}/registers", a.getRegs)
	mux.HandleFunc("PUT /api/v1/sensors/{id}/registers", a.putRegs)
	mux.HandleFunc("GET /api/v1/crazy", func(w http.ResponseWriter, r *http.Request) { reply(w, toJSON(a.f.Crazy())) })
	mux.HandleFunc("PUT /api/v1/crazy", a.putCrazy)
	mux.HandleFunc("GET /api/v1/service", a.getService)
	mux.HandleFunc("PUT /api/v1/service", a.putService)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
			w.Write(page)
			return
		}
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

type lineStatus struct {
	Line     int             `json:"line"`
	Device   string          `json:"device"`
	Open     bool            `json:"open"`
	Error    string          `json:"error,omitempty"`
	Settings server.Settings `json:"settings"`
	Stats    stats           `json:"stats"`
	Units    []int           `json:"units"`
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Info
		UptimeS float64      `json:"uptime_s"`
		Running bool         `json:"running"`
		Sensors int          `json:"sensors"`
		Lines   []lineStatus `json:"lines"`
	}{Info: a.info, UptimeS: time.Since(a.start).Seconds(), Running: a.f.Running(), Sensors: len(a.f.List()), Lines: []lineStatus{}}
	for _, l := range a.f.Lines() {
		s := l.Stats.Snapshot()
		ls := lineStatus{Line: l.Index, Device: l.Device(), Open: l.Open(), Error: l.Err(), Settings: l.Settings(),
			Stats: stats{s.Req, s.Ans, s.Exc, s.Foreign, s.Bcast, s.Resync, s.Drop, s.BadCRC}}
		for _, d := range l.Devices {
			ls.Units = append(ls.Units, int(d.Unit()))
		}
		out.Lines = append(out.Lines, ls)
	}
	reply(w, out)
}

type sensorOut struct {
	fleet.Sensor
	Live   *fleet.Live `json:"live,omitempty"`
	Values []values    `json:"values,omitempty"` // with ?values=1
}

// values are the registers of one block of the main values of a sensor (measurements, not the passport).
type values struct {
	Addr int      `json:"addr"`
	Regs []uint16 `json:"regs"`
}

// mainValues: up to 3 blocks whose values change (wave, file, random), up to 4 registers each;
// the generic device: its first 4 registers.
func (a *API) mainValues(id int) []values {
	d, p, ok := a.f.Device(id)
	if !ok {
		return nil
	}
	var out []values
	for _, b := range MapOf(p, a.regs).Holding {
		if len(out) == 3 {
			break
		}
		if p != nil && (b.Type == "string" || !(strings.HasPrefix(b.Source, "wave") || strings.HasPrefix(b.Source, "file") || strings.HasPrefix(b.Source, "random"))) {
			continue
		}
		if regs, err := d.ReadRegs(3, uint16(b.Addr), uint16(min(b.Regs, 4))); err == nil {
			out = append(out, values{b.Addr, regs})
		}
		if p == nil {
			break
		}
	}
	return out
}

func (a *API) sensors(w http.ResponseWriter, r *http.Request) {
	live := a.f.LiveOf()
	withValues := r.URL.Query().Get("values") != ""
	out := []sensorOut{}
	for _, s := range a.f.List() {
		so := sensorOut{Sensor: s}
		if lv, ok := live[s.ID]; ok {
			so.Live = &lv
		}
		if withValues {
			so.Values = a.mainValues(s.ID)
		}
		out = append(out, so)
	}
	reply(w, out)
}

func (a *API) id(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || a.f.Get(id).ID == 0 {
		fail(w, http.StatusNotFound, fmt.Sprintf("no sensor %q", r.PathValue("id")))
		return 0, false
	}
	return id, true
}

func (a *API) getSensor(w http.ResponseWriter, r *http.Request) {
	if id, ok := a.id(w, r); ok {
		so := sensorOut{Sensor: a.f.Get(id)}
		if lv, ok := a.f.LiveOf()[id]; ok {
			so.Live = &lv
		}
		reply(w, so)
	}
}

// editable are the fields of a sensor the API may set.
var editable = map[string]bool{"name": true, "profile": true, "unit": true, "transport": true, "port": true,
	"baud": true, "mode": true, "listen": true, "crazy": true, "file": true, "stopped": true}

// overlay applies the JSON fields of body to s; unknown fields are an error.
func overlay(s *fleet.Sensor, body []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	for k := range raw {
		if !editable[k] {
			return fmt.Errorf("unknown field %s", k)
		}
	}
	cur, _ := json.Marshal(s)
	var m map[string]json.RawMessage
	json.Unmarshal(cur, &m)
	for k, v := range raw {
		m[k] = v
	}
	merged, _ := json.Marshal(m)
	return json.Unmarshal(merged, s)
}

func readBody(r *http.Request) []byte {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	return b
}

func (a *API) addSensor(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	var s fleet.Sensor
	if strings.TrimSpace(string(body)) != "" {
		if err := overlay(&s, body); err != nil {
			fail(w, http.StatusUnprocessableEntity, "body: "+err.Error())
			return
		}
	}
	got, err := a.f.Add(s)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(sensorOut{Sensor: got})
}

func (a *API) putSensor(w http.ResponseWriter, r *http.Request) {
	id, ok := a.id(w, r)
	if !ok {
		return
	}
	body := readBody(r)
	var perr error
	got, also, err := a.f.Update(id, func(s *fleet.Sensor) { perr = overlay(s, body) })
	if perr != nil {
		fail(w, http.StatusUnprocessableEntity, "body: "+perr.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if also == nil {
		also = []int{}
	}
	reply(w, map[string]any{"sensor": got, "also": also})
}

func (a *API) delSensor(w http.ResponseWriter, r *http.Request) {
	if id, ok := a.id(w, r); ok {
		if err := a.f.Remove(id); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) dev(w http.ResponseWriter, r *http.Request) (*device.Device, *profile.Profile) {
	id, ok := a.id(w, r)
	if !ok {
		return nil, nil
	}
	d, p, _ := a.f.Device(id)
	return d, p
}

func (a *API) getMap(w http.ResponseWriter, r *http.Request) {
	if d, p := a.dev(w, r); d != nil {
		reply(w, MapOf(p, a.regs))
	}
}

func (a *API) getRegs(w http.ResponseWriter, r *http.Request) {
	d, _ := a.dev(w, r)
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
	d, _ := a.dev(w, r)
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
	log.Printf("api: unit %d: wrote %d register(s) at 0x%04X", d.Unit(), len(vals), *in.Addr)
	reply(w, map[string]any{"addr": *in.Addr, "values": vals})
}

// Faults is the API / state-file form of server.Faults: times in ms.
type Faults struct {
	DelayMs   float64 `json:"delay_ms"`
	JitterMs  float64 `json:"jitter_ms"`
	Drop      float64 `json:"drop"`
	Exc       float64 `json:"exc"`
	BadCRC    float64 `json:"badcrc"`
	Frag      int     `json:"frag"`
	FragGapMs float64 `json:"frag_gap_ms"`
}

func ms(d time.Duration) float64   { return float64(d) / float64(time.Millisecond) }
func dur(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }

func toJSON(f server.Faults) Faults {
	return Faults{ms(f.Delay), ms(f.Jitter), f.Drop, f.Exc, f.BadCRC, f.Frag, ms(f.FragGap)}
}

// ToFaults converts back; Check first.
func (in Faults) ToFaults() server.Faults {
	return server.Faults{Delay: dur(in.DelayMs), Jitter: dur(in.JitterMs), Drop: in.Drop, Exc: in.Exc, BadCRC: in.BadCRC,
		Frag: in.Frag, FragGap: dur(in.FragGapMs)}
}

// FaultsOf is the API form of f.
func FaultsOf(f server.Faults) Faults { return toJSON(f) }

// Check validates the ranges.
func (in Faults) Check() error {
	for _, p := range []float64{in.Drop, in.Exc, in.BadCRC} {
		if p < 0 || p > 1 {
			return errors.New("drop, exc, badcrc: a probability 0..1")
		}
	}
	for _, t := range []float64{in.DelayMs, in.JitterMs, in.FragGapMs} {
		if t < 0 || t > 60000 {
			return errors.New("times: 0..60000 ms")
		}
	}
	if in.Frag < 0 || in.Frag > 64 {
		return errors.New("frag: 0..64")
	}
	return nil
}

func (a *API) putCrazy(w http.ResponseWriter, r *http.Request) {
	in := toJSON(a.f.Crazy())
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusUnprocessableEntity, "body: "+err.Error())
		return
	}
	if err := in.Check(); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	a.f.SetCrazy(in.ToFaults())
	log.Printf("api: crazy mode %+v", in)
	reply(w, in)
}

// Block is one block of the register map.
type Block struct {
	Addr      int     `json:"addr"`
	Regs      int     `json:"regs"` // registers the block takes
	Name      string  `json:"name,omitempty"`
	Type      string  `json:"type"`
	WordOrder string  `json:"word_order,omitempty"`
	Scale     float64 `json:"scale,omitempty"`
	Source    string  `json:"source"`
	RW        bool    `json:"rw"`
}

// RegMap is the register map of a template.
type RegMap struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Functions   []int   `json:"functions"`
	Holding     []Block `json:"holding"`
	Input       []Block `json:"input,omitempty"` // empty: FC 4 reads the holding map
}

// MapOf describes a profile; p == nil is the generic device with n registers.
func MapOf(p *profile.Profile, n int) RegMap {
	if p == nil {
		return RegMap{Name: fleet.Generic, Description: fmt.Sprintf("v1 device: %d holding registers, random values or from a file", n),
			Functions: []int{3, 4}, Holding: []Block{{Addr: 0, Regs: n, Name: "registers", Type: "uint16", Source: "random or file"}}}
	}
	conv := func(rs []profile.Reg) []Block {
		var out []Block
		for i := range rs {
			r := &rs[i]
			t := r.Type
			if t == "" {
				t = "uint16"
			}
			if r.String != nil {
				t = "string"
			}
			b := Block{Addr: r.Addr, Regs: r.Regs(), Name: r.Name, Type: t, Source: r.Kind(), RW: r.RW}
			if r.Regs() > 1 && t != "uint16" && t != "int16" && t != "string" {
				b.WordOrder = cmp(r.WordOrder, "big")
			}
			if r.Scale != 0 && r.Scale != 1 {
				b.Scale = r.Scale
			}
			out = append(out, b)
		}
		return out
	}
	return RegMap{Name: p.Name, Description: p.Description, Functions: p.Functions, Holding: conv(p.Holding), Input: conv(p.Input)}
}
