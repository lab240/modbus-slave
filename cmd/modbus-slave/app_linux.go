package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/lab240/modbus-slave/internal/api"
)

// appMode: a start with no arguments on a desktop (X11 or Wayland) is the program in a window:
// Chrome/Chromium as an app window (no address bar, no tabs), else the default browser. Closing the
// window stops the emulator. Without a display (ssh) it is the help, as before. A second start
// opens a window of the first one.
func appMode(o *opts) bool {
	if len(os.Args) > 1 || (os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "") {
		return false
	}
	if url, ok := running(); ok {
		fmt.Println("modbus-slave уже запущен, открываю его окно:", url)
		openBrowser(url)
		os.Exit(0)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	appSetup(o, filepath.Join(dir, "modbus-slave"))
	fmt.Printf("Закройте окно или нажмите Ctrl+C, чтобы остановить.\n\n")
	return true
}

// appRun: the emulator and its window. The page says bye when closed (a reload comes back within a
// few seconds); failing that (a crashed browser) it stops when the page has not been seen for 3
// minutes: a hidden page polls rarely.
func appRun(o opts) {
	var seen, bye atomic.Int64
	seen.Store(time.Now().Add(time.Minute).UnixMilli()) // the browser takes a while to start
	onAPI = func(a *api.API) {
		a.Seen = func() { seen.Store(time.Now().UnixMilli()) }
		a.Bye = func() { bye.Store(time.Now().UnixMilli()) }
	}
	afterAPI = func() {
		go openBrowser("http://" + o.apiAddr + "/")
		go func() {
			for range time.Tick(time.Second) {
				now, s, b := time.Now().UnixMilli(), seen.Load(), bye.Load()
				if (b > 0 && b >= s && now-b > 4000) || now-s > 180000 {
					log.Printf("the window is closed: stopping")
					os.Exit(0) // the sensors are saved on every change
				}
			}
		}()
	}
	if err := run(o); err != nil {
		log.Fatal(err)
	}
}

// openBrowser: an --app window of Chrome/Chromium looks like a program; else the default browser.
func openBrowser(url string) {
	for _, b := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "microsoft-edge"} {
		if p, err := exec.LookPath(b); err == nil {
			if exec.Command(p, "--app="+url, "--window-size=1280,860").Start() == nil {
				return
			}
		}
	}
	if exec.Command("xdg-open", url).Start() != nil {
		fmt.Println("Откройте в браузере:", url)
	}
}
