package shelly_v2

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	clientsapi "github.com/jimjibone/woodhouse-api/go/v2/clients"
	"github.com/jimjibone/woodhouse-api/values"
)

// Payloads in the shape of Shelly.GetConfig and Shelly.GetStatus responses.
// MAC addresses are placeholders.

// A Plus 2PM: two metered outputs with a temperature each, input 0 a switch
// and input 1 a button.
const plus2PMConfig = `{
  "ble": {"enable": false},
  "input:0": {"id": 0, "name": "Wall switch", "type": "switch", "invert": false},
  "input:1": {"id": 1, "name": null, "type": "button", "invert": false},
  "switch:0": {"id": 0, "name": "Lamp", "in_mode": "follow"},
  "switch:1": {"id": 1, "name": null, "in_mode": "momentary"},
  "sys": {"device": {"name": "Test 2PM", "mac": "AABBCCDDEEFF", "fw_id": "20250101-000000/1.5.0-gabcdef1"}}
}`

const plus2PMStatus = `{
  "input:0": {"id": 0, "state": true},
  "input:1": {"id": 1, "state": null},
  "switch:0": {"id": 0, "source": "init", "output": false, "apower": 0.0, "voltage": 230.1, "current": 0.0,
    "aenergy": {"total": 1500.0, "by_minute": [0.0, 0.0, 0.0], "minute_ts": 1},
    "temperature": {"tC": 40.5, "tF": 104.9}},
  "switch:1": {"id": 1, "source": "init", "output": true, "apower": 60.5, "voltage": 230.2, "current": 0.26,
    "aenergy": {"total": 2500.0, "by_minute": [0.0, 0.0, 0.0], "minute_ts": 1},
    "temperature": {"tC": null, "tF": null}},
  "sys": {"mac": "AABBCCDDEEFF", "available_updates": {"stable": {"version": "1.6.0"}}}
}`

// A Plus Plug UK: one output, no inputs, metered without a temperature.
const plugUKConfig = `{
  "switch:0": {"id": 0, "name": null, "in_mode": "detached"},
  "sys": {"device": {"name": "Test Plug", "mac": "AABBCCDDEE01", "fw_id": "20250101-000000/1.5.0-gabcdef1"}}
}`

const plugUKStatus = `{
  "switch:0": {"id": 0, "source": "init", "output": true, "apower": 12.0, "voltage": 229.9, "current": 0.05,
    "aenergy": {"total": 10.0, "by_minute": [0.0, 0.0, 0.0], "minute_ts": 1}},
  "sys": {"mac": "AABBCCDDEE01", "available_updates": {}}
}`

// A Plus 1PM with one output that only reports its state (not metered), and an
// analog input and a counter input on their own endpoints.
const mixedConfig = `{
  "input:0": {"id": 0, "name": null, "type": "analog", "invert": false, "range_map": [0, 10]},
  "input:1": {"id": 1, "name": "Pulses", "type": "count"},
  "input:2": {"id": 2, "name": "Door", "type": "switch"},
  "switch:0": {"id": 0, "name": null, "in_mode": "detached"},
  "sys": {"device": {"name": "Test Mixed", "mac": "AABBCCDDEE02", "fw_id": "dev"}}
}`

const mixedStatus = `{
  "input:0": {"id": 0, "percent": 50.0, "xpercent": null},
  "input:2": {"id": 2, "state": false},
  "switch:0": {"id": 0, "output": false},
  "sys": {"mac": "AABBCCDDEE02"}
}`

func mustConfig(t *testing.T, s string) GetConfigResponse {
	t.Helper()
	var v GetConfigResponse
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("config: %s", err)
	}
	return v
}

func mustStatus(t *testing.T, s string) GetStatusResponse {
	t.Helper()
	var v GetStatusResponse
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("status: %s", err)
	}
	return v
}

// newTestDevice builds a device from a config and status as if it had just
// connected, and returns what it sends to the bridge.
func newTestDevice(t *testing.T, config, status string) (*ShellyPlus, <-chan *clientsapi.Device) {
	t.Helper()
	dev, err := newShellyPlus("Shelly Test", "shellytest-aabbccddeeff", "aabbccddeeff", "192.0.2.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dev.Close)

	ch := make(chan *clientsapi.Device, 64)
	dev.shelly.dev.Init(func(state *clientsapi.Device) { ch <- state })
	dev.onConnected(mustConfig(t, config), mustStatus(t, status))
	return dev, ch
}

// fullState returns the device's full state, skipping anything sent before it.
func fullState(t *testing.T, dev *ShellyPlus, ch <-chan *clientsapi.Device) *clientsapi.Device {
	t.Helper()
	dev.shelly.dev.SendFullState()
	deadline := time.After(time.Second)
	for {
		select {
		case d := <-ch:
			if d.FullState {
				return d
			}
		case <-deadline:
			t.Fatal("no full state")
			return nil
		}
	}
}

// shape lists the service types of each endpoint, keyed by endpoint ID.
func shape(d *clientsapi.Device) map[string][]string {
	out := make(map[string][]string)
	for _, ep := range d.Endpoints {
		types := []string{}
		for _, srv := range ep.Services {
			types = append(types, srv.Typ.String())
		}
		out[ep.Id] = types
	}
	return out
}

func endpoint(t *testing.T, d *clientsapi.Device, id string) *clientsapi.Endpoint {
	t.Helper()
	for _, ep := range d.Endpoints {
		if ep.Id == id {
			return ep
		}
	}
	t.Fatalf("no endpoint %q", id)
	return nil
}

func service(t *testing.T, d *clientsapi.Device, endpointID string, typ clientsapi.Service_ServiceType) *clientsapi.Service {
	t.Helper()
	for _, srv := range endpoint(t, d, endpointID).Services {
		if srv.Typ == typ {
			return srv
		}
	}
	t.Fatalf("no %s on endpoint %q", typ, endpointID)
	return nil
}

func attr(t *testing.T, srv *clientsapi.Service, id string) *clientsapi.Attribute {
	t.Helper()
	for _, a := range srv.Attrs {
		if a.Id == id {
			return a
		}
	}
	return nil
}

func wantShape(t *testing.T, got map[string][]string, want map[string][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("endpoints: got %v, want %v", got, want)
	}
	for id, w := range want {
		if !slices.Equal(got[id], w) {
			t.Errorf("endpoint %q: got %v, want %v", id, got[id], w)
		}
	}
}

func TestPlus2PMShape(t *testing.T) {
	dev, ch := newTestDevice(t, plus2PMConfig, plus2PMStatus)
	full := fullState(t, dev, ch)

	if full.Id != "shellyaabbccddeeff" {
		t.Errorf("device id: %q", full.Id)
	}
	wantShape(t, shape(full), map[string][]string{
		"0": {"INFO", "ONLINE", "UPDATE"},
		"1": {"ON_OFF", "POWER_METER", "DEVICE_TEMPERATURE", "BINARY_INPUT"},
		"2": {"ON_OFF", "POWER_METER", "DEVICE_TEMPERATURE", "BUTTON"},
	})

	// Labels: the configured name, else Output N.
	if got := endpoint(t, full, "1").Label; got != "Lamp" {
		t.Errorf("endpoint 1 label: %q", got)
	}
	if got := endpoint(t, full, "2").Label; got != "Output 2" {
		t.Errorf("endpoint 2 label: %q", got)
	}

	// Info and update.
	info := service(t, full, "0", clientsapi.Service_INFO)
	if got := attr(t, info, "name").GetValue().GetText(); got != "Test 2PM" {
		t.Errorf("name: %q", got)
	}
	if got := attr(t, info, "serial_number").GetValue().GetText(); got != "AABBCCDDEEFF" {
		t.Errorf("serial number: %q", got)
	}
	if got := attr(t, info, "name").GetPerms(); got != clientsapi.Permissions_PERM_READWRITE {
		// Rename is enabled by the bridge.
		t.Errorf("name perms: %v", got)
	}
	upd := service(t, full, "0", clientsapi.Service_UPDATE)
	if !attr(t, upd, "available").GetValue().GetBool() {
		t.Error("update not available")
	}
	if got := attr(t, upd, "update_version").GetValue().GetText(); got != "1.6.0" {
		t.Errorf("update version: %q", got)
	}
	if got := attr(t, upd, "current_version").GetValue().GetText(); got != "1.5.0" {
		t.Errorf("current version: %q", got)
	}
	if !slices.ContainsFunc(upd.Commands, func(c *clientsapi.CommandSpec) bool { return c.Id == "start" }) {
		t.Error("update has no start command")
	}

	// Output states and metering.
	if attr(t, service(t, full, "1", clientsapi.Service_ON_OFF), "on").GetValue().GetBool() {
		t.Error("output 1 on")
	}
	if !attr(t, service(t, full, "2", clientsapi.Service_ON_OFF), "on").GetValue().GetBool() {
		t.Error("output 2 off")
	}
	m2 := service(t, full, "2", clientsapi.Service_POWER_METER)
	if got := attr(t, m2, "power").GetValue().GetFloat(); got != 60.5 {
		t.Errorf("power: %v", got)
	}
	if got := attr(t, m2, "energy").GetValue().GetFloat(); got != 2.5 {
		t.Errorf("energy kWh: %v", got)
	}
	if got := attr(t, service(t, full, "1", clientsapi.Service_DEVICE_TEMPERATURE), "temperature").GetValue().GetFloat(); got != 40.5 {
		t.Errorf("temperature: %v", got)
	}

	// The switch input follows the configured type and reports its state;
	// the button lists the events it emits.
	if !attr(t, service(t, full, "1", clientsapi.Service_BINARY_INPUT), "on").GetValue().GetBool() {
		t.Error("input 0 off")
	}
	btn := service(t, full, "2", clientsapi.Service_BUTTON)
	opts := attr(t, btn, "last_event").GetSpec().GetEnum().GetOptions()
	if !slices.Equal(opts, buttonEvents) {
		t.Errorf("button options: %v", opts)
	}

	// Switch services have the toggle command.
	if !slices.ContainsFunc(service(t, full, "1", clientsapi.Service_ON_OFF).Commands, func(c *clientsapi.CommandSpec) bool { return c.Id == "toggle" }) {
		t.Error("on_off has no toggle command")
	}
}

func TestPlugUKShape(t *testing.T) {
	dev, ch := newTestDevice(t, plugUKConfig, plugUKStatus)
	full := fullState(t, dev, ch)

	// One output, metered, no temperature reported, no inputs.
	wantShape(t, shape(full), map[string][]string{
		"0": {"INFO", "ONLINE", "UPDATE"},
		"1": {"ON_OFF", "POWER_METER"},
	})
	if got := endpoint(t, full, "1").Label; got != "Output 1" {
		t.Errorf("label: %q", got)
	}
	meter := service(t, full, "1", clientsapi.Service_POWER_METER)
	for _, id := range []string{"power", "voltage", "current", "energy"} {
		if attr(t, meter, id) == nil {
			t.Errorf("meter has no %s", id)
		}
	}
	if attr(t, service(t, full, "0", clientsapi.Service_UPDATE), "update_version") != nil {
		t.Error("update version set with no update available")
	}
	if attr(t, service(t, full, "0", clientsapi.Service_UPDATE), "available").GetValue().GetBool() {
		t.Error("update available")
	}
}

func TestUnpairedInputsAndUnmeteredOutput(t *testing.T) {
	dev, ch := newTestDevice(t, mixedConfig, mixedStatus)
	full := fullState(t, dev, ch)

	// Output 0 has an id of its own; input 0 is paired with it so is an
	// analog input on endpoint 1. Input 1 is a counter, unsupported. Input 2
	// has no switch so has its own endpoint 3.
	wantShape(t, shape(full), map[string][]string{
		"0": {"INFO", "ONLINE", "UPDATE"},
		"1": {"ON_OFF", "ANALOG_INPUT"},
		"3": {"BINARY_INPUT"},
	})
	if got := endpoint(t, full, "3").Label; got != "Door" {
		t.Errorf("label: %q", got)
	}

	analog := service(t, full, "1", clientsapi.Service_ANALOG_INPUT)
	if !slices.Equal(analog.Features, []string{"range_map"}) {
		t.Errorf("features: %v", analog.Features)
	}
	if got := attr(t, analog, "value").GetValue().GetFloat(); got != 50 {
		t.Errorf("value: %v", got)
	}
	if got := attr(t, analog, "mapped").GetValue().GetFloat(); got != 5 {
		t.Errorf("mapped: %v", got)
	}
	rm := values.Fields(attr(t, analog, "range_map").GetValue().GetStruct().GetFields())
	if rm["min"].GetFloat() != 0 || rm["max"].GetFloat() != 10 {
		t.Errorf("range map: %v", rm)
	}
	if attr(t, analog, "range_map").GetPerms() != clientsapi.Permissions_PERM_READWRITE {
		t.Error("range map not writable")
	}

	// A new reading is mapped onto the range.
	dev.onNotificationFrame(frame(t, `{"method": "NotifyStatus", "params": {"ts": 1.0, "input:0": {"id": 0, "percent": 80.0}}}`))
	if got := analog2(t, dev, ch).GetValue().GetFloat(); got != 8 {
		t.Errorf("mapped after update: %v", got)
	}
}

// analog2 returns the mapped attribute of the analog input after an update.
func analog2(t *testing.T, dev *ShellyPlus, ch <-chan *clientsapi.Device) *clientsapi.Attribute {
	t.Helper()
	full := fullState(t, dev, ch)
	return attr(t, service(t, full, "1", clientsapi.Service_ANALOG_INPUT), "mapped")
}

func frame(t *testing.T, s string) NotificationFrame {
	t.Helper()
	var f NotificationFrame
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		t.Fatalf("frame: %s", err)
	}
	return f
}

// events waits for n events to be sent.
func events(t *testing.T, ch <-chan *clientsapi.Device, n int) []*clientsapi.Event {
	t.Helper()
	var out []*clientsapi.Event
	deadline := time.After(2 * time.Second)
	for len(out) < n {
		select {
		case d := <-ch:
			out = append(out, d.Events...)
		case <-deadline:
			t.Fatalf("got %d events, want %d", len(out), n)
		}
	}
	return out
}

func eventNames(evs []*clientsapi.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.EndpointId+"/"+e.ServiceId+"/"+e.Id)
	}
	return out
}

func TestBinaryInputEmitsChangedOnlyOnChange(t *testing.T) {
	dev, ch := newTestDevice(t, plus2PMConfig, plus2PMStatus)
	fullState(t, dev, ch) // drain

	// Input 0 starts on. The same state again is no change; off is.
	dev.onNotificationFrame(frame(t, `{"method": "NotifyStatus", "params": {"ts": 1.0, "input:0": {"id": 0, "state": true}}}`))
	dev.onNotificationFrame(frame(t, `{"method": "NotifyStatus", "params": {"ts": 2.0, "input:0": {"id": 0, "state": false}}}`))

	evs := events(t, ch, 1)
	if got := eventNames(evs); !slices.Equal(got, []string{"1/binary_input/changed"}) {
		t.Fatalf("events: %v", got)
	}
	fields := values.Fields(evs[0].Fields)
	if v, ok := fields["on"]; !ok || v.GetBool() {
		t.Errorf("changed fields: %v", fields)
	}
}

func TestButtonEvents(t *testing.T) {
	dev, ch := newTestDevice(t, plus2PMConfig, plus2PMStatus)
	fullState(t, dev, ch) // drain

	notify := func(event string, ts float64) {
		b, _ := json.Marshal(map[string]any{
			"method": "NotifyEvent",
			"params": map[string]any{
				"ts":     ts,
				"events": []map[string]any{{"component": "input:1", "id": 1, "event": event, "ts": ts}},
			},
		})
		dev.onNotificationFrame(frame(t, string(b)))
	}

	// A short press, a double press, then a long press held for 1.5s.
	notify("btn_down", 100.0)
	notify("btn_up", 100.1)
	notify("single_push", 100.4)
	notify("double_push", 110.0)
	notify("btn_down", 120.0)
	notify("long_push", 120.5)
	notify("btn_up", 121.5)
	// Events of other components are ignored.
	b, _ := json.Marshal(map[string]any{
		"method": "NotifyEvent",
		"params": map[string]any{"events": []map[string]any{{"component": "script:1", "id": 1, "event": "btn_down"}}},
	})
	dev.onNotificationFrame(frame(t, string(b)))

	want := []string{"press", "release", "multi_press", "multi_press", "press", "hold", "release", "long_release"}
	evs := events(t, ch, len(want))
	for i, w := range want {
		if evs[i].EndpointId != "2" || evs[i].ServiceId != "button" || evs[i].Id != w {
			t.Fatalf("event %d: %s/%s/%s, want 2/button/%s", i, evs[i].EndpointId, evs[i].ServiceId, evs[i].Id, w)
		}
	}
	counts := []int64{values.Fields(evs[2].Fields)["count"].GetInt(), values.Fields(evs[3].Fields)["count"].GetInt()}
	if !slices.Equal(counts, []int64{1, 2}) {
		t.Errorf("multi_press counts: %v", counts)
	}
	if got, ok := values.AsDuration(values.Fields(evs[7].Fields)["duration"]); !ok || got != 1500*time.Millisecond {
		t.Errorf("long_release duration: %v", got)
	}

	full := fullState(t, dev, ch)
	btn := service(t, full, "2", clientsapi.Service_BUTTON)
	if got := attr(t, btn, "last_event").GetValue().GetEnum(); got != "long_release" {
		t.Errorf("last_event: %q", got)
	}
	if got, _ := values.AsDuration(attr(t, btn, "last_duration").GetValue()); got != 1500*time.Millisecond {
		t.Errorf("last_duration: %v", got)
	}
}

func TestOfflineOnDisconnect(t *testing.T) {
	dev, ch := newTestDevice(t, plugUKConfig, plugUKStatus)
	dev.onDisconnected()
	full := fullState(t, dev, ch)
	if attr(t, service(t, full, "0", clientsapi.Service_ONLINE), "online").GetValue().GetBool() {
		t.Error("online after disconnect")
	}
}

func TestReconnectKeepsShape(t *testing.T) {
	dev, ch := newTestDevice(t, plus2PMConfig, plus2PMStatus)
	// Renamed channel, updated on reconnect.
	renamed := strings.Replace(plus2PMConfig, `"name": "Lamp"`, `"name": "Ceiling"`, 1)
	dev.onConnected(mustConfig(t, renamed), mustStatus(t, plus2PMStatus))
	full := fullState(t, dev, ch)
	wantShape(t, shape(full), map[string][]string{
		"0": {"INFO", "ONLINE", "UPDATE"},
		"1": {"ON_OFF", "POWER_METER", "DEVICE_TEMPERATURE", "BINARY_INPUT"},
		"2": {"ON_OFF", "POWER_METER", "DEVICE_TEMPERATURE", "BUTTON"},
	})
	if got := endpoint(t, full, "1").Label; got != "Ceiling" {
		t.Errorf("label: %q", got)
	}
}

func TestFirmwareVersion(t *testing.T) {
	for in, want := range map[string]string{
		"20250101-000000/1.5.0-gabcdef1": "1.5.0",
		"20250101-000000/1.5.0":          "1.5.0",
		"dev":                            "dev",
	} {
		if got := firmwareVersion(in); got != want {
			t.Errorf("firmwareVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEndpointID(t *testing.T) {
	// Component indexes start at 0, endpoint "0" is the root.
	if endpointID(0) != "1" || endpointID(1) != "2" {
		t.Errorf("endpoint ids: %q %q", endpointID(0), endpointID(1))
	}
}
