package shelly_v1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jimjibone/log"
	"github.com/jimjibone/wh/v2/bridges"
	"github.com/jimjibone/wh/v2/bridges/services"
	"github.com/jimjibone/wh/v2/types"
	clientsapi "github.com/jimjibone/woodhouse-api/go/v2/clients"
)

func init() {
	registerDevice("SHDM-2", func(hostname, mac, ip string, client *bridges.Bridge) Device {
		dev, err := NewShellyDimmer2(hostname, mac, ip, client)
		if err != nil {
			log.Errorf("failed to create %s: %s", hostname, err)
			return nil
		}
		return dev
	})
}

// The Dimmer 2 has a single light channel, gen 1 component index 0, which is
// published as endpoint "1" (endpoint "0" is the root endpoint).
const (
	dimmer2LightIndex    = 0
	dimmer2EndpointID    = "1"
	dimmer2MaxTransition = 5000 * time.Millisecond
)

// ShellyDimmer2 - device type: SHDM-2
//
// Device shape:
//
//	endpoint 0 (root): info, online, update, device_temperature (when reported)
//	endpoint 1:        light (feature brightness)
type ShellyDimmer2 struct {
	log      *log.Context
	rest     *Rest
	hostname string
	ip       string
	nextIP   string

	bridge *bridges.Bridge
	added  bool

	dev    *bridges.Device
	update *services.Update
	temp   *services.DeviceTemperature // nil until the device reports a temperature
	light  *services.Light

	mu    sync.RWMutex
	wg    sync.WaitGroup
	close func()
}

type ShellyDimmer2State struct {
	IsOn           bool   `json:"ison"`            // Whether the channel is turned ON or OFF
	Source         string `json:"source"`          // Source of the last command
	HasTimer       bool   `json:"has_timer"`       // Whether a timer is currently armed for this channel
	TimerStarted   int    `json:"timer_started"`   // Unix timestamp of timer start; 0 if timer inactive or time not synced
	TimerDuration  int    `json:"timer_duration"`  // Timer duration, s
	TimerRemaining int    `json:"timer_remaining"` // experimental If there is an active timer, shows seconds until timer elapses; 0 otherwise
	Mode           string `json:"mode"`            // Always white
	Brightness     int    `json:"brightness"`      // Output brightness, 1..100
	TransitionMs   int    `json:"transition"`      // One-shot transition, 0..5000 [ms]
}

// newShellyDimmer2 creates the device and its services without touching the
// network or starting the run loop.
func newShellyDimmer2(hostname, mac, ip string, client *bridges.Bridge) (*ShellyDimmer2, error) {
	id, err := bridges.DeviceID("shelly", mac)
	if err != nil {
		return nil, fmt.Errorf("device id from mac %q: %w", mac, err)
	}

	dev := &ShellyDimmer2{
		log:      log.NewContext(log.DefaultLogger, hostname, log.DebugLevel),
		rest:     NewRest(ip),
		hostname: hostname,
		ip:       ip,
		bridge:   client,
		dev:      bridges.NewDevice(id, clientsapi.Device_DEVICE),
		update:   services.NewUpdate(""),
		light:    services.NewLight("", services.LightFeatureBrightness),
		close:    func() {},
	}

	// Root endpoint: info and online are created with the device.
	dev.dev.AddService(dev.update)
	dev.dev.Info.EnableRename(dev.handleRename)
	dev.update.OnStart(dev.handleUpdateStart)

	// Output endpoint. The label is set once the settings are known.
	dev.dev.Endpoint(dimmer2EndpointID).SetLabel("Output 1")
	dev.dev.Endpoint(dimmer2EndpointID).AddService(dev.light)
	dev.light.On.OnWrite(func(on bool) error {
		return dev.setLight(&on, nil, nil)
	})
	dev.light.Brightness.OnWrite(func(bri int64) error {
		return dev.setLight(nil, &bri, nil)
	})
	dev.light.OnSet(func(p types.LightSetParams) error {
		return dev.setLight(p.On, p.Brightness, p.Transition)
	})
	narrowTransition(dev.light)

	return dev, nil
}

// narrowTransition limits the transition param of light.set to what the
// Dimmer 2 supports: 0 to 5000ms.
func narrowTransition(light *services.Light) {
	spec := light.CommandSpec("set")
	if spec == nil {
		return
	}
	for _, param := range spec.GetParams() {
		if param.GetId() != "transition" {
			continue
		}
		if d := param.GetSpec().GetDuration(); d != nil {
			max := dimmer2MaxTransition.Milliseconds()
			step := uint64(1)
			d.Max = &max
			d.Step = &step
		}
	}
}

func NewShellyDimmer2(hostname, mac, ip string, client *bridges.Bridge) (*ShellyDimmer2, error) {
	dev, err := newShellyDimmer2(hostname, mac, ip, client)
	if err != nil {
		return nil, err
	}
	dev.log.Infof("created %s", dev.dev.ID())

	ctx, close := context.WithCancel(context.Background())
	dev.close = close

	// Get the initial info and state.
	err = dev.updateInfo()
	if err != nil {
		dev.log.Warnf("failed to update info: %s", err)
	}
	err = dev.requestAndUpdateState("")
	if err != nil {
		dev.log.Warnf("failed to update state: %s", err)
	}

	dev.wg.Add(1)
	go dev.run(ctx)
	return dev, nil
}

func (dev *ShellyDimmer2) Close() {
	dev.close()
	dev.wg.Wait()
	dev.dev.Close()
}

func (dev *ShellyDimmer2) SetNextIP(ip string) {
	dev.mu.Lock()
	defer dev.mu.Unlock()
	dev.nextIP = ip
}

// handleRename sets the device name via the settings endpoint, which responds
// with the updated settings from which the info name attribute is refreshed.
func (dev *ShellyDimmer2) handleRename(newName string) error {
	dev.mu.Lock()
	defer dev.mu.Unlock()

	settings := DeviceSettings{}
	err := dev.rest.GetJSON("settings?"+url.Values{"name": {newName}}.Encode(), &settings)
	if err != nil {
		dev.log.Errorf("failed to rename device: %s", err)
		return fmt.Errorf("failed to rename device: %w", err)
	}

	dev.log.Infof("renamed device to %q", settings.Name)
	dev.dev.Info.Name.Set(settings.Name)
	return nil
}

// handleUpdateStart asks the device to install the available firmware update.
func (dev *ShellyDimmer2) handleUpdateStart() error {
	dev.mu.Lock()
	defer dev.mu.Unlock()

	if !dev.update.Available.Get() {
		return errors.New("no firmware update available")
	}

	var res map[string]any
	err := dev.rest.GetJSON("ota?update=true", &res)
	if err != nil {
		dev.log.Errorf("failed to start update: %s", err)
		return fmt.Errorf("failed to start update: %w", err)
	}

	dev.log.Infof("started firmware update")
	dev.update.Updating.Set(true)
	return nil
}

// setLight sends the requested changes to the light channel. Any of on,
// brightness and transition may be nil to leave it unchanged.
func (dev *ShellyDimmer2) setLight(on *bool, brightness *int64, transition *time.Duration) error {
	reqparams := lightParams(on, brightness, transition)
	if reqparams == "" {
		return nil
	}

	dev.log.Infof("name: %s, sending request: %s", dev.dev.Info.Name.Get(), reqparams)
	err := dev.requestAndUpdateState(reqparams)
	if err != nil {
		dev.log.Errorf("failed to set state: %s", err)
		return err
	}
	return nil
}

// lightParams builds the light/0 query string for the requested changes.
func lightParams(on *bool, brightness *int64, transition *time.Duration) string {
	var params []string

	if on != nil {
		if *on {
			params = append(params, "turn=on")
		} else {
			params = append(params, "turn=off")
		}
	}

	if brightness != nil {
		bri := math.Max(math.Min(float64(*brightness), 100), 0)
		params = append(params, fmt.Sprintf("brightness=%.0f", bri))
	}

	if transition != nil {
		trans := math.Max(math.Min(float64(transition.Milliseconds()), float64(dimmer2MaxTransition.Milliseconds())), 0)
		params = append(params, fmt.Sprintf("transition=%.0f", trans))
	}

	return strings.Join(params, "&")
}

func (dev *ShellyDimmer2) run(ctx context.Context) {
	defer dev.wg.Done()

	infoTicker := time.NewTicker(10 * time.Second)
	defer infoTicker.Stop()

	stateTicker := time.NewTicker(time.Second)
	defer stateTicker.Stop()

	for {
		connected := false
		select {
		case <-ctx.Done():
			return

		case <-infoTicker.C:
			err := dev.updateInfo()
			if err != nil {
				dev.log.Warnf("failed to update info: %s", err)
			} else {
				connected = true
			}

		case <-stateTicker.C:
			err := dev.requestAndUpdateState("")
			if err != nil {
				dev.log.Warnf("failed to update state: %s", err)
			} else {
				connected = true
			}
		}

		// If we didn't connect then implement exponential backoff.
		dev.rest.Backoff(dev.log, ctx, connected)
	}
}

func (dev *ShellyDimmer2) updateInfo() error {
	dev.mu.Lock()
	defer dev.mu.Unlock()

	settings, err := dev.rest.GetSettings()
	if err != nil {
		// If this failed, try switching to the next IP if there is one.
		if dev.nextIP != "" && dev.ip != dev.nextIP {
			dev.log.Infof("switching to new ip: %s", dev.nextIP)
			dev.ip = dev.nextIP
			dev.nextIP = ""
			dev.rest.SetIP(dev.ip)
		}

		return err
	}

	// The status carries the firmware update and temperature, which decide
	// the services of the root endpoint, so it is needed before the device
	// is added.
	var status DeviceStatusSummary
	err = dev.rest.GetJSON("status", &status)
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}

	dev.log.Debugf("settings: %s", settings.Name)

	dev.applySettings(settings)
	dev.applyStatus(status)

	// Add the device to the client once we've fully connected to it.
	if !dev.added {
		dev.added = true
		dev.bridge.AddDevice(dev.dev)
	}

	return nil
}

// applySettings updates the info service and the output label from the
// device settings.
func (dev *ShellyDimmer2) applySettings(settings DeviceSettings) {
	if dev.dev.Info.Name.Set(settings.Name) {
		dev.log.Infof("name: %s", settings.Name)
	}
	dev.dev.Info.Model.Set("Shelly Dimmer 2")
	dev.dev.Info.Manufacturer.Set("Shelly")
	dev.dev.Info.SerialNumber.Set(settings.Device.Mac)
	dev.dev.Info.FirmwareVersion.Set(settings.Fw)
	dev.dev.Info.WebURL.Set("http://" + dev.ip)
	dev.dev.Online.Online.Set(true)
	dev.dev.Online.LastSeen.Set(time.Now())

	label := "Output 1"
	if dimmer2LightIndex < len(settings.Lights) && settings.Lights[dimmer2LightIndex].Name != "" {
		label = settings.Lights[dimmer2LightIndex].Name
	}
	dev.dev.Endpoint(dimmer2EndpointID).SetLabel(label)
}

// applyStatus updates the update and device_temperature services from the
// device status. The temperature service is created on the first status that
// reports a temperature; that must happen before the device is added.
func (dev *ShellyDimmer2) applyStatus(status DeviceStatusSummary) {
	dev.update.Available.Set(status.Update.HasUpdate)
	if status.Update.OldVersion != "" {
		dev.update.CurrentVersion.Set(status.Update.OldVersion)
	}
	if status.Update.NewVersion != "" {
		dev.update.UpdateVersion.Set(status.Update.NewVersion)
	}
	dev.update.Updating.Set(status.Update.Status == "updating")

	if status.Tmp != nil {
		if dev.temp == nil {
			dev.temp = services.NewDeviceTemperature("")
			dev.dev.AddService(dev.temp)
		}
		if status.Tmp.IsValid {
			dev.temp.Temperature.Set(status.Tmp.TC)
		}
	}
}

func (dev *ShellyDimmer2) requestAndUpdateState(params string) error {
	dev.mu.Lock()
	defer dev.mu.Unlock()

	endpoint := fmt.Sprintf("light/%d", dimmer2LightIndex)
	if params != "" {
		endpoint = endpoint + "?" + params
	}

	// Get the latest state.
	next := ShellyDimmer2State{}
	err := dev.rest.GetJSON(endpoint, &next)
	if err != nil {
		if dev.dev.Online.Online.Set(false) {
			dev.log.Infof("went offline")
		}
		return err
	}

	dev.applyState(next)
	return nil
}

// applyState updates the online and light services from the light state.
func (dev *ShellyDimmer2) applyState(next ShellyDimmer2State) {
	changed := false
	if dev.dev.Online.Online.Set(true) {
		dev.log.Infof("came online")
		changed = true
	}
	if dev.light.On.Set(next.IsOn) {
		dev.log.Infof("on: %t", next.IsOn)
		changed = true
	}
	if dev.light.Brightness.Set(int64(next.Brightness)) {
		dev.log.Infof("brightness: %d%%", next.Brightness)
		changed = true
	}
	if changed {
		// Prevent unnecessary updates when nothing changed.
		dev.dev.Online.LastSeen.Set(time.Now())
	}
}
