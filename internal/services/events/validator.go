package events

import (
	"errors"
	"fmt"
	"strings"

	"bticino-go-companion/internal/domain/event"
)

var ErrUnknownType = errors.New("unknown event type")

type Validator struct{}

func NewValidator() *Validator {
	return &Validator{}
}

func (v *Validator) Validate(ev event.Envelope) error {
	kind := strings.TrimSpace(ev.Type)
	if kind == "" {
		return fmt.Errorf("%w: empty", ErrUnknownType)
	}
	if !event.IsKnownType(kind) {
		return fmt.Errorf("%w: %s", ErrUnknownType, kind)
	}
	return nil
}
