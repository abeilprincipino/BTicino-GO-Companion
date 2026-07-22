package httputil

import (
	"encoding/json"
	"errors"
	"io"
)

var ErrMultipleJSONValues = errors.New("multiple JSON values")

// DecodeJSON strictly decodes one JSON value from a bounded reader.
func DecodeJSON(reader io.Reader, limit int64, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, limit))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return err
	}

	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrMultipleJSONValues
	}

	return nil
}
