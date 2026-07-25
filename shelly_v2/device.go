package shelly_v2

import (
	"github.com/jimjibone/log"
	"github.com/jimjibone/wh/v1/bridges"
)

type Device interface {
	ID() string
	Close()
	SetNextIP(ip string)
}

func Generate(app, hostname, ip, name string, client *bridges.Bridge) Device {
	// switch hostname {
	// case "ShellyPlusPlugUK-DEADBEEF":
	// 	return NewShellyPlusPlugUK(hostname, ip, client)
	// default:
	// 	log.Warnf("unknown app %q for %s (%s)", app, hostname, name)
	// }
	switch app {
	case "Plus1PM":
		return NewShellyPlus1PM(hostname, ip, client)
	case "Plus2PM":
		return NewShellyPlus2PM(hostname, ip, client)
	case "PlusPlugUK":
		return NewShellyPlusPlugUK(hostname, ip, client)
	default:
		log.Warnf("unknown app %q for %s (%s)", app, hostname, name)
	}
	return nil
}
