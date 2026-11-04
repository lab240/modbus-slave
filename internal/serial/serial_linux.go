package serial

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type fdConn struct {
	fd   int
	name string
	pty  bool // master end of a pty: no client is not an error
}

func (c *fdConn) Name() string { return c.name }

func (c *fdConn) Wait(timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLIN}}
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	n, err := unix.Poll(fds, ms)
	if err == unix.EINTR {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if fds[0].Revents&unix.POLLIN != 0 {
		return true, nil
	}
	if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
		if c.pty { // nobody has the other end open: wait for a client
			time.Sleep(timeout)
			return false, nil
		}
		return false, ErrHangup
	}
	return false, nil
}

func (c *fdConn) Read(p []byte) (int, error) {
	n, err := unix.Read(c.fd, p)
	if err == unix.EAGAIN || err == unix.EINTR {
		return 0, nil
	}
	if c.pty && err == unix.EIO { // the client closed its end
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if n == 0 && !c.pty {
		return 0, ErrHangup // readable but empty: the USB device is gone
	}
	return n, nil
}

func (c *fdConn) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		n, err := unix.Write(c.fd, p[done:])
		if err == unix.EAGAIN || err == unix.EINTR {
			fds := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLOUT}}
			unix.Poll(fds, 100)
			continue
		}
		if err != nil {
			return done, err
		}
		done += n
	}
	return done, nil
}

func (c *fdConn) Drain() error {
	if c.pty {
		return nil
	}
	return unix.IoctlSetInt(c.fd, unix.TCSBRK, 1) // = tcdrain()
}

func (c *fdConn) Close() error { return unix.Close(c.fd) }

// rawTermios sets raw mode and any baud rate (termios2 + BOTHER, so 250000, 1000000, 4000000 ... work too).
func rawTermios(fd int, cfg Config) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS2)
	if err != nil {
		return fmt.Errorf("TCGETS2: %w", err)
	}
	t.Iflag, t.Oflag, t.Lflag = 0, 0, 0
	t.Cflag = unix.CREAD | unix.CLOCAL
	if cfg.DataBits == 7 {
		t.Cflag |= unix.CS7
	} else {
		t.Cflag |= unix.CS8
	}
	switch cfg.Parity {
	case 'E':
		t.Cflag |= unix.PARENB
	case 'O':
		t.Cflag |= unix.PARENB | unix.PARODD
	}
	if cfg.StopBits == 2 {
		t.Cflag |= unix.CSTOPB
	}
	if cfg.Baud > 0 {
		t.Cflag |= unix.BOTHER
		t.Ispeed, t.Ospeed = uint32(cfg.Baud), uint32(cfg.Baud)
	}
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS2, t); err != nil {
		return fmt.Errorf("TCSETS2 %s: %w", cfg, err)
	}
	return nil
}

// Open opens a serial device in raw mode and flushes it.
func Open(dev string, cfg Config) (Conn, error) {
	fd, err := unix.Open(dev, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := rawTermios(fd, cfg); err != nil {
		unix.Close(fd)
		return nil, err
	}
	unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIOFLUSH)
	return &fdConn{fd: fd, name: dev}, nil
}

// SetLatency sets the FTDI latency_timer (ms) of a ttyUSB device; other devices are left alone.
func SetLatency(dev string, ms int) {
	p := "/sys/bus/usb-serial/devices/" + filepath.Base(dev) + "/latency_timer"
	if _, err := os.Stat(p); err == nil {
		os.WriteFile(p, []byte(strconv.Itoa(ms)), 0)
	}
}

// Resolve turns a port spec into a device path. "0".."3" (or "A".."D") is the FT4232H / FT2232H channel,
// found by its USB interface number, so it survives a re-plug (the kernel may then hand out ttyUSB4..7).
// Anything else is a path. "" = the channel is not present right now.
func Resolve(spec string) string {
	ch := -1
	switch {
	case len(spec) == 1 && spec[0] >= '0' && spec[0] <= '7':
		ch = int(spec[0] - '0')
	case len(spec) == 1 && strings.ContainsRune("ABCDabcd", rune(spec[0])):
		ch = int(strings.ToUpper(spec)[0] - 'A')
	default:
		return spec
	}
	base := "/sys/bus/usb-serial/devices"
	ents, _ := os.ReadDir(base)
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool { return ttyNum(names[i]) < ttyNum(names[j]) })
	for _, n := range names {
		real, err := filepath.EvalSymlinks(filepath.Join(base, n))
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(real), "bInterfaceNumber"))
		if err != nil {
			continue
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 16, 32); err == nil && int(v) == ch {
			return "/dev/" + n
		}
	}
	return ""
}

func ttyNum(n string) int {
	v, _ := strconv.Atoi(strings.TrimLeft(n, "ttyUSBACM"))
	return v
}

// OpenPty makes a pty, puts its slave end in raw mode and points link at it (the slave end is not kept
// open, so a busy-port check sees the line as free). The emulator serves the master end.
func OpenPty(link string) (Conn, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (Conn, error) { unix.Close(fd); return nil, err }
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(fmt.Errorf("unlockpt: %w", err))
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(fmt.Errorf("ptsname: %w", err))
	}
	slave := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	err = rawTermios(sfd, Config{DataBits: 8, Parity: 'N', StopBits: 1})
	unix.Close(sfd) // the termios stay while the master is open
	if err != nil {
		return fail(err)
	}
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fail(fmt.Errorf("%s exists and is not a symlink", link))
		}
		os.Remove(link)
	}
	if err := os.Symlink(slave, link); err != nil {
		return fail(err)
	}
	return &fdConn{fd: fd, name: link + " -> " + slave, pty: true}, nil
}
