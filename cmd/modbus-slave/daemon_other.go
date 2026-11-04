//go:build !unix

package main

import (
	"fmt"
	"os"
)

func daemonize() int {
	fmt.Fprintln(os.Stderr, "-d is not available here: run it with `start /b modbus-slave ...` or as a service")
	return 2
}

func daemonChild() func() { return func() {} }

func daemonControl(kill bool) int {
	fmt.Fprintln(os.Stderr, "-k / -s are not available here")
	return 2
}
