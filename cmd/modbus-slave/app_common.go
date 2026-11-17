//go:build windows || linux

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// appSetup: the start with no arguments (the program in a window): 3 sensors with random registers
// on Modbus TCP :1502 at first, then the sensors as changed in the window (dir/state.json); the page
// on the first free 127.0.0.1:8650..8659.
func appSetup(o *opts, dir string) {
	os.MkdirAll(dir, 0o755)
	o.state = filepath.Join(dir, "state.json")
	o.tcps = multi{":1502"}
	o.profile, o.units = "generic", "1-3"
	o.apiAddr = freeAPI()
	fmt.Printf("modbus-slave %s: эмулятор датчиков Modbus\n\n", version)
	fmt.Printf("  страница:  http://%s/\n", o.apiAddr)
	fmt.Printf("  датчики:   Modbus TCP, порт 1502; COM-порт, скорость и датчики меняются в окне\n")
	fmt.Printf("  настройки: %s\n\n", o.state)
}

// running: the page of a modbus-slave already on 127.0.0.1:8650..8659.
func running() (string, bool) {
	c := http.Client{Timeout: 700 * time.Millisecond}
	for p := 8650; p < 8660; p++ {
		url := fmt.Sprintf("http://127.0.0.1:%d/", p)
		r, err := c.Get(url + "api/v1/status")
		if err != nil {
			continue
		}
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		r.Body.Close()
		if strings.Contains(string(b[:n]), "version") {
			return url, true
		}
	}
	return "", false
}

// freeAPI: 127.0.0.1:8650, or the next free port when another program holds it.
func freeAPI() string {
	for p := 8650; p < 8660; p++ {
		a := fmt.Sprintf("127.0.0.1:%d", p)
		if l, err := net.Listen("tcp", a); err == nil {
			l.Close()
			return a
		}
	}
	return "127.0.0.1:8650"
}
