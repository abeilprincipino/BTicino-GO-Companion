package events

import (
	"errors"
	"testing"

	"bticino-go-companion/internal/domain/event"
)

func TestValidatorValidateKnownType(t *testing.T) {
	v := NewValidator()
	if err := v.Validate(event.Envelope{Type: event.TypeRingStarted}); err != nil {
		t.Fatalf("expected known type to pass, got %v", err)
	}
}

func TestValidatorValidateUnknownType(t *testing.T) {
	v := NewValidator()
	err := v.Validate(event.Envelope{Type: "foo.bar"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("expected ErrUnknownType, got %v", err)
	}
}
