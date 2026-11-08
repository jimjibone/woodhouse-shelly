package shelly_v1

import (
	"github.com/jimjibone/wh/v2/bridges"
)

type Device interface {
	Close()
	SetNextIP(ip string)
}

// DeviceGenerator creates a device. The Woodhouse device ID is derived from
// the MAC address, hostname is only used for logging.
type DeviceGenerator func(hostname, mac, ip string, client *bridges.Bridge) Device

var registry = make(map[string]DeviceGenerator)

func registerDevice(deviceType string, generator DeviceGenerator) {
	registry[deviceType] = generator
}

func Generate(deviceType, hostname, mac, ip string, client *bridges.Bridge) Device {
	if gen, found := registry[deviceType]; found {
		return gen(hostname, mac, ip, client)
	}
	return nil
}
