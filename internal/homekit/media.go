package homekit

import "bticino-go-companion/internal/media"

type MediaConsumer interface {
	Consume(media.Packet)
}

type MediaDistributor interface {
	RegisterConsumer(string, media.Consumer) error
	UnregisterConsumer(string) bool
}
