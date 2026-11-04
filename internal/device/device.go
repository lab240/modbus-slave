// Package device is one emulated Modbus slave: a unit address, a register map and the function codes it answers.
package device

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/lab240/modbus-slave/internal/rtu"
)

// ErrAddress is returned by a register map for an unmapped or read-only address (-> exception 2).
var ErrAddress = errors.New("illegal data address")

// ErrDeviceFailure is returned when the data is not available (e.g. a stale file) (-> exception 4).
var ErrDeviceFailure = errors.New("server device failure")

func excFor(fc byte, err error) []byte {
	if errors.Is(err, ErrDeviceFailure) {
		return rtu.Exception(fc, rtu.ExcDeviceFailure)
	}
	return rtu.Exception(fc, rtu.ExcIllegalAddress)
}

// Regs is a register map. Read gets count registers starting at addr in one go, so a source
// backed by a file is read once per request, not once per register.
type Regs interface {
	Read(fc byte, addr, count uint16) ([]uint16, error)
	Write(addr uint16, vals []uint16) error
}

// Device is one slave on a line.
type Device struct {
	unit   atomic.Uint32
	Regs   Regs
	funcs  map[byte]bool
	mu     sync.Mutex // one request at a time per device (writes vs reads)
	Served atomic.Uint64
}

// New makes a device answering the given function codes (3, 4, 6, 8, 16 are implemented).
func New(unit byte, regs Regs, funcs ...byte) *Device {
	d := &Device{Regs: regs, funcs: map[byte]bool{}}
	d.unit.Store(uint32(unit))
	for _, f := range funcs {
		d.funcs[f] = true
	}
	return d
}

func (d *Device) Unit() byte            { return byte(d.unit.Load()) }
func (d *Device) SetUnit(u byte)        { d.unit.Store(uint32(u)) }
func (d *Device) Supports(fc byte) bool { return d.funcs[fc] }

// Handle answers a request PDU (function code + data) with an answer PDU.
func (d *Device) Handle(pdu []byte) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	fc := pdu[0]
	if !d.funcs[fc] {
		return rtu.Exception(fc, rtu.ExcIllegalFunction)
	}
	be := binary.BigEndian
	switch fc {
	case 3, 4:
		if len(pdu) < 5 {
			return rtu.Exception(fc, rtu.ExcIllegalValue)
		}
		addr, count := be.Uint16(pdu[1:]), be.Uint16(pdu[3:])
		if count < 1 || count > 125 {
			return rtu.Exception(fc, rtu.ExcIllegalValue)
		}
		if int(addr)+int(count) > 0x10000 {
			return rtu.Exception(fc, rtu.ExcIllegalAddress)
		}
		vals, err := d.Regs.Read(fc, addr, count)
		if err != nil {
			return excFor(fc, err)
		}
		out := make([]byte, 2, 2+2*len(vals))
		out[0], out[1] = fc, byte(2*count)
		for _, v := range vals {
			out = be.AppendUint16(out, v)
		}
		return out
	case 6:
		if len(pdu) < 5 {
			return rtu.Exception(fc, rtu.ExcIllegalValue)
		}
		if err := d.Regs.Write(be.Uint16(pdu[1:]), []uint16{be.Uint16(pdu[3:])}); err != nil {
			return excFor(fc, err)
		}
		return append([]byte(nil), pdu[:5]...)
	case 16:
		if len(pdu) < 6 {
			return rtu.Exception(fc, rtu.ExcIllegalValue)
		}
		addr, count, n := be.Uint16(pdu[1:]), be.Uint16(pdu[3:]), int(pdu[5])
		if count < 1 || count > 123 || n != 2*int(count) || len(pdu) < 6+n {
			return rtu.Exception(fc, rtu.ExcIllegalValue)
		}
		vals := make([]uint16, count)
		for i := range vals {
			vals[i] = be.Uint16(pdu[6+2*i:])
		}
		if err := d.Regs.Write(addr, vals); err != nil {
			return excFor(fc, err)
		}
		return append([]byte(nil), pdu[:5]...)
	case 8:
		if len(pdu) >= 3 && pdu[1] == 0 && pdu[2] == 0 { // sub-function 0: return query data
			return append([]byte(nil), pdu...)
		}
		return rtu.Exception(fc, rtu.ExcIllegalFunction)
	}
	return rtu.Exception(fc, rtu.ExcIllegalFunction)
}

// ReadRegs reads registers for the API (not counted as a request, no function-code check).
func (d *Device) ReadRegs(fc byte, addr, count uint16) ([]uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Regs.Read(fc, addr, count)
}

// WriteRegs writes registers for the API, with the same rules as FC 16 (only writable registers).
func (d *Device) WriteRegs(addr uint16, vals []uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Regs.Write(addr, vals)
}
