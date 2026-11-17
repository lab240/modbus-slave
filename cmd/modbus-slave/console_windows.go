package main

import (
	"bufio"
	"fmt"
	"os"
)

// pauseIfOwnConsole: started by a double click, the console window is ours alone and closes with the
// program, so wait for Enter to let the text be read. From cmd / PowerShell / ssh it does not wait.
func pauseIfOwnConsole() {
	if ownConsole() {
		fmt.Fprint(os.Stderr, "\nНажмите Enter, чтобы закрыть окно...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
}
