package device

import "github.com/lab240/modbus-slave/internal/source"

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
