// modbus-slave v2 — Modbus RTU slave emulator / soft sensor.
//
// v1-compatible:  modbus-slave -p /dev/ttyUSB0 -b 115200 -a 5 -f 1:/tmp/cpu.dat -t 15 [-o 8N1] [-d | -k | -s]
// v2 additions:   several -p (each one is a line), FT4232H channels (-p 0..3 / A..D), --pty LINK,
//
//	--profile kth2r | file.yaml (YAML device profiles), --units 1-4,7, fault injection, any baud rate.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lab240/modbus-slave/internal/api"
	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/profile"
	"github.com/lab240/modbus-slave/internal/rtu"
	"github.com/lab240/modbus-slave/internal/serial"
	"github.com/lab240/modbus-slave/internal/server"
	"github.com/lab240/modbus-slave/internal/source"
)

var version = "2.0.0-dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

type opts struct {
	ports, ptys, files multi
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
		fmt.Fprintf(os.Stderr, "modbus-slave %s — Modbus RTU slave emulator\n\nUsage:\n  modbus-slave -p <port> -b <baud> [-a n] [-o 8N1] [-f id:file] [-t s] [options]\n  modbus-slave -k | -s\n  modbus-slave line-test send|listen -p <port> [-p ...] -b <baud>   (raw line test, see line-test -h)\n\nOptions:\n", version)
		fs.PrintDefaults()
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
	if len(o.ports) == 0 && len(o.ptys) == 0 {
		fs.Usage()
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
	var prof *profile.Profile
	if o.profile != "generic" {
		if prof, err = profile.Load(o.profile); err != nil {
			return err
		}
		if len(files) > 0 {
			return fmt.Errorf("-f works with --profile generic; in a YAML profile use file: in the register map")
		}
	}
	env := profile.Env{Baud: cfg.Baud, Parity: cfg.Parity, Seed: o.seed, Start: time.Now(), Files: &profile.Files{}}
	gap := time.Duration(o.gap * float64(time.Millisecond))
	if gap <= 0 {
		gap = rtu.T35(cfg.Baud, cfg.BitsPerChar())
	}
	f := server.Faults{
		Delay: ms(o.delay), Jitter: ms(o.jitter), Drop: o.drop, Exc: o.exc, BadCRC: o.badcrc,
		Frag: o.frag, FragGap: ms(o.fragGap), Echo: o.echo,
	}
	specs := append(append([]string{}, o.ports...), o.ptys...)
	var lines []*server.Line
	for i, spec := range specs {
		var devs []*device.Device
		for _, u := range units {
			if prof != nil {
				e := env
				e.Line, e.Unit = i, u
				devs = append(devs, profile.Build(prof, e))
				continue
			}
			var src source.Block = source.NewRandom(o.seed + uint64(i)*1000 + uint64(u))
			path := files[i][u]
			if path == "" {
				path = files[-1][u]
			}
			if path != "" {
				src = &source.File{Path: path, MaxAge: time.Duration(o.maxAge) * time.Second}
			}
			devs = append(devs, device.NewGeneric(u, o.regs, src))
		}
		lines = append(lines, server.NewLine(i, spec, devs, f, gap, o.seed))
	}
	log.Printf("modbus-slave %s: %d line(s) %s, %s, profile %s, units %s", version, len(lines),
		strings.Join(specs, ","), cfg, o.profile, unitList(units))
	for _, l := range lines {
		for _, d := range l.Devices {
			if g, ok := d.Regs.(*device.Generic); ok {
				log.Printf("  line %d id=%-3d src=%s", l.Index, d.Unit(), g.Src.Describe())
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup
	for i, l := range lines {
		wg.Add(1)
		isPty := i >= len(o.ports)
		go func() { defer wg.Done(); keepServing(ctx, l, cfg, o.latency, isPty) }()
	}
	if o.report > 0 {
		go reporter(ctx, lines, time.Duration(o.report*float64(time.Second)))
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
		a := api.New(api.Info{Version: version, Profile: o.profile, Mode: cfg.String(), Baud: cfg.Baud}, lines, token)
		go a.Serve(ln)
		go func() { <-ctx.Done(); ln.Close() }()
		log.Printf("api on %s", o.apiAddr)
	}
	wg.Wait()
	for i, l := range lines {
		if i >= len(o.ports) {
			os.Remove(l.Spec)
		}
	}
	log.Printf("stopped")
	return nil
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

// keepServing opens the line and serves it; when the port fails (USB re-plug) it waits and reopens.
func keepServing(ctx context.Context, l *server.Line, cfg serial.Config, latency int, isPty bool) {
	lastErr := ""
	for ctx.Err() == nil {
		var c serial.Conn
		var err error
		if isPty {
			c, err = serial.OpenPty(l.Spec)
		} else if dev := serial.Resolve(l.Spec); dev == "" {
			err = fmt.Errorf("channel %s not present", l.Spec)
		} else {
			if latency > 0 {
				serial.SetLatency(dev, latency) // a re-plugged FTDI starts again at 16 ms
			}
			c, err = serial.Open(dev, cfg)
		}
		if err == nil {
			if lastErr != "" {
				log.Printf("line %d (%s): back on %s", l.Index, l.Spec, c.Name())
				lastErr = ""
			} else {
				log.Printf("line %d: serving %s", l.Index, c.Name())
			}
			err = l.Serve(ctx, c)
			c.Close()
			if ctx.Err() != nil {
				return
			}
		}
		if err.Error() != lastErr { // log once per new error, not every second
			log.Printf("line %d (%s): %v, waiting", l.Index, l.Spec, err)
			lastErr = err.Error()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func reporter(ctx context.Context, lines []*server.Line, every time.Duration) {
	t0 := time.Now()
	prev := make([]server.Snapshot, len(lines))
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		var parts []string
		for i, l := range lines {
			s := l.Stats.Snapshot()
			p := fmt.Sprintf("line %d: %d ans %.0f/s", l.Index, s.Ans, float64(s.Ans-prev[i].Ans)/every.Seconds())
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
			prev[i] = s
		}
		log.Printf("[%6.0fs] %s", time.Since(t0).Seconds(), strings.Join(parts, "  "))
	}
}
