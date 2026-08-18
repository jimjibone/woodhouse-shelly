package shelly_v2

// SysSetConfig - request parameters for the Sys.SetConfig RPC method. Only the
// settable fields we use are included; omitted fields are left unchanged by
// the device.
type SysSetConfig struct {
	Config SysConfig `json:"config"`
}

type SysConfig struct {
	Device SysDeviceConfig `json:"device"`
}

type SysDeviceConfig struct {
	Name string `json:"name"`
}
