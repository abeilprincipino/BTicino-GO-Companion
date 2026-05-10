package openwebnet

import (
	"context"
	"errors"
	"fmt"
	"net"

	"bticino-go-companion/internal/domain/event"
	protocol "bticino-go-companion/internal/protocol/openwebnet"
)

type Listener struct {
	group  string
	port   int
	buffer int
	parser *protocol.Parser
	mapper *protocol.Mapper
}

func NewListener(group string, port int, buffer int) *Listener {
	if buffer <= 0 {
		buffer = 65535
	}
	return &Listener{
		group:  group,
		port:   port,
		buffer: buffer,
		parser: protocol.NewParser(),
		mapper: protocol.NewMapper(),
	}
}

func (l *Listener) Run(ctx context.Context, sink func(event.Envelope)) error {
	ip := net.ParseIP(l.group)
	if ip == nil {
		return fmt.Errorf("invalid multicast group: %s", l.group)
	}
	addr := &net.UDPAddr{IP: ip, Port: l.port}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(l.buffer)

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	buf := make([]byte, l.buffer)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			continue
		}
		msg, parseErr := l.parser.Parse(buf[:n])
		if parseErr != nil {
			continue
		}
		for _, ev := range l.mapper.Map(msg) {
			sink(ev)
		}
	}
}
