package shelly_v1

import (
	"github.com/jimjibone/wh/v1/bridges"
)

type Device interface {
	ID() string
	Close()
	SetNextIP(ip string)
}

type DeviceGenerator func(hostname, ip string, client *bridges.Bridge) Device

var registry = make(map[string]DeviceGenerator)

func registerDevice(deviceType string, generator DeviceGenerator) {
	registry[deviceType] = generator
}

func Generate(deviceType, hostname, ip string, client *bridges.Bridge) Device {
	if gen, found := registry[deviceType]; found {
		return gen(hostname, ip, client)
	}
	return nil
}
