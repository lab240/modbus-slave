package serial

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// List finds the serial ports: USB adapters (ttyUSB, ttyACM) and on-board UARTs that have a driver.
// The 8250 placeholders the kernel creates without hardware (serial8250 ttyS*) are left out.
func List() []PortInfo {
	byPath := map[string]string{}
	for _, dir := range []string{"/dev/serial/by-path", "/dev/serial/by-id"} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.Contains(e.Name(), "-usbv") { // older udev aliases (usbv2-...) duplicate the usb-... ones
				continue
			}
			link := filepath.Join(dir, e.Name())
			if real, err := filepath.EvalSymlinks(link); err == nil {
				if _, seen := byPath[real]; !seen {
					byPath[real] = link
				}
			}
		}
	}
	consoles := map[string]bool{}
	if b, err := os.ReadFile("/proc/cmdline"); err == nil {
		for _, f := range strings.Fields(string(b)) {
			if c, ok := strings.CutPrefix(f, "console="); ok {
				consoles[strings.SplitN(c, ",", 2)[0]] = true
			}
		}
	}
	var out []PortInfo
	ents, _ := os.ReadDir("/sys/class/tty")
	for _, e := range ents {
		name := e.Name()
		if !hasAnyPrefix(name, "ttyUSB", "ttyACM", "ttyS", "ttyAMA", "ttymxc", "ttySC", "ttyXRUSB", "ttyRS485", "ttyO") {
			continue
		}
		devDir, err := filepath.EvalSymlinks(filepath.Join("/sys/class/tty", name, "device"))
		if err != nil {
			continue // virtual
		}
		if drv, err := filepath.EvalSymlinks(filepath.Join(devDir, "driver")); err == nil && filepath.Base(drv) == "serial8250" {
			continue
		}
		if t, err := os.ReadFile(filepath.Join("/sys/class/tty", name, "type")); err == nil && strings.TrimSpace(string(t)) == "0" {
			continue // a UART slot without hardware (8250 nr_uarts)
		}
		dev := "/dev/" + name
		p := PortInfo{Path: dev, Dev: dev, Info: usbInfo(devDir)}
		if consoles[name] {
			p.Info = strings.TrimSpace(p.Info + " system console")
		}
		if l, ok := byPath[dev]; ok {
			p.Path = l
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Dev, out[j].Dev
		if len(a) != len(b) && strings.TrimRight(a, "0123456789") == strings.TrimRight(b, "0123456789") {
			return len(a) < len(b)
		}
		return a < b
	})
	return out
}

func hasAnyPrefix(s string, p ...string) bool {
	for _, x := range p {
		if strings.HasPrefix(s, x) {
			return true
		}
	}
	return false
}

// usbInfo walks up from the tty's device to the USB device and reads its strings.
func usbInfo(dir string) string {
	for i := 0; i < 5 && dir != "/"; i++ {
		if prod, err := os.ReadFile(filepath.Join(dir, "product")); err == nil {
			var parts []string
			for _, f := range []string{"manufacturer", "product", "serial"} {
				if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
					if v := strings.TrimSpace(string(b)); v != "" && !(f == "manufacturer" && strings.Contains(string(prod), v)) {
						parts = append(parts, v)
					}
				}
			}
			return strings.Join(parts, " ")
		}
		dir = filepath.Dir(dir)
	}
	return ""
}
