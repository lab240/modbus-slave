package main

import (
	"bufio"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pauseIfOwnConsole: started by a double click, the console window is ours alone and closes with the
// program, so wait for Enter to let the help be read. From cmd / PowerShell / ssh it does not wait.
func pauseIfOwnConsole() {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) != nil {
		return // stdin is a pipe or a file (ssh, a script)
	}
	var ids [2]uint32
	n, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&ids[0])), 2)
	if n == 1 {
		fmt.Fprint(os.Stderr, "\nPress Enter to close this window...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
}
