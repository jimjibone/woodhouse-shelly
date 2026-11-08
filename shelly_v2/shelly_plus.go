package shelly_v2

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jimjibone/wh/v2/bridges"
	"github.com/jimjibone/wh/v2/bridges/services"
	"github.com/jimjibone/wh/v2/types"
	clientsapi "github.com/jimjibone/woodhouse-api/go/v2/clients"
)

const (
	inputTypeSwitch = "switch"
	inputTypeButton = "button"
	inputTypeAnalog = "analog"

	updateStage = "stable"
)

// buttonEvents are the events a Shelly button input emits, listed as the
// options of the button's last_event.
var buttonEvents = []string{"press", "release", "hold", "long_release", "multi_press"}

// endpointID returns the endpoint ID of a component. Shelly components are
// indexed from 0, but endpoint "0" is the root endpoint, so the endpoint ID is
// the component index plus one.
func endpointID(index int) string {
	return strconv.Itoa(index + 1)
}

// ShellyPlus is a Gen 2 device made of switch (output) and input components:
// the Plus 1PM, Plus 2PM and Plus Plug UK. The endpoints and services are
// built from the config and status the device reports on first connection.
//
// Device shape:
//
//	endpoint 0 (root): info, online, update
//	endpoint N+1, per switch N: on_off, power_meter (when metered),
//	    device_temperature (when the switch reports one), and the input N
//	    when there is one (binary_input, button or analog_input)
//	endpoint N+1, per input N without a switch N: the input
type ShellyPlus struct {
	shelly *ShellyComms
	model  string

	// Built on the first connection, before the device is added to the
	// bridge. After that the maps are only read, on the comms goroutine, and
	// the handlers capture what they need.
	built    bool
	update   *services.Update
	switches map[int]*plusSwitch
	inputs   map[int]*plusInput
}

// plusSwitch is the services of a switch component.
type plusSwitch struct {
	id    int
	ep    *bridges.Endpoint
	onOff *services.OnOff
	meter *services.PowerMeter        // nil if the switch is not metered
	temp  *services.DeviceTemperature // nil if the switch has no temperature
}

// plusInput is the service of an input component, of the type configured on
// the device.
type plusInput struct {
	id     int
	typ    string // input type at build time
	paired bool   // shares the endpoint of the switch with the same ID
	ep     *bridges.Endpoint

	binary *services.BinaryInput // typ == switch
	button *services.Button      // typ == button
	analog *services.AnalogInput // typ == analog

	// Button press tracking, to time long presses. Only used on the comms
	// goroutine.
	downTs float64
	downAt time.Time
	held   bool

	// Analog reading and range map, used to work out the mapped value. The
	// range map is set from the comms goroutine and from the write handler.
	mu        sync.Mutex
	percent   *float64
	rangeMin  float64
	rangeMax  float64
	rangeInit bool
}

func NewShellyPlus(model, hostname, mac, ip string, client *bridges.Bridge) (*ShellyPlus, error) {
	dev, err := newShellyPlus(model, hostname, mac, ip, client)
	if err != nil {
		return nil, err
	}
	dev.shelly.Start()
	return dev, nil
}

// newShellyPlus creates the device without connecting to it.
func newShellyPlus(model, hostname, mac, ip string, client *bridges.Bridge) (*ShellyPlus, error) {
	shelly, err := NewShellyComms(hostname, mac, ip, clientsapi.Device_DEVICE, client)
	if err != nil {
		return nil, err
	}
	dev := &ShellyPlus{
		shelly:   shelly,
		model:    model,
		update:   services.NewUpdate(""),
		switches: make(map[int]*plusSwitch),
		inputs:   make(map[int]*plusInput),
	}

	dev.shelly.OnConnected(dev.onConnected)
	dev.shelly.OnDisconnected(dev.onDisconnected)
	dev.shelly.OnNotificationFrame(dev.onNotificationFrame)
	dev.shelly.dev.AddService(dev.update)
	dev.shelly.dev.Info.EnableRename(dev.handleRename)
	dev.update.OnStart(dev.handleUpdateStart)

	return dev, nil
}

func (dev *ShellyPlus) Close() {
	dev.shelly.Close()
}

func (dev *ShellyPlus) SetNextIP(ip string) {
	dev.shelly.SetNextIP(ip)
}

func (dev *ShellyPlus) onConnected(config GetConfigResponse, status GetStatusResponse) {
	info := dev.shelly.dev.Info
	info.Name.Set(config.System.Device.Name)
	info.Model.Set(dev.model)
	info.Manufacturer.Set("Shelly")
	info.SerialNumber.Set(config.System.Device.MAC)
	info.FirmwareVersion.Set(config.System.Device.FirmwareID)
	info.WebURL.Set("http://" + dev.shelly.ip)
	dev.shelly.dev.Online.Online.Set(true)
	dev.shelly.dev.Online.LastSeen.Set(time.Now())

	if !dev.built {
		dev.build(config, status)
		dev.built = true
	} else {
		dev.refresh(config)
	}

	// A device reconnecting after a firmware update has finished updating.
	dev.update.Updating.Set(false)
	dev.update.CurrentVersion.Set(firmwareVersion(config.System.Device.FirmwareID))
	if stable, found := status.System.AvailableUpdates[updateStage]; found {
		dev.update.Available.Set(true)
		dev.update.UpdateVersion.Set(stable.Version)
	} else {
		dev.update.Available.Set(false)
	}

	// The state at connect is not a change that happened, so do not emit
	// input events for it.
	dev.applyStatus(status.Inputs, status.Switches, false)
}

func (dev *ShellyPlus) onDisconnected() {
	dev.shelly.dev.Online.Online.Set(false)
}

func (dev *ShellyPlus) onNotificationFrame(frame NotificationFrame) {
	dev.shelly.dev.Online.Online.Set(true)
	dev.shelly.dev.Online.LastSeen.Set(time.Now())

	if frame.NotifyStatus != nil {
		dev.applyStatus(frame.NotifyStatus.Inputs, frame.NotifyStatus.Switches, true)
	}
	if frame.NotifyEvent != nil {
		for _, ev := range frame.NotifyEvent.Events {
			dev.handleComponentEvent(ev)
		}
	}
}

// build creates the endpoints and services from the device's components. It
// must run before the device is added to the bridge.
func (dev *ShellyPlus) build(config GetConfigResponse, status GetStatusResponse) {
	root := dev.shelly.dev

	switchStatus := make(map[int]GetStatusResponseSwitch)
	for _, val := range status.Switches {
		switchStatus[val.ID] = val
	}

	// Each switch is an output endpoint.
	for _, val := range sortedSwitches(config.Switches) {
		id := val.ID
		sw := &plusSwitch{
			id:    id,
			ep:    root.Endpoint(endpointID(id)),
			onOff: services.NewOnOff(""),
		}
		sw.ep.AddService(sw.onOff)
		sw.onOff.On.OnWrite(func(on bool) error { return dev.setSwitch(id, sw, on) })
		sw.onOff.OnToggle(func() error { return dev.toggleSwitch(id, sw) })

		// Only add what the device reports.
		st := switchStatus[id]
		if st.AveragePower != nil || st.Voltage != nil || st.Current != nil || st.AverageEnergy != nil {
			sw.meter = services.NewPowerMeter("")
			sw.ep.AddService(sw.meter)
		}
		if st.Temperature != nil {
			sw.temp = services.NewDeviceTemperature("")
			sw.ep.AddService(sw.temp)
		}

		dev.switches[id] = sw
	}

	// Each input is a service of the type configured on the device. It shares
	// the endpoint of the switch with the same ID, as on the 2PM, else has its
	// own endpoint.
	for _, val := range sortedInputs(config.Inputs) {
		id := val.ID
		if !supportedInputType(val.Type) {
			dev.shelly.log.Warnf("config contained unsupported input type %q for %+v", val.Type, val)
			continue
		}
		inp := &plusInput{id: id, typ: val.Type}
		if sw, found := dev.switches[id]; found {
			inp.ep = sw.ep
			inp.paired = true
		} else {
			inp.ep = root.Endpoint(endpointID(id))
		}

		switch val.Type {
		case inputTypeSwitch:
			inp.binary = services.NewBinaryInput("")
			inp.binary.On.Set(false)
			inp.ep.AddService(inp.binary)

		case inputTypeButton:
			inp.button = services.NewButton("")
			inp.button.LastEvent.SetOptions(buttonEvents)
			inp.button.LastEvent.Set("release")
			inp.ep.AddService(inp.button)

		case inputTypeAnalog:
			inp.analog = services.NewAnalogInput("", services.AnalogInputFeatureRangeMap)
			inp.ep.AddService(inp.analog)
			inp.analog.RangeMap.OnWrite(func(fields []*clientsapi.FieldValue) error {
				return dev.setRangeMap(id, inp, types.ParseAnalogInputRangeMap(fields))
			})
		}

		dev.inputs[id] = inp
	}

	dev.refresh(config)
}

// refresh updates the endpoint labels and the analog range maps from the
// config. It also reports changes the device made to its components that need
// the bridge to be restarted to take effect.
func (dev *ShellyPlus) refresh(config GetConfigResponse) {
	for _, val := range config.Switches {
		sw, found := dev.switches[val.ID]
		if !found {
			dev.shelly.log.Warnf("config contained new switch %+v, restart the bridge to use it", val)
			continue
		}
		sw.ep.SetLabel(componentLabel(val.Name, "Output", val.ID))
	}

	for _, val := range config.Inputs {
		inp, found := dev.inputs[val.ID]
		if !found {
			if supportedInputType(val.Type) {
				dev.shelly.log.Warnf("config contained new input %+v, restart the bridge to use it", val)
			}
			continue
		}
		if val.Type != inp.typ {
			dev.shelly.log.Warnf("input %d changed type from %q to %q, restart the bridge to use it", val.ID, inp.typ, val.Type)
			continue
		}
		if !inp.paired {
			inp.ep.SetLabel(componentLabel(val.Name, "Input", val.ID))
		}
		if inp.analog != nil {
			min, max := 0.0, 100.0 // the device default
			if len(val.RangeMap) == 2 {
				min, max = val.RangeMap[0], val.RangeMap[1]
			}
			inp.setRange(min, max)
		}
	}
}

// supportedInputType reports whether inputs of the type are published. Counter
// inputs are not.
func supportedInputType(typ string) bool {
	return typ == inputTypeSwitch || typ == inputTypeButton || typ == inputTypeAnalog
}

// componentLabel returns the configured name of a component, or "<kind> <N>"
// numbered from 1, matching the endpoint ID.
func componentLabel(name, kind string, index int) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("%s %d", kind, index+1)
}

// applyStatus updates the services from component status. It is used for the
// full status on connect and for the partial status of notifications. When
// emit is set, changes of a binary input also emit the changed event.
func (dev *ShellyPlus) applyStatus(inputs []GetStatusResponseInput, switches []GetStatusResponseSwitch, emit bool) {
	for _, val := range inputs {
		inp, found := dev.inputs[val.ID]
		if !found {
			continue
		}
		inp.applyStatus(val, emit, func(err error) {
			dev.shelly.log.Errorf("failed to emit input %d event: %s", val.ID, err)
		})
	}

	for _, val := range switches {
		sw, found := dev.switches[val.ID]
		if !found {
			dev.shelly.log.Warnf("status contained unexpected switch %+v", val)
			continue
		}
		sw.applyStatus(val)
	}
}

func (sw *plusSwitch) applyStatus(val GetStatusResponseSwitch) {
	if val.Output != nil {
		sw.onOff.On.Set(*val.Output)
	}
	if sw.meter != nil {
		if val.AveragePower != nil {
			sw.meter.Power.Set(*val.AveragePower)
		}
		if val.Voltage != nil {
			sw.meter.Voltage.Set(*val.Voltage)
		}
		if val.Current != nil {
			sw.meter.Current.Set(*val.Current)
		}
		if val.AverageEnergy != nil {
			// The device reports Wh, the service kWh.
			sw.meter.Energy.Set(val.AverageEnergy.Total / 1000)
		}
	}
	if sw.temp != nil && val.Temperature != nil && val.Temperature.Centigrade != nil {
		sw.temp.Temperature.Set(*val.Temperature.Centigrade)
	}
}

func (inp *plusInput) applyStatus(val GetStatusResponseInput, emit bool, onErr func(error)) {
	switch {
	case inp.binary != nil:
		if val.State == nil {
			return
		}
		if inp.binary.On.Set(*val.State) && emit {
			err := inp.binary.EmitChanged(types.BinaryInputChangedEvent{On: *val.State})
			if err != nil {
				onErr(err)
			}
		}

	case inp.analog != nil:
		if val.Percent == nil {
			return
		}
		inp.mu.Lock()
		inp.percent = val.Percent
		inp.mapLocked()
		inp.mu.Unlock()
		inp.analog.Value.Set(*val.Percent)
	}
	// A button has no state, only events.
}

// setRange records the analog range map and publishes it.
func (inp *plusInput) setRange(min, max float64) {
	inp.mu.Lock()
	defer inp.mu.Unlock()
	inp.rangeMin, inp.rangeMax, inp.rangeInit = min, max, true
	inp.analog.SetRangeMap(types.AnalogInputRangeMap{Min: min, Max: max})
	inp.mapLocked()
}

// mapLocked maps the last reading, 0-100%, onto the range map. The caller
// holds the lock.
func (inp *plusInput) mapLocked() {
	if inp.percent == nil || !inp.rangeInit {
		return
	}
	inp.analog.Mapped.Set(inp.rangeMin + *inp.percent/100*(inp.rangeMax-inp.rangeMin))
}

// handleComponentEvent turns a button input event into the button service's
// events and last_event/last_duration. Other components' events are ignored.
//
// Shelly event -> Woodhouse event:
//
//	btn_down     press
//	btn_up       release, and long_release if a long_push came before it
//	single_push  multi_press count 1
//	double_push  multi_press count 2
//	triple_push  multi_press count 3
//	long_push    hold
func (dev *ShellyPlus) handleComponentEvent(ev ComponentEvent) {
	if !strings.HasPrefix(ev.Component, "input:") {
		return
	}
	inp, found := dev.inputs[ev.ID]
	if !found || inp.button == nil {
		return
	}
	b := inp.button

	emit := func(name string, send func() error) {
		b.LastEvent.Set(name)
		err := send()
		if err != nil {
			dev.shelly.log.Errorf("failed to emit input %d %s event: %s", ev.ID, name, err)
		}
	}
	multi := func(count int64) {
		emit("multi_press", func() error { return b.EmitMultiPress(types.ButtonMultiPressEvent{Count: count}) })
	}

	switch ev.Event {
	case "btn_down":
		inp.downTs, inp.downAt, inp.held = ev.Timestamp, time.Now(), false
		emit("press", b.EmitPress)

	case "btn_up":
		emit("release", b.EmitRelease)
		if inp.held {
			inp.held = false
			d := inp.heldFor(ev.Timestamp)
			b.LastDuration.Set(d)
			emit("long_release", func() error { return b.EmitLongRelease(types.ButtonLongReleaseEvent{Duration: d}) })
		}

	case "single_push":
		multi(1)
	case "double_push":
		multi(2)
	case "triple_push":
		multi(3)

	case "long_push":
		inp.held = true
		emit("hold", b.EmitHold)

	default:
		dev.shelly.log.Warnf("unknown input %d event %q", ev.ID, ev.Event)
	}
}

// heldFor returns how long the button was held, from btn_down to the btn_up
// at upTs. The device timestamps are used when it has a clock, else the time
// the events arrived.
func (inp *plusInput) heldFor(upTs float64) time.Duration {
	if inp.downTs > 0 && upTs > inp.downTs {
		return time.Duration((upTs - inp.downTs) * float64(time.Second)).Round(time.Millisecond)
	}
	if inp.downAt.IsZero() {
		return 0
	}
	return time.Since(inp.downAt).Round(time.Millisecond)
}

func (dev *ShellyPlus) handleRename(newName string) error {
	err := dev.shelly.RequestSetDeviceName(newName)
	if err != nil {
		dev.shelly.log.Errorf("failed to rename device: %s", err)
		return fmt.Errorf("failed to rename device: %w", err)
	}
	dev.shelly.log.Infof("renamed device to %q", newName)
	dev.shelly.dev.Info.Name.Set(newName)
	return nil
}

func (dev *ShellyPlus) handleUpdateStart() error {
	if !dev.update.Available.Get() {
		return errors.New("no firmware update available")
	}
	err := dev.shelly.RequestUpdate(updateStage)
	if err != nil {
		dev.shelly.log.Errorf("failed to start update: %s", err)
		return fmt.Errorf("failed to start update: %w", err)
	}
	dev.shelly.log.Infof("started firmware update")
	dev.update.Updating.Set(true)
	return nil
}

func (dev *ShellyPlus) setSwitch(id int, sw *plusSwitch, on bool) error {
	err := dev.shelly.RequestSwitchSet(SwitchSet{ID: id, On: on})
	if err != nil {
		dev.shelly.log.Errorf("failed to set switch: %s", err)
		return fmt.Errorf("failed to set switch: %w", err)
	}
	sw.onOff.On.Set(on)
	return nil
}

func (dev *ShellyPlus) toggleSwitch(id int, sw *plusSwitch) error {
	wasOn, err := dev.shelly.RequestSwitchToggle(id)
	if err != nil {
		dev.shelly.log.Errorf("failed to toggle switch: %s", err)
		return fmt.Errorf("failed to toggle switch: %w", err)
	}
	sw.onOff.On.Set(!wasOn)
	return nil
}

func (dev *ShellyPlus) setRangeMap(id int, inp *plusInput, rm types.AnalogInputRangeMap) error {
	if rm.Max < rm.Min {
		return fmt.Errorf("range map max %g is below min %g", rm.Max, rm.Min)
	}
	err := dev.shelly.RequestInputSetRangeMap(id, rm.Min, rm.Max)
	if err != nil {
		dev.shelly.log.Errorf("failed to set range map: %s", err)
		return fmt.Errorf("failed to set range map: %w", err)
	}
	inp.setRange(rm.Min, rm.Max)
	return nil
}

// firmwareVersion extracts the version from a firmware ID such as
// "<build date>/<version>-g<commit>", or returns the ID if it has another
// form.
func firmwareVersion(fwID string) string {
	_, ver, found := strings.Cut(fwID, "/")
	if !found {
		return fwID
	}
	if i := strings.Index(ver, "-g"); i > 0 {
		ver = ver[:i]
	}
	return ver
}

func sortedSwitches(in []GetConfigResponseSwitch) []GetConfigResponseSwitch {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b GetConfigResponseSwitch) int { return a.ID - b.ID })
	return out
}

func sortedInputs(in []GetConfigResponseInput) []GetConfigResponseInput {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b GetConfigResponseInput) int { return a.ID - b.ID })
	return out
}
