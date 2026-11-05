// modbus-slave v2 — Modbus RTU slave emulator / soft sensor.
//
// v1-compatible:  modbus-slave -p /dev/ttyUSB0 -b 115200 -a 5 -f 1:/tmp/cpu.dat -t 15 [-o 8N1] [-d | -k | -s]
// v2 additions:   several -p (each one is a line), FT4232H channels (-p 0..3 / A..D), --pty LINK,
//
//	--profile kth2r | file.yaml (YAML device profiles), --units 1-4,7, fault injection, any baud rate.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lab240/modbus-slave/internal/api"
	"github.com/lab240/modbus-slave/internal/fleet"
	"github.com/lab240/modbus-slave/internal/profile"
	"github.com/lab240/modbus-slave/internal/serial"
	"github.com/lab240/modbus-slave/internal/server"
)

var version = "2.0.0-dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

type opts struct {
	ports, ptys, files multi
	tcps               multi
	state              string
	baud               int
	mode               string
	count              int
	units              string
	maxAge             int
	profile            string
	regs               int
	delay, jitter      float64
	drop, exc, badcrc  float64
	frag               int
	fragGap            float64
	echo               bool
	latency            int
	gap                float64
	report             float64
	seed               uint64
	daemon, kill, stat bool
	showVersion        bool
	listProfiles       bool
	apiAddr, apiToken  string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "line-test" {
		os.Exit(lineTest(os.Args[2:]))
	}
	var o opts
	fs := flag.NewFlagSet("modbus-slave", flag.ExitOnError)
	fs.Var(&o.ports, "p", "serial port: /dev/ttyUSB0, /dev/ttyS7, or FT4232H channel 0..3 (A..D, survives re-plug); repeat for several lines")
	fs.Var(&o.tcps, "tcp", "serve Modbus TCP on this address (:502, 0.0.0.0:1502) instead of a serial port; repeatable")
	fs.StringVar(&o.state, "state", "", "file that keeps the line settings changed over the API (port, speed, parity, TCP) over a restart")
	fs.Var(&o.ptys, "pty", "serve on a new pty and point this symlink at it (no hardware, for tests); repeatable")
	fs.IntVar(&o.baud, "b", 19200, "baud rate, any value the UART can do (1200 .. 4000000)")
	fs.StringVar(&o.mode, "o", "8N1", "port mode: 8N1 8E1 8O1 7E1 8N2")
	fs.IntVar(&o.count, "a", 1, "number of slaves, unit IDs 1..n")
	fs.StringVar(&o.units, "units", "", "unit IDs instead of -a, e.g. 1-4,7")
	fs.Var(&o.files, "f", "[line.]id:file — register values of slave id from a text file (one value per line); repeatable")
	fs.IntVar(&o.maxAge, "t", 10, "max file age, s: an older file gives zeros (0 = no check)")
	fs.StringVar(&o.profile, "profile", "generic", "device: generic (v1: N holding registers, random or -f file), a built-in profile (--list-profiles) or a YAML file")
	fs.BoolVar(&o.listProfiles, "list-profiles", false, "list the built-in profiles")
	fs.IntVar(&o.regs, "regs", 20, "generic: number of registers")
	fs.Float64Var(&o.delay, "delay", 0, "answer delay, ms")
	fs.Float64Var(&o.jitter, "jitter", 0, "random extra answer delay 0..N ms")
	fs.Float64Var(&o.drop, "drop", 0, "probability of not answering")
	fs.Float64Var(&o.exc, "exc", 0, "probability of exception 4 instead of the answer")
	fs.Float64Var(&o.badcrc, "badcrc", 0, "probability of a broken CRC in the answer")
	fs.IntVar(&o.frag, "frag", 0, "send the answer in N pieces")
	fs.Float64Var(&o.fragGap, "frag-gap", 5, "pause between pieces, ms")
	fs.BoolVar(&o.echo, "echo", false, "discard the local echo of our answers (transceiver hears itself)")
	fs.IntVar(&o.latency, "latency", 1, "FTDI latency_timer, ms (0 = leave as is)")
	fs.Float64Var(&o.gap, "gap", 20, "silence that discards a partial frame, ms (0 = t3.5 of the baud rate)")
	fs.Float64Var(&o.report, "report", 60, "statistics interval, s (0 = off)")
	fs.Uint64Var(&o.seed, "seed", 0, "random seed for repeatable runs (0 = new every start)")
	fs.StringVar(&o.apiAddr, "api", "", "local JSON API: 127.0.0.1:PORT or unix:/path (off by default)")
	fs.StringVar(&o.apiToken, "api-token-file", "", "file with a token for the API (Authorization: Bearer); needed for a non-loopback --api")
	fs.BoolVar(&o.daemon, "d", false, "run as a background daemon (logs -> syslog); with systemd use the unit instead")
	fs.BoolVar(&o.kill, "k", false, "stop the running daemon")
	fs.BoolVar(&o.stat, "s", false, "show daemon status")
	fs.BoolVar(&o.showVersion, "version", false, "print version")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "modbus-slave %s — Modbus RTU / TCP slave emulator\n\nUsage:\n  modbus-slave -p <port> -b <baud> [-a n] [-o 8N1] [-f id:file] [-t s] [options]\n  modbus-slave --tcp :502 [-a n] [options]\n  modbus-slave -k | -s\n  modbus-slave line-test send|listen -p <port> [-p ...] -b <baud>   (raw line test, see line-test -h)\n\nOptions:\n", version)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, usageExamples())
	}
	fs.Parse(os.Args[1:])
	if o.showVersion {
		fmt.Println("modbus-slave", version)
		return
	}
	if o.listProfiles {
		for _, n := range profile.Builtin() {
			p, _ := profile.Load(n)
			fmt.Printf("%-12s %s\n", n, p.Description)
		}
		return
	}
	if o.kill || o.stat {
		os.Exit(daemonControl(o.kill))
	}
	if len(o.ports) == 0 && len(o.ptys) == 0 && len(o.tcps) == 0 {
		fs.Usage()
		pauseIfOwnConsole()
		os.Exit(2)
	}
	if o.daemon {
		os.Exit(daemonize())
	}
	if o.seed == 0 {
		o.seed = uint64(time.Now().UnixNano())
	}
	cleanup := daemonChild()
	err := run(o)
	cleanup()
	if err != nil {
		log.Fatal(err)
	}
}

func parseUnits(s string, count int) ([]byte, error) {
	if s == "" {
		if count < 1 || count > 247 {
			return nil, fmt.Errorf("-a %d: must be 1..247", count)
		}
		s = "1-" + strconv.Itoa(count)
	}
	var out []byte
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		a, b, _ := strings.Cut(part, "-")
		lo, err1 := strconv.Atoi(a)
		hi := lo
		var err2 error
		if b != "" {
			hi, err2 = strconv.Atoi(b)
		}
		if err1 != nil || err2 != nil || lo < 1 || hi > 247 || lo > hi {
			return nil, fmt.Errorf("bad unit list %q (want e.g. 1-4,7; IDs 1..247)", s)
		}
		for u := lo; u <= hi; u++ {
			if !seen[u] {
				seen[u] = true
				out = append(out, byte(u))
			}
		}
	}
	return out, nil
}

// parseFiles: "[line.]id:path" -> files[line][id]; line -1 = every line.
func parseFiles(specs []string) (map[int]map[byte]string, error) {
	out := map[int]map[byte]string{}
	for _, s := range specs {
		key, path, ok := strings.Cut(s, ":")
		if !ok || path == "" {
			return nil, fmt.Errorf("-f %q: want id:file or line.id:file", s)
		}
		line, id := -1, key
		if l, i, ok := strings.Cut(key, "."); ok {
			n, err := strconv.Atoi(l)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("-f %q: bad line", s)
			}
			line, id = n, i
		}
		u, err := strconv.Atoi(id)
		if err != nil || u < 1 || u > 247 {
			return nil, fmt.Errorf("-f %q: bad slave id", s)
		}
		if out[line] == nil {
			out[line] = map[byte]string{}
		}
		out[line][byte(u)] = path
	}
	return out, nil
}

func run(o opts) error {
	cfg, err := serial.ParseMode(o.mode)
	if err != nil {
		return err
	}
	cfg.Baud = o.baud
	units, err := parseUnits(o.units, o.count)
	if err != nil {
		return err
	}
	files, err := parseFiles(o.files)
	if err != nil {
		return err
	}
	if o.profile != fleet.Generic {
		if _, err = profile.Load(o.profile); err != nil {
			return err
		}
		if len(files) > 0 {
			return fmt.Errorf("-f works with --profile generic; in a YAML profile use file: in the register map")
		}
	}
	// the sensors of the command line: every unit on every port
	var sensors []fleet.Sensor
	specs := append(append(append([]string{}, o.ports...), o.ptys...), o.tcps...)
	for i, spec := range specs {
		for _, u := range units {
			s := fleet.Sensor{Profile: o.profile, Unit: int(u), Line: i, Transport: "rtu", Port: spec, Baud: cfg.Baud, Mode: o.mode}
			switch {
			case i >= len(o.ports)+len(o.ptys):
				s.Transport, s.Port, s.Listen = "tcp", "", spec
			case i >= len(o.ports):
				s.Pty = true
			}
			if path := cmp(files[i][u], files[-1][u]); path != "" {
				s.File = path
			}
			sensors = append(sensors, s)
		}
	}
	crazy := fleet.DefaultCrazy
	if o.state != "" {
		saved, c, err := loadState(o.state) // "stop" lasts until start or a restart of the service
		switch {
		case err != nil:
			log.Printf("state %s: %v (ignored)", o.state, err)
		case len(saved) > 0:
			log.Printf("state %s: %d sensor(s) as saved (the ports and units of the command line are not used)", o.state, len(saved))
			sensors = saved
		}
		if c != nil {
			crazy = *c
		}
	}
	opt := fleet.Options{
		Normal: server.Faults{Delay: ms(o.delay), Jitter: ms(o.jitter), Drop: o.drop, Exc: o.exc, BadCRC: o.badcrc,
			Frag: o.frag, FragGap: ms(o.fragGap), Echo: o.echo},
		Crazy:       crazy,
		Gap:         ms(o.gap), // 0 = t3.5 of the baud rate
		Latency:     o.latency,
		Regs:        o.regs,
		MaxAge:      time.Duration(o.maxAge) * time.Second,
		Seed:        o.seed,
		ProfileDirs: []string{"/etc/modbus-slave/profiles", "/usr/share/modbus-slave/profiles"},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("modbus-slave %s: %d sensor(s)", version, len(sensors))
	fl, err := fleet.New(ctx, opt, sensors)
	if err != nil {
		return err
	}
	if o.state != "" {
		fl.Save = func(ss []fleet.Sensor, c server.Faults, _ bool) error { return saveState(o.state, ss, c) }
	}

	for _, s := range fl.List() {
		desc := ""
		if s.File != "" {
			desc = ", values from " + s.File
		}
		log.Printf("  sensor %d: %s, address %d, %s%s", s.ID, s.Profile, s.Unit, settingsOf(s), desc)
	}
	if o.report > 0 {
		go reporter(ctx, fl.Lines, time.Duration(o.report*float64(time.Second)))
	}
	if o.apiAddr != "" {
		token := ""
		if o.apiToken != "" {
			b, err := os.ReadFile(o.apiToken)
			if err != nil {
				return err
			}
			token = strings.TrimSpace(string(b))
		}
		ln, err := api.Listen(o.apiAddr, token)
		if err != nil {
			return err
		}
		a := api.New(api.Info{Version: version}, fl, o.regs, token)
		go a.Serve(ln)
		go func() { <-ctx.Done(); ln.Close() }()
		log.Printf("api and web page on %s", o.apiAddr)
	}
	<-ctx.Done()
	fl.Wait()
	log.Printf("stopped")
	return nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func settingsOf(s fleet.Sensor) string {
	if s.Transport == "tcp" {
		return "Modbus TCP " + s.Listen
	}
	return fmt.Sprintf("RTU %s %d %s", s.Port, s.Baud, s.Mode)
}

// state file: the sensors and the crazy mode, as changed on the web page or over the API.
type stateFile struct {
	Sensors []fleet.Sensor `json:"sensors"`
	Crazy   *api.Faults    `json:"crazy,omitempty"`
}

func loadState(path string) ([]fleet.Sensor, *server.Faults, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, nil, err
	}
	var c *server.Faults
	if sf.Crazy != nil && sf.Crazy.Check() == nil {
		f := sf.Crazy.ToFaults()
		c = &f
	}
	return sf.Sensors, c, nil
}

func saveState(path string, ss []fleet.Sensor, crazy server.Faults) error {
	c := api.FaultsOf(crazy)
	b, err := json.MarshalIndent(stateFile{Sensors: ss, Crazy: &c}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ms(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func unitList(u []byte) string {
	if len(u) > 8 {
		return fmt.Sprintf("%d..%d (%d)", u[0], u[len(u)-1], len(u))
	}
	s := make([]string, len(u))
	for i, x := range u {
		s[i] = strconv.Itoa(int(x))
	}
	return strings.Join(s, ",")
}

func reporter(ctx context.Context, linesOf func() []*server.Line, every time.Duration) {
	t0 := time.Now()
	prev := map[*server.Line]server.Snapshot{} // a line restarted with new settings starts again from 0
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		var parts []string
		lines := linesOf()
		seen := map[*server.Line]server.Snapshot{}
		for _, l := range lines {
			s := l.Stats.Snapshot()
			p := fmt.Sprintf("line %d: %d ans %.0f/s", l.Index, s.Ans, float64(s.Ans-prev[l].Ans)/every.Seconds())
			var bad []string
			for _, kv := range []struct {
				k string
				v uint64
			}{{"exc", s.Exc}, {"foreign", s.Foreign}, {"bcast", s.Bcast}, {"resync", s.Resync}, {"drop", s.Drop}, {"badcrc", s.BadCRC}} {
				if kv.v > 0 {
					bad = append(bad, fmt.Sprintf("%s %d", kv.k, kv.v))
				}
			}
			if len(bad) > 0 {
				p += " (" + strings.Join(bad, ", ") + ")"
			}
			parts = append(parts, p)
			seen[l] = s
		}
		prev = seen
		log.Printf("[%6.0fs] %s", time.Since(t0).Seconds(), strings.Join(parts, "  "))
	}
}

// usageExamples: ready-to-run commands with the usual parameters, printed after the options
// (a first start without parameters should show how to actually start it and open the page).
func usageExamples() string {
	port, open, list := "/dev/ttyUSB0", "xdg-open http://127.0.0.1:8650", "ls /dev/serial/by-path/"
	exe := "modbus-slave"
	if runtime.GOOS == "windows" {
		port, open, list, exe = "COM5", "start http://127.0.0.1:8650", "mode   (or Device Manager -> Ports)", `.\modbus-slave.exe`
	}
	return fmt.Sprintf(`
Examples:
  # 4 KTH2-R sensors (addresses 1-4) on a serial port, 9600 8N1, with the web page
  %[1]s -p %[2]s -b 9600 --profile kth2r --units 1-4 --api 127.0.0.1:8650

  # the same over Modbus TCP, no hardware needed
  %[1]s --tcp :1502 --profile kth2r --units 1-4 --api 127.0.0.1:8650

  # then open the page in a browser (sensors, ports, registers are changed there):
  %[3]s

  # poll it (registers from 1 in mbpoll/modpoll: 769 = T/RH of KTH2-R):
  modpoll -m rtu -b 9600 -p none -a 1 -t 4 -r 769 -c 2 <port of the master>
  modpoll -m tcp -p 1502 -a 1 -t 4 -r 769 -c 2 127.0.0.1

  # serial ports of this machine: %[4]s
  # built-in device profiles:     %[1]s --list-profiles
`, exe, port, open, list)
}
