package shelly_v2

// InputSetConfig - request parameters for the Input.SetConfig RPC method.
type InputSetConfig struct {
	ID     int         `json:"id"`
	Config InputConfig `json:"config"`
}

// InputConfig holds the settable input fields we use. Omitted fields are left
// unchanged by the device.
type InputConfig struct {
	RangeMap []float64 `json:"range_map,omitempty"` // analog only: [min, max]
}
