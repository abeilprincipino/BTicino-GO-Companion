package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
)

const (
	multicastGroup = "239.255.76.67"
	multicastPort  = 7667
	readBufferSize = 65535
)

// Listener converts device multicast frames into projector events.
type Listener struct {
	group  string
	port   int
	buffer int
	parser Parser
	mapper *Mapper
	logger *slog.Logger
	trace  *Trace
}

func NewListener(entrypoints []config.Entrypoint, logger *slog.Logger, trace *Trace) *Listener {
	if logger == nil {
		logger = slog.Default()
	}
	return &Listener{group: multicastGroup, port: multicastPort, buffer: readBufferSize, mapper: NewMapper(entrypoints), logger: logger, trace: trace}
}

func (l *Listener) Run(ctx context.Context, sink func(core.Event)) error {
	ip := net.ParseIP(l.group)
	if ip == nil {
		return fmt.Errorf("invalid multicast group %q", l.group)
	}
	if l.buffer <= 0 {
		l.buffer = 65535
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: l.port})
	if err != nil {
		return fmt.Errorf("listen multicast: %w", err)
	}
	defer conn.Close()
	if err := conn.SetReadBuffer(l.buffer); err != nil {
		l.logger.Warn("set multicast read buffer", "error", err)
	}
	go func() { <-ctx.Done(); _ = conn.Close() }()
	buf := make([]byte, l.buffer)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			l.logger.Warn("read multicast frame", "error", err)
			continue
		}
		message, err := l.parser.Parse(buf[:n])
		if err != nil {
			continue
		}
		events := l.mapper.Map(message)
		l.trace.Record(message, len(events))
		for _, event := range events {
			sink(event)
		}
	}
}
