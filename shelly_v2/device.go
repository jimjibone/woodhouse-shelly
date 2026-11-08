package shelly_v2

import (
	"github.com/jimjibone/log"
	"github.com/jimjibone/wh/v2/bridges"
)

type Device interface {
	Close()
	SetNextIP(ip string)
}

// plusModels maps the app reported by the device onto its model name. All of
// these share the generic ShellyPlus implementation, which builds the device
// from the components the device reports.
var plusModels = map[string]string{
	"Plus1PM":    "Shelly Plus 1PM",
	"Plus2PM":    "Shelly Plus 2PM",
	"PlusPlugUK": "Shelly Plus Plug UK",
}

// Generate creates a device. The Woodhouse device ID is derived from the MAC
// address, hostname is only used for logging.
func Generate(app, hostname, mac, ip, name string, client *bridges.Bridge) Device {
	model, found := plusModels[app]
	if !found {
		log.Warnf("unknown app %q for %s (%s)", app, hostname, name)
		return nil
	}

	dev, err := NewShellyPlus(model, hostname, mac, ip, client)
	if err != nil {
		log.Errorf("failed to create %s (%s): %s", hostname, name, err)
		return nil
	}
	return dev
}
