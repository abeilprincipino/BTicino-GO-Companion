package observability

import "time"

type Health struct {
	Live      bool      `json:"live"`
	Ready     bool      `json:"ready"`
	BootTime  time.Time `json:"boot_time"`
	Timestamp time.Time `json:"timestamp"`
}

func New(boot time.Time, ready bool) Health {
	return Health{
		Live:      true,
		Ready:     ready,
		BootTime:  boot,
		Timestamp: time.Now().UTC(),
	}
}
