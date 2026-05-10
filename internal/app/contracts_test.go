package app

import (
	"bticino-go-companion/internal/adapters/openwebnet"
	"bticino-go-companion/internal/adapters/sip"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/media"
)

var _ control.UnlockDriver = (*openwebnet.CommandClient)(nil)
var _ control.StreamDriver = (*media.Service)(nil)
var _ control.CallDriver = (*sipadapter.Manager)(nil)
var _ media.Backend = (*sipadapter.Manager)(nil)
