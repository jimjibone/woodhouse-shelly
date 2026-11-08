package shelly_v1

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	clientsapi "github.com/jimjibone/woodhouse-api/go/v2/clients"
)

// Payloads in the shape of Dimmer 2 responses. MAC addresses are placeholders.
const dimmer2Settings = `{
  "device": {"type": "SHDM-2", "mac": "AABBCCDDEEFF", "hostname": "shellydimmer2-AABBCCDDEEFF"},
  "name": "Test Dimmer",
  "fw": "20230913-114008/v1.14.0-gcb84623",
  "lights": [{"name": "Hall", "ison": false, "brightness": 100}]
}`

const dimmer2Status = `{
  "time": "12:00",
  "has_update": true,
  "tmp": {"tC": 45.5, "tF": 113.9, "is_valid": true},
  "loaderror": 0,
  "overload": 0,
  "update": {"status": "idle", "has_update": true, "new_version": "20240101-000000/v1.15.0-gabcdef1", "old_version": "20230913-114008/v1.14.0-gcb84623"}
}`

const dimmer2Light = `{"ison": true, "source": "http", "mode": "white", "brightness": 40, "transition": 0}`

func newTestDimmer(t *testing.T) (*ShellyDimmer2, <-chan *clientsapi.Device) {
	t.Helper()
	dev, err := newShellyDimmer2("shellydimmer2-test", "aa:bb:cc:dd:ee:ff", "192.0.2.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dev.Close)

	ch := make(chan *clientsapi.Device, 64)
	dev.dev.Init(func(state *clientsapi.Device) { ch <- state })

	var settings DeviceSettings
	if err := json.Unmarshal([]byte(dimmer2Settings), &settings); err != nil {
		t.Fatal(err)
	}
	var status DeviceStatusSummary
	if err := json.Unmarshal([]byte(dimmer2Status), &status); err != nil {
		t.Fatal(err)
	}
	var state ShellyDimmer2State
	if err := json.Unmarshal([]byte(dimmer2Light), &state); err != nil {
		t.Fatal(err)
	}
	dev.applySettings(settings)
	dev.applyStatus(status)
	dev.applyState(state)
	return dev, ch
}

func fullState(t *testing.T, dev *ShellyDimmer2, ch <-chan *clientsapi.Device) *clientsapi.Device {
	t.Helper()
	dev.dev.SendFullState()
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

func attr(srv *clientsapi.Service, id string) *clientsapi.Attribute {
	for _, a := range srv.Attrs {
		if a.Id == id {
			return a
		}
	}
	return nil
}

func TestDimmer2Shape(t *testing.T) {
	dev, ch := newTestDimmer(t)
	full := fullState(t, dev, ch)

	if full.Id != "shellyaabbccddeeff" {
		t.Errorf("device id: %q", full.Id)
	}
	if len(full.Endpoints) != 2 {
		t.Fatalf("endpoints: %v", full.Endpoints)
	}

	root, out := full.Endpoints[0], full.Endpoints[1]
	var rootTypes []string
	for _, srv := range root.Services {
		rootTypes = append(rootTypes, srv.Typ.String())
	}
	if want := []string{"INFO", "ONLINE", "UPDATE", "DEVICE_TEMPERATURE"}; !slices.Equal(rootTypes, want) {
		t.Errorf("root services: %v, want %v", rootTypes, want)
	}
	if root.Id != "0" || out.Id != "1" || out.Label != "Hall" {
		t.Errorf("endpoints: %q, %q (%q)", root.Id, out.Id, out.Label)
	}

	if len(out.Services) != 1 || out.Services[0].Typ != clientsapi.Service_LIGHT {
		t.Fatalf("output services: %v", out.Services)
	}
	light := out.Services[0]
	if !slices.Equal(light.Features, []string{"brightness"}) {
		t.Errorf("light features: %v", light.Features)
	}
	if !attr(light, "on").GetValue().GetBool() {
		t.Error("light off")
	}
	if got := attr(light, "brightness").GetValue().GetInt(); got != 40 {
		t.Errorf("brightness: %d", got)
	}
	if attr(light, "color_temp") != nil || attr(light, "color") != nil {
		t.Error("light has colour attributes")
	}

	// The transition of light.set is limited to the Dimmer 2's 5s.
	var set *clientsapi.CommandSpec
	for _, c := range light.Commands {
		if c.Id == "set" {
			set = c
		}
	}
	if set == nil {
		t.Fatal("light has no set command")
	}
	var found bool
	for _, p := range set.Params {
		if p.Id == "transition" {
			found = true
			if got := p.Spec.GetDuration().GetMax(); got != 5000 {
				t.Errorf("transition max: %d ms", got)
			}
		}
	}
	if !found {
		t.Error("set has no transition param")
	}

	// Update and temperature.
	var upd, tmp *clientsapi.Service
	for _, srv := range root.Services {
		switch srv.Typ {
		case clientsapi.Service_UPDATE:
			upd = srv
		case clientsapi.Service_DEVICE_TEMPERATURE:
			tmp = srv
		}
	}
	if !attr(upd, "available").GetValue().GetBool() {
		t.Error("update not available")
	}
	if got := attr(upd, "update_version").GetValue().GetText(); got != "20240101-000000/v1.15.0-gabcdef1" {
		t.Errorf("update version: %q", got)
	}
	if !slices.ContainsFunc(upd.Commands, func(c *clientsapi.CommandSpec) bool { return c.Id == "start" }) {
		t.Error("update has no start command")
	}
	if got := attr(tmp, "temperature").GetValue().GetFloat(); got != 45.5 {
		t.Errorf("temperature: %v", got)
	}
}

func TestDimmer2NoTemperature(t *testing.T) {
	dev, err := newShellyDimmer2("shellydimmer2-test", "aabbccddeeff", "192.0.2.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	var settings DeviceSettings
	if err := json.Unmarshal([]byte(`{"device": {"mac": "AABBCCDDEEFF"}, "name": "x", "lights": [{"name": ""}]}`), &settings); err != nil {
		t.Fatal(err)
	}
	var status DeviceStatusSummary
	if err := json.Unmarshal([]byte(`{"update": {"status": "idle"}}`), &status); err != nil {
		t.Fatal(err)
	}
	dev.applySettings(settings)
	dev.applyStatus(status)

	ch := make(chan *clientsapi.Device, 8)
	dev.dev.Init(func(state *clientsapi.Device) { ch <- state })
	full := fullState(t, dev, ch)
	for _, srv := range full.Endpoints[0].Services {
		if srv.Typ == clientsapi.Service_DEVICE_TEMPERATURE {
			t.Error("temperature service without a reported temperature")
		}
	}
	if got := full.Endpoints[1].Label; got != "Output 1" {
		t.Errorf("label: %q", got)
	}
}

func TestDimmer2BadMAC(t *testing.T) {
	_, err := newShellyDimmer2("shellydimmer2-test", "", "192.0.2.1", nil)
	if err == nil {
		t.Error("expected error for empty mac")
	}
}

func TestLightParams(t *testing.T) {
	on, off := true, false
	bri, high, low := int64(40), int64(200), int64(-5)
	trans, long := 1500*time.Millisecond, time.Minute

	for _, c := range []struct {
		name string
		on   *bool
		bri  *int64
		tr   *time.Duration
		want string
	}{
		{"nothing", nil, nil, nil, ""},
		{"on", &on, nil, nil, "turn=on"},
		{"off", &off, nil, nil, "turn=off"},
		{"brightness", nil, &bri, nil, "brightness=40"},
		{"brightness clamped", nil, &high, nil, "brightness=100"},
		{"brightness floor", nil, &low, nil, "brightness=0"},
		{"all", &on, &bri, &trans, "turn=on&brightness=40&transition=1500"},
		{"transition clamped", nil, nil, &long, "transition=5000"},
	} {
		if got := lightParams(c.on, c.bri, c.tr); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
