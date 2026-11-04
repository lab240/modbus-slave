//go:build unix

package main

import (
	"fmt"
	"log"
	"log/syslog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// As v1: -d detaches (logs -> syslog, pid in /var/run/modbus_slave.pid or /tmp/modbus_slave.pid),
// -k stops it, -s shows the status. Go cannot fork(), so -d starts a copy of itself in a new session.

const envDaemon = "MODBUS_SLAVE_DAEMON"

var pidFiles = []string{"/var/run/modbus_slave.pid", "/tmp/modbus_slave.pid"}

func daemonize() int {
	if pid := readPid(); pid > 0 {
		fmt.Fprintf(os.Stderr, "modbus-slave is already running, pid %d\n", pid)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var args []string
	for _, a := range os.Args[1:] {
		if a != "-d" && a != "--d" {
			args = append(args, a)
		}
	}
	null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	p, err := os.StartProcess(exe, append([]string{os.Args[0]}, args...), &os.ProcAttr{
		Env:   append(os.Environ(), envDaemon+"=1"),
		Files: []*os.File{null, null, null},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("modbus-slave started in background, pid %d (logs: journalctl -t modbus_slave)\n", p.Pid)
	return 0
}

// daemonChild runs in the detached copy: log to syslog, write the pid file. Returns a cleanup func.
func daemonChild() func() {
	if os.Getenv(envDaemon) != "1" {
		return func() {}
	}
	if w, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "modbus_slave"); err == nil {
		log.SetOutput(w)
		log.SetFlags(0)
	}
	var used string
	for _, f := range pidFiles {
		if os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644) == nil {
			used = f
			break
		}
	}
	return func() {
		if used != "" {
			os.Remove(used)
		}
	}
}

func readPid() int {
	for _, f := range pidFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err == nil && pid > 0 && syscall.Kill(pid, 0) == nil {
			return pid
		}
	}
	return 0
}

func daemonControl(kill bool) int {
	pid := readPid()
	if pid == 0 {
		fmt.Println("modbus-slave is not running")
		if kill {
			return 1
		}
		return 0
	}
	if !kill {
		fmt.Printf("modbus-slave is running, pid %d\n", pid)
		return 0
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		fmt.Printf("pid %d did not stop\n", pid)
		return 1
	}
	fmt.Printf("modbus-slave stopped (pid %d)\n", pid)
	return 0
}
