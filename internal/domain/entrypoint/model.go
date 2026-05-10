package entrypoint

type Model struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	DevAddr   string `json:"devaddr"`
	HasStream bool   `json:"has_stream"`
	HasUnlock bool   `json:"has_unlock"`
	HasRing   bool   `json:"has_ring"`
}
