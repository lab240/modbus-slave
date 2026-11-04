package server

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// MaxTCPClients is how many Modbus TCP connections one line serves at once; more are refused.
const MaxTCPClients = 16

// ServeTCP answers Modbus TCP (MBAP) requests on ln until ctx is done; ln is closed on return.
// The devices of the line are behind it as behind a gateway: the unit id picks the device,
// unit 0 / 255 is the first one, a unit that is not here gets exception 0x0B.
// The faults apply as on RTU, except bad CRC and echo (TCP has neither).
func (l *Line) ServeTCP(ctx context.Context, ln net.Listener) error {
	l.setDevice("tcp " + ln.Addr().String())
	l.open.Store(true)
	defer l.open.Store(false)

	var mu sync.Mutex
	conns := map[net.Conn]bool{}
	var wg sync.WaitGroup
	stop := context.AfterFunc(ctx, func() {
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	defer func() {
		stop()
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		mu.Lock()
		if len(conns) >= MaxTCPClients {
			mu.Unlock()
			log.Printf("line %d: tcp client %s refused, %d connections already", l.Index, c.RemoteAddr(), MaxTCPClients)
			c.Close()
			continue
		}
		conns[c] = true
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.tcpClient(c)
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
			c.Close()
		}()
	}
}

// tcpClient serves one connection: MBAP header (transaction, protocol 0, length, unit) + PDU.
func (l *Line) tcpClient(c net.Conn) {
	hdr := make([]byte, 7)
	for {
		c.SetReadDeadline(time.Now().Add(10 * time.Minute)) // a dead peer must not hold a slot forever
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(hdr[4:6]))
		if binary.BigEndian.Uint16(hdr[2:4]) != 0 || n < 2 || n > 254 {
			l.Stats.Resync.Add(1) // not Modbus TCP: drop the connection, as a real server would
			return
		}
		pdu := make([]byte, n-1)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, pdu); err != nil {
			return
		}
		ans := l.handle(hdr[6], pdu, true)
		if ans == nil {
			continue // no answer: the master times out (an injected drop)
		}
		f := l.FaultsFor(hdr[6])
		if d := f.Delay + l.jitter(f.Jitter); d > 0 {
			time.Sleep(d)
		}
		out := make([]byte, 7, 7+len(ans))
		copy(out, hdr[:4])
		binary.BigEndian.PutUint16(out[4:6], uint16(len(ans)+1))
		out[6] = hdr[6]
		out = append(out, ans...)
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if p := f.Frag; p > 1 { // in pieces: tests how the master reassembles a TCP stream
			step := (len(out) + p - 1) / p
			for i := 0; i < len(out); i += step {
				if _, err := c.Write(out[i:min(i+step, len(out))]); err != nil {
					return
				}
				time.Sleep(f.FragGap)
			}
		} else if _, err := c.Write(out); err != nil {
			return
		}
		l.Stats.Ans.Add(1)
	}
}
