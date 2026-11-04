package main

// line-test: a raw test of the RS-485 line itself, without Modbus. One side sends, the other listens;
// all ports at once (crosstalk counts), then swap the roles.
//
//	modbus-slave line-test listen -p 0 -p 1 -p 2 -p 3 -b 57600 --secs 6     # start first
//	modbus-slave line-test send   -p 0 -p 1 -p 2 -p 3 -b 57600 --secs 3
//
// tokens (default): "<P1 00042>" = sending port + counter; the listener tells which port reached it
// (crossed pairs) and how many bytes are wrong. bytes: 00..FF repeated; the listener counts the bits
// that flipped 0->1 and 1->0 (a slow line loses a lone 0 after 1s: 03 arrives as 07).

import (
	"bytes"
	"flag"
	"fmt"
	"math/bits"
	"os"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/lab240/modbus-slave/internal/serial"
)

var tokenRe = regexp.MustCompile(`<P(\d) (\d{5})>`)

func lineTest(args []string) int {
	fs := flag.NewFlagSet("line-test", flag.ExitOnError)
	var ports multi
	fs.Var(&ports, "p", "port or FT4232H channel; repeat for several lines")
	baud := fs.Int("b", 9600, "baud rate")
	mode := fs.String("o", "8N1", "port mode")
	secs := fs.Float64("secs", 3, "send: how long to send; listen: how long to listen (start the listener first, give it 3 s more)")
	pattern := fs.String("pattern", "tokens", "tokens (which port, how many wrong bytes) | bytes (00..FF, which bits flip)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: modbus-slave line-test send|listen -p <port> [-p ...] -b <baud> [--secs s] [--pattern tokens|bytes]\n\n")
		fs.PrintDefaults()
	}
	if len(args) == 0 || (args[0] != "send" && args[0] != "listen") {
		fs.Usage()
		return 2
	}
	role := args[0]
	fs.Parse(args[1:])
	if len(ports) == 0 || (*pattern != "tokens" && *pattern != "bytes") {
		fs.Usage()
		return 2
	}
	cfg, err := serial.ParseMode(*mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg.Baud = *baud
	conns := make([]serial.Conn, len(ports))
	for i, p := range ports {
		dev := serial.Resolve(p)
		if dev == "" {
			fmt.Fprintf(os.Stderr, "port %s: not present\n", p)
			return 1
		}
		serial.SetLatency(dev, 1)
		if conns[i], err = serial.Open(dev, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "port %s: %v\n", p, err)
			return 1
		}
		defer conns[i].Close()
	}
	dur := time.Duration(*secs * float64(time.Second))
	if role == "send" {
		return send(conns, cfg, dur, *pattern)
	}
	return listen(conns, ports, cfg, dur, *pattern)
}

func payload(port int, n int, pattern string) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		if pattern == "bytes" {
			b.WriteByte(byte(i))
		} else {
			fmt.Fprintf(&b, "<P%d %05d>", port%10, i%100000)
		}
	}
	return b.Bytes()[:n]
}

func send(conns []serial.Conn, cfg serial.Config, dur time.Duration, pattern string) int {
	n := int(float64(cfg.Baud) / float64(cfg.BitsPerChar()) * dur.Seconds())
	if pattern == "tokens" {
		n -= n % 10
	}
	var wg sync.WaitGroup
	t0 := time.Now()
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Write(payload(i, n, pattern))
			c.Drain()
		}()
	}
	wg.Wait()
	fmt.Printf("sent %d bytes x %d ports at %s in %.1f s\n", n, len(conns), cfg, time.Since(t0).Seconds())
	return 0
}

func listen(conns []serial.Conn, ports []string, cfg serial.Config, dur time.Duration, pattern string) int {
	got := make([][]byte, len(conns))
	var wg sync.WaitGroup
	end := time.Now().Add(dur)
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 65536)
			for time.Now().Before(end) {
				ok, err := c.Wait(200 * time.Millisecond)
				if err != nil {
					return
				}
				if ok {
					n, _ := c.Read(buf)
					got[i] = append(got[i], buf[:n]...)
				}
			}
		}()
	}
	wg.Wait()
	fmt.Printf("listened %.0f s at %s\n", dur.Seconds(), cfg)
	return report(got, ports, pattern)
}

// report prints a verdict per port; 1 = something is not clean.
func report(got [][]byte, ports []string, pattern string) int {
	bad := 0
	for i, g := range got {
		var verdict string
		if pattern == "tokens" {
			m := tokenRe.FindAllSubmatch(g, -1)
			from := map[int]int{}
			for _, t := range m {
				p, _ := strconv.Atoi(string(t[1]))
				from[p]++
			}
			var src []string
			for _, p := range sortedKeys(from) {
				src = append(src, fmt.Sprintf("P%d x%d", p, from[p]))
			}
			junk := len(g) - 10*len(m)
			switch {
			case len(g) == 0:
				verdict = "nothing"
			case junk == 0:
				verdict = "clean"
			default:
				verdict = fmt.Sprintf("%d wrong bytes (%.0f%%)", junk, 100*float64(junk)/float64(len(g)))
			}
			fmt.Printf("  %-12s %7d bytes  %-28s from %v\n", ports[i], len(g), verdict, src)
			if len(g) == 0 || junk > 0 || len(from) != 1 {
				bad++
			}
			continue
		}
		// bytes: follow the running counter. Over the whole stream count the bytes that differ from it;
		// the bit statistics only over the first 64 bytes: after the first lost byte the alignment is a guess.
		const window = 64
		var wrong, up, down int
		var want byte
		if len(g) > 0 {
			want = g[0]
		}
		for k, b := range g {
			if b != want {
				wrong++
				if k < window {
					up += bits.OnesCount8(b &^ want)
					down += bits.OnesCount8(want &^ b)
				}
			}
			want++
		}
		verdict = "clean"
		if len(g) == 0 {
			verdict = "nothing"
		} else if wrong > 0 {
			verdict = fmt.Sprintf("%d bytes off the counter; first %d bytes: bits 0->1 %d, 1->0 %d", wrong, min(window, len(g)), up, down)
		}
		fmt.Printf("  %-12s %7d bytes  %s\n", ports[i], len(g), verdict)
		if len(g) > 0 && len(g) <= 16 || wrong > 0 {
			fmt.Printf("               first bytes: % X\n", g[:min(len(g), 16)])
		}
		if len(g) == 0 || wrong > 0 {
			bad++
		}
	}
	if bad > 0 {
		fmt.Printf("RESULT: %d of %d ports not clean\n", bad, len(got))
		return 1
	}
	fmt.Println("RESULT: all ports clean")
	return 0
}

func sortedKeys(m map[int]int) []int {
	k := make([]int, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Ints(k)
	return k
}
