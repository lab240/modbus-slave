// Package rtu holds the Modbus RTU framing: CRC16, request length by function code, exceptions.
package rtu

import "time"

// Exception codes.
const (
	ExcIllegalFunction = 0x01
	ExcIllegalAddress  = 0x02
	ExcIllegalValue    = 0x03
	ExcDeviceFailure   = 0x04
)

var crcTable [256]uint16

func init() {
	for i := range crcTable {
		c := uint16(i)
		for j := 0; j < 8; j++ {
			if c&1 != 0 {
				c = c>>1 ^ 0xA001
			} else {
				c >>= 1
			}
		}
		crcTable[i] = c
	}
}

// CRC returns the Modbus CRC16 of b.
func CRC(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, x := range b {
		crc = crc>>8 ^ crcTable[byte(crc)^x]
	}
	return crc
}

// AppendCRC appends the CRC of b to b (low byte first, as on the wire).
func AppendCRC(b []byte) []byte {
	c := CRC(b)
	return append(b, byte(c), byte(c>>8))
}

// CheckCRC reports whether the last two bytes of frame are its CRC.
func CheckCRC(frame []byte) bool {
	n := len(frame)
	return n >= 4 && CRC(frame[:n-2]) == uint16(frame[n-2])|uint16(frame[n-1])<<8
}

// RequestLen is the length of the request at the start of buf (unit + PDU + CRC):
// -1 = more bytes are needed to tell, 0 = function code with an unknown request length.
func RequestLen(buf []byte) int {
	if len(buf) < 2 {
		return -1
	}
	switch buf[1] {
	case 1, 2, 3, 4, 5, 6, 8:
		return 8
	case 15, 16:
		if len(buf) < 7 {
			return -1
		}
		return 9 + int(buf[6])
	}
	return 0
}

// Exception builds an exception answer PDU.
func Exception(fc, code byte) []byte {
	return []byte{fc | 0x80, code}
}

// T35 is the Modbus inter-frame silence for the given line settings:
// 3.5 characters, but a fixed 1750 us above 19200 baud (Modbus over serial line spec, 2.5.1.1).
func T35(baud, bitsPerChar int) time.Duration {
	if baud > 19200 {
		return 1750 * time.Microsecond
	}
	return time.Duration(float64(time.Second) * 3.5 * float64(bitsPerChar) / float64(baud))
}
