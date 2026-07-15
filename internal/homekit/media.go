package homekit

import "bticino-go-companion/internal/media"

type MediaDistributor interface {
	RegisterConsumer(string, media.Consumer) error
	UnregisterConsumer(string) bool
}
