package rtu

import (
	"bytes"
	"testing"
	"time"
)

func TestCRC(t *testing.T) {
	// read 10 holding registers of unit 1 from 0: well-known frame 01 03 00 00 00 0A C5 CD
	f := AppendCRC([]byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x0A})
	if !bytes.Equal(f, []byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x0A, 0xC5, 0xCD}) {
		t.Fatalf("got % X", f)
	}
	if !CheckCRC(f) {
		t.Fatal("CheckCRC false on a good frame")
	}
	f[3] ^= 1
	if CheckCRC(f) {
		t.Fatal("CheckCRC true on a broken frame")
	}
}

func TestRequestLen(t *testing.T) {
	for _, c := range []struct {
		buf  []byte
		want int
	}{
		{[]byte{1}, -1},
		{[]byte{1, 3}, 8},
		{[]byte{1, 16, 0, 0, 0, 2}, -1},
		{[]byte{1, 16, 0, 0, 0, 2, 4}, 13},
		{[]byte{1, 0x2B}, 0},
	} {
		if got := RequestLen(c.buf); got != c.want {
			t.Errorf("RequestLen(% X) = %d, want %d", c.buf, got, c.want)
		}
	}
}

func TestT35(t *testing.T) {
	if got := T35(9600, 11); got < 4000*time.Microsecond || got > 4020*time.Microsecond {
		t.Errorf("T35(9600) = %v, want ~4.01ms", got)
	}
	if got := T35(115200, 11); got != 1750*time.Microsecond {
		t.Errorf("T35(115200) = %v, want 1.75ms", got)
	}
}
