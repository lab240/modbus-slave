package device

import (
	"time"

	"github.com/lab240/modbus-slave/internal/source"
)

// ── generic: the v1 device ─────────────────────────────────────────────────────────────────────────────
// N holding registers at 0..N-1 (FC 3, and FC 4 reads the same map), values from a source block.

type Generic struct {
	N   int
	Src source.Block
}

func (g *Generic) Read(fc byte, addr, count uint16) ([]uint16, error) {
	if int(addr)+int(count) > g.N {
		return nil, ErrAddress
	}
	return g.Src.Values(g.N)[addr : addr+count], nil
}

func (g *Generic) Write(addr uint16, vals []uint16) error { return ErrAddress }

// NewGeneric is the v1-compatible device: FC 3 (+ FC 4 on the same map), 20 registers by default.
func NewGeneric(unit byte, n int, src source.Block) *Device {
	return New(unit, &Generic{N: n, Src: src}, 3, 4)
}

// ── kth2r: KTH2-R temperature / humidity sensor ────────────────────────────────────────────────────────
// Register map (FC 3 and FC 4 read the same map):
//   0x0000..0x0004  model 0x0001, fw 0x0120, name "KTH2-R"           -- as the real sensor
//   0x000C..0x000E  unit address, speed code, parity code            -- FC 6 to 0x000C changes the address
//   0x0300..0x0301  T x10 (signed), RH x10: sawtooth drift, base differs per line, so crossed lines show up
//   0x04FF..0x0500  T / RH correction (RW)
//   0x1000..0x1004  emulator only: requests served (hi, lo), line index, unit, baud/100
//   0x2000..0x207C  emulator only: 125 regs = 0x1000*(line+1) + 0x100*unit + i, RW -- max frame test

var speedCode = map[int]uint16{1200: 0, 2400: 1, 4800: 2, 9600: 3, 19200: 4, 38400: 5, 57600: 6, 115200: 7}

type KTH2R struct {
	dev    *Device
	line   int
	baud   int
	parity byte // 'N', 'E', 'O'
	t0     time.Time
	corr   [2]int16
	block  [125]uint16
}

// NewKTH2R makes a KTH2-R on line `line` (0-based, shows up in the values and in 0x1002).
func NewKTH2R(unit byte, line, baud int, parity byte) *Device {
	k := &KTH2R{line: line, baud: baud, parity: parity, t0: time.Now()}
	for i := range k.block {
		k.block[i] = uint16(0x1000*(line+1) + 0x100*int(unit) + i)
	}
	k.dev = New(unit, k, 3, 4, 6, 8, 16)
	return k.dev
}

func (k *KTH2R) Read(fc byte, addr, count uint16) ([]uint16, error) {
	out := make([]uint16, count)
	for i := range out {
		v, ok := k.reg(addr + uint16(i))
		if !ok {
			return nil, ErrAddress
		}
		out[i] = v
	}
	return out, nil
}

func (k *KTH2R) reg(a uint16) (uint16, bool) {
	switch {
	case a < 5:
		return [5]uint16{0x0001, 0x0120, 'K'<<8 | 'T', 'H'<<8 | '2', '-'<<8 | 'R'}[a], true
	case a >= 0x000C && a <= 0x000E:
		sc, ok := speedCode[k.baud]
		if !ok {
			sc = 0xFF
		}
		par := map[byte]uint16{'N': 0, 'E': 1, 'O': 2}[k.parity]
		return [3]uint16{uint16(k.dev.Unit()), sc, par}[a-0x000C], true
	case a == 0x0300 || a == 0x0301:
		t := time.Since(k.t0).Seconds()
		if a == 0x0300 {
			return uint16(210 + 10*k.line + saw(t, 60, 5) + int(k.corr[0])), true
		}
		return uint16(300 + 10*k.line + saw(t, 90, 10) + int(k.corr[1])), true
	case a == 0x04FF || a == 0x0500:
		return uint16(k.corr[a-0x04FF]), true
	case a >= 0x1000 && a <= 0x1004:
		s := k.dev.Served.Load()
		return [5]uint16{uint16(s >> 16), uint16(s), uint16(k.line), uint16(k.dev.Unit()), uint16(k.baud / 100)}[a-0x1000], true
	case a >= 0x2000 && a < 0x2000+uint16(len(k.block)):
		return k.block[a-0x2000], true
	}
	return 0, false
}

// saw is the Python emulator's int(amp * ((t / half) % 2 - 1)): a sawtooth from -amp to +amp over 2*half s.
func saw(t, half float64, amp int) int {
	x := t/half - 2*float64(int(t/half/2))
	return int(float64(amp) * (x - 1))
}

func (k *KTH2R) Write(addr uint16, vals []uint16) error {
	for i, v := range vals {
		a := addr + uint16(i)
		switch {
		case a == 0x000C && v >= 1 && v <= 247:
			k.dev.SetUnit(byte(v)) // takes effect for the next request, like the real sensor
		case a == 0x04FF || a == 0x0500:
			k.corr[a-0x04FF] = int16(v)
		case a >= 0x2000 && a < 0x2000+uint16(len(k.block)):
			k.block[a-0x2000] = v
		default:
			return ErrAddress
		}
	}
	return nil
}
