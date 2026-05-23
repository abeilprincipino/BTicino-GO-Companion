package rtspadapter

import (
	"fmt"
	"net"
	"sync"

	"github.com/pion/rtp"
)

type returnAudioForwarder struct {
	addr *net.UDPAddr

	mu     sync.Mutex
	conn   *net.UDPConn
	closed bool
}

func newReturnAudioForwarder(rawAddr string) *returnAudioForwarder {
	addr, err := net.ResolveUDPAddr("udp4", rawAddr)
	if err != nil {
		return &returnAudioForwarder{}
	}
	return &returnAudioForwarder{addr: addr}
}

func (f *returnAudioForwarder) WriteRTP(pkt *rtp.Packet) error {
	if f == nil || pkt == nil {
		return nil
	}
	if f.addr == nil {
		return fmt.Errorf("return audio target unavailable")
	}

	payload, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("marshal return audio rtp: %w", err)
	}

	conn, err := f.connection()
	if err != nil {
		return err
	}
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write return audio rtp: %w", err)
	}
	return nil
}

func (f *returnAudioForwarder) connection() (*net.UDPConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil, net.ErrClosed
	}
	if f.conn != nil {
		return f.conn, nil
	}
	conn, err := net.DialUDP("udp4", nil, f.addr)
	if err != nil {
		return nil, fmt.Errorf("open return audio udp: %w", err)
	}
	f.conn = conn
	return conn, nil
}

func (f *returnAudioForwarder) Close() {
	if f == nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true
	if f.conn != nil {
		_ = f.conn.Close()
		f.conn = nil
	}
}
