package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

var (
	kernel32    = windows.NewLazySystemDLL("kernel32.dll")
	user32      = windows.NewLazySystemDLL("user32.dll")
	pConsoleWnd = kernel32.NewProc("GetConsoleWindow")
	pShowWindow = user32.NewProc("ShowWindow")
	pFindWindow = user32.NewProc("FindWindowW")
	pForeground = user32.NewProc("SetForegroundWindow")
	pDpi        = user32.NewProc("GetDpiForSystem")
)

// the window of the program lives on the main thread
func init() { runtime.LockOSThread() }

// ownConsole: started by a double click (or from the Start menu), the console window is ours alone.
// From cmd / PowerShell / ssh it is shared, or stdin is not a console at all.
func ownConsole() bool {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) != nil {
		return false
	}
	var ids [2]uint32
	n, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&ids[0])), 2)
	return n == 1
}

var appDir string

// appMode: a start with no arguments (a double click, a shortcut, a console) is the program in a
// window of its own (WebView2, the Edge engine of Windows 10/11): 3 sensors with random registers
// on Modbus TCP :1502 at first, then the sensors as changed in the window
// (%LOCALAPPDATA%\modbus-slave\state.json). Closing the window stops the emulator. A second start
// brings the window of the first one to the front.
func appMode(o *opts) bool {
	if len(os.Args) > 1 {
		return false
	}
	kernel32.NewProc("SetConsoleTitleW").Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("modbus-slave " + version))))
	appDir = os.Getenv("LOCALAPPDATA")
	if appDir == "" {
		appDir, _ = os.UserConfigDir()
	}
	appDir = filepath.Join(appDir, "modbus-slave")
	os.MkdirAll(appDir, 0o755)
	if url, ok := running(); ok {
		if h, _, _ := pFindWindow.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("webview"))),
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("modbus-slave")))); h != 0 {
			pShowWindow.Call(h, 9) // SW_RESTORE
			pForeground.Call(h)
		} else {
			openBrowser(url)
		}
		os.Exit(0)
	}
	o.state = filepath.Join(appDir, "state.json")
	o.tcps = multi{":1502"}
	o.profile, o.units = "generic", "1-3" // 3 sensors with random registers
	o.apiAddr = freeAPI()
	fmt.Printf("modbus-slave %s: эмулятор датчиков Modbus\n\n", version)
	fmt.Printf("  страница:  http://%s/\n", o.apiAddr)
	fmt.Printf("  датчики:   Modbus TCP, порт 1502; COM-порт, скорость и датчики меняются в окне\n")
	fmt.Printf("  настройки: %s\n\n", o.state)
	return true
}

// appRun: the emulator in the background, its page in the window on the main thread; the window
// closed, the program ends (the sensors are saved on every change).
func appRun(o opts) {
	ready := make(chan struct{})
	afterAPI = func() { close(ready) }
	errc := make(chan error, 1)
	go func() { errc <- run(o) }()
	select {
	case <-ready:
	case err := <-errc:
		log.Print(err)
		pauseIfOwnConsole()
		os.Exit(1)
	}
	url := "http://" + o.apiAddr + "/"
	con, _, _ := pConsoleWnd.Call()
	hide := ownConsole() && con != 0
	if hide {
		pShowWindow.Call(con, 0) // SW_HIDE
	}
	scale := uint(96)
	if d, _, _ := pDpi.Call(); d != 0 {
		scale = uint(d)
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(appDir, "webview"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title: "modbus-slave", IconId: 1, Center: true,
			Width: 1280 * scale / 96, Height: 860 * scale / 96,
		},
	})
	if w == nil { // no WebView2 runtime: the default browser, the console stays
		if hide {
			pShowWindow.Call(con, 5) // SW_SHOW
		}
		openBrowser(url)
		fmt.Printf("Страница открыта в браузере. Пока это окно открыто, эмулятор работает. Закрыть окно: остановить.\n\n")
		if err := <-errc; err != nil {
			log.Print(err)
			pauseIfOwnConsole()
		}
		os.Exit(0)
	}
	w.Navigate(url)
	w.Run()
	w.Destroy()
	os.Exit(0)
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

func openBrowser(url string) {
	windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(url), nil, nil, windows.SW_SHOWNORMAL)
}
