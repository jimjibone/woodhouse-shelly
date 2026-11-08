package shelly_v2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jimjibone/log"
	"github.com/jimjibone/wh/v2/bridges"
	clientsapi "github.com/jimjibone/woodhouse-api/go/v2/clients"
)

const (
	minBackoff = 1 * time.Second
	maxBackoff = 10 * time.Second
)

// ShellyComms manages the lifecycle of device comms.
type ShellyComms struct {
	log   *log.Context
	close func()
	wg    sync.WaitGroup

	bridge *bridges.Bridge
	dev    *bridges.Device
	added  bool

	ip     string
	nextIP string
	connMu sync.RWMutex
	conn   *websocket.Conn

	lastBackoff     time.Time
	lastRestart     time.Time
	backoffDuration time.Duration

	onConnected         func(config GetConfigResponse, status GetStatusResponse)
	onDisconnected      func()
	onResponseFrame     func(ResponseFrame)
	onNotificationFrame func(NotificationFrame)
}

// NewShellyComms creates the comms and the Woodhouse device, whose ID is
// derived from the MAC address. Set the handlers and then call Start.
func NewShellyComms(hostname, mac, ip string, typ clientsapi.Device_DeviceType, client *bridges.Bridge) (*ShellyComms, error) {
	id, err := bridges.DeviceID("shelly", mac)
	if err != nil {
		return nil, fmt.Errorf("device id from mac %q: %w", mac, err)
	}
	return &ShellyComms{
		log:    log.NewContext(log.DefaultLogger, hostname, log.DebugLevel),
		close:  func() {},
		bridge: client,
		dev:    bridges.NewDevice(id, typ),
		ip:     ip,
	}, nil
}

// Start connects to the device and keeps the connection up until Close.
func (dev *ShellyComms) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	dev.close = cancel
	dev.wg.Add(1)
	go dev.run(ctx)
}

func (dev *ShellyComms) Close() {
	dev.close()
	dev.wg.Wait()
	dev.dev.Close()
}

func (dev *ShellyComms) SetNextIP(ip string) {
	dev.nextIP = ip
}

func (dev *ShellyComms) OnConnected(handler func(config GetConfigResponse, status GetStatusResponse)) {
	dev.onConnected = handler
}

func (dev *ShellyComms) OnDisconnected(handler func()) {
	dev.onDisconnected = handler
}

func (dev *ShellyComms) OnResponseFrame(handler func(ResponseFrame)) {
	dev.onResponseFrame = handler
}

func (dev *ShellyComms) OnNotificationFrame(handler func(NotificationFrame)) {
	dev.onNotificationFrame = handler
}

// requestRPC calls an RPC method and returns its result. The request is made
// over HTTP rather than the websocket so that the response can be awaited and
// any device-reported error returned to the caller.
func (dev *ShellyComms) requestRPC(method string, params any) (json.RawMessage, error) {
	payload, err := json.Marshal(RequestFrame{
		ID:     "122",
		Src:    "woodhouse-4",
		Method: method,
		Params: params,
	})
	if err != nil {
		return nil, err
	}

	client := http.Client{Timeout: 10 * time.Second}
	res, err := client.Post(fmt.Sprintf("http://%s/rpc", dev.ip), "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var frame ResponseFrame
	err = json.Unmarshal(body, &frame)
	if err != nil {
		return nil, fmt.Errorf("unmarshal response frame: %w", err)
	}
	if frame.Error.Message != "" {
		return nil, fmt.Errorf("device error %d: %s", frame.Error.Code, frame.Error.Message)
	}

	return frame.Result, nil
}

// RequestSetDeviceName renames the device using the Sys.SetConfig RPC method.
func (dev *ShellyComms) RequestSetDeviceName(name string) error {
	_, err := dev.requestRPC("Sys.SetConfig", SysSetConfig{Config: SysConfig{Device: SysDeviceConfig{Name: name}}})
	return err
}

// RequestSwitchSet turns a switch on or off using the Switch.Set RPC method.
func (dev *ShellyComms) RequestSwitchSet(request SwitchSet) error {
	_, err := dev.requestRPC("Switch.Set", request)
	return err
}

// RequestSwitchToggle toggles a switch using the Switch.Toggle RPC method and
// returns whether it was on before.
func (dev *ShellyComms) RequestSwitchToggle(id int) (wasOn bool, err error) {
	result, err := dev.requestRPC("Switch.Toggle", SwitchToggle{ID: id})
	if err != nil {
		return false, err
	}
	var res struct {
		WasOn bool `json:"was_on"`
	}
	err = json.Unmarshal(result, &res)
	if err != nil {
		return false, fmt.Errorf("unmarshal toggle result: %w", err)
	}
	return res.WasOn, nil
}

// RequestInputSetRangeMap sets the range map of an analog input using the
// Input.SetConfig RPC method.
func (dev *ShellyComms) RequestInputSetRangeMap(id int, min, max float64) error {
	_, err := dev.requestRPC("Input.SetConfig", InputSetConfig{ID: id, Config: InputConfig{RangeMap: []float64{min, max}}})
	return err
}

// RequestUpdate starts a firmware update using the Shelly.Update RPC method.
func (dev *ShellyComms) RequestUpdate(stage string) error {
	_, err := dev.requestRPC("Shelly.Update", ShellyUpdate{Stage: stage})
	return err
}

func (dev *ShellyComms) run(ctx context.Context) {
	defer dev.wg.Done()

	dev.log.Infof("started")
	defer dev.log.Infof("finished")

	// Close the websocket when the context is cancelle
	go func() {
		<-ctx.Done()
		dev.disconnect()
	}()

	for {
		// Try to connect.
		err := dev.connect()
		if err != nil {
			dev.log.Errorf("failed to connect: %s", err)

			// If this failed, try switching to the next IP if there is one.
			if dev.nextIP != "" && dev.ip != dev.nextIP {
				dev.log.Infof("switching to new ip: %s", dev.nextIP)
				dev.ip = dev.nextIP
				dev.nextIP = ""
			}
		} else {
			// Receive updates and send requests.
			dev.recv(ctx)

			// Close teh websocket.
			dev.disconnect()
		}

		// Check if we're done.
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Backoff for a short while before the next connection attempt.
		dev.backoff(ctx)
	}
}

func (dev *ShellyComms) connect() (err error) {
	var conn *websocket.Conn
	conn, _, err = websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/rpc", dev.ip), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// Close the conn on error.
	defer func() {
		if err != nil {
			conn.Close()
			conn = nil
		}
	}()

	dev.log.Debugf("created websocket")

	err = conn.WriteJSON(RequestFrame{
		ID:     "120",
		Src:    "woodhouse-4",
		Method: "Shelly.GetConfig",
	})
	// err = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":300, "src":"woodhouse", "method":"Shelly.GetConfig"}`))
	if err != nil {
		return fmt.Errorf("write get config: %w", err)
	}

	_, message, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read get config: %w", err)
	}
	// dev.log.Debugf("--> recv: %s", message)

	var frame ResponseFrame
	err = json.Unmarshal(message, &frame)
	if err != nil {
		return fmt.Errorf("unmarshal config frame: %w", err)
	}
	// dev.log.Debugf("config frame: %+v", frame)

	var configResponse GetConfigResponse
	if frame.Result != nil {
		err = json.Unmarshal(frame.Result, &configResponse)
		if err != nil {
			return fmt.Errorf("unmarshal config: %w", err)
		}
		dev.log.Debugf("config: %+v", configResponse)
	}

	err = conn.WriteJSON(RequestFrame{
		ID:     "121",
		Src:    "woodhouse-4",
		Method: "Shelly.GetStatus",
	})
	// err = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":301, "src":"woodhouse", "method":"Shelly.GetStatus"}`))
	if err != nil {
		return fmt.Errorf("write get status: %w", err)
	}

	_, message, err = conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read get status: %w", err)
	}
	// dev.log.Debugf("--> recv: %s", message)

	err = json.Unmarshal(message, &frame)
	if err != nil {
		return fmt.Errorf("unmarshal status frame: %w", err)
	}
	// dev.log.Debugf("status frame: %+v", frame)

	var statusResponse GetStatusResponse
	if frame.Result != nil {
		err = json.Unmarshal(frame.Result, &statusResponse)
		if err != nil {
			return fmt.Errorf("unmarshal status: %w", err)
		}
		dev.log.Debugf("status: %+v", statusResponse)
	}

	dev.connMu.Lock()
	dev.conn = conn
	dev.connMu.Unlock()

	dev.logDeviceInfo(configResponse, statusResponse)

	// Let the device build its endpoints and services from the config and fill
	// in the state. This must happen before the device is added to the client
	// so that the full state sent on adding contains everything.
	if dev.onConnected != nil {
		dev.onConnected(configResponse, statusResponse)
	}

	// Add the device to the client once we've fully connected to it.
	if !dev.added {
		dev.added = true
		dev.bridge.AddDevice(dev.dev)
	}

	return nil
}

func (dev *ShellyComms) disconnect() {
	dev.connMu.Lock()
	defer dev.connMu.Unlock()
	if dev.conn != nil {
		dev.log.Infof("closing websocket")
		if dev.onDisconnected != nil {
			dev.onDisconnected()
		}
		dev.conn.Close()
		dev.conn = nil
	}
}

func (dev *ShellyComms) recv(ctx context.Context) {
	dev.log.Debugf("recv started")
	defer dev.log.Debugf("recv finished")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, message, err := dev.conn.ReadMessage()
		if err != nil {
			dev.log.Errorf("read: %s", err)
			return
		}
		// dev.log.Infof("--> recv: %s", message)

		switch DetectFrameType(message) {
		default:
			dev.log.Warnf("unknown frame type for message: %s", message)

		case ResponseFrameType:
			var frame ResponseFrame
			err = json.Unmarshal(message, &frame)
			if err != nil {
				dev.log.Warnf("unmarshal: %s, from message: %s", err, message)
			} else {
				dev.log.Debugf("--> recv response frame: %+v", frame)
				if dev.onResponseFrame != nil {
					dev.onResponseFrame(frame)
				}
			}

		case NotificationFrameType:
			var frame NotificationFrame
			err = json.Unmarshal(message, &frame)
			if err != nil {
				dev.log.Warnf("unmarshal: %s, from message: %s", err, message)
			} else {
				dev.log.Debugf("--> recv notification frame: %+v", frame)
				if frame.NotifyStatus != nil || frame.NotifyEvent != nil {
					if dev.onNotificationFrame != nil {
						dev.onNotificationFrame(frame)
					}
				}
			}
		}
	}
}

func (dev *ShellyComms) backoff(ctx context.Context) {
	// Reset the backoff duration if the backoff has not been used for a
	// suitable amount of time.
	dt := time.Since(dev.lastRestart)
	if dt > dev.backoffDuration {
		dev.log.Infof("backoff reset after %s", dt)
		dev.backoffDuration = minBackoff
	}
	dev.lastBackoff = time.Now()
	dev.log.Infof("starting backoff for %s", dev.backoffDuration)
	timer := time.NewTimer(dev.backoffDuration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	dev.log.Infof("backoff finished")
	dev.backoffDuration = dev.backoffDuration * 2
	if dev.backoffDuration > maxBackoff {
		dev.backoffDuration = maxBackoff
	}
	dev.lastRestart = time.Now()
}

func (dev *ShellyComms) logDeviceInfo(config GetConfigResponse, status GetStatusResponse) {
	name := config.System.Device.Name
	inputs := make(map[int]*InputValue)
	scripts := make(map[int]*ScriptValue)
	switches := make(map[int]*SwitchValue)

	// Get the inputs/scripts/switches from the config.
	for _, val := range config.Inputs {
		inputs[val.ID] = &InputValue{
			ID:     val.ID,
			Name:   val.Name,
			Type:   val.Type,
			Invert: val.Invert,
		}
	}
	for _, val := range config.Scripts {
		scripts[val.ID] = &ScriptValue{
			ID:     val.ID,
			Name:   val.Name,
			Enable: val.Enable,
		}
	}
	for _, val := range config.Switches {
		switches[val.ID] = &SwitchValue{
			ID:   val.ID,
			Name: val.Name,
		}
	}

	// Get values from the status.
	for _, val := range status.Inputs {
		if prev, found := inputs[val.ID]; found {
			prev.Timestamp = time.Now()
			if val.State != nil {
				prev.State = *val.State
			}
		}
	}
	for _, val := range status.Scripts {
		if prev, found := scripts[val.ID]; found {
			prev.Timestamp = time.Now()
			prev.Running = val.Running
		}
	}
	for _, val := range status.Switches {
		if prev, found := switches[val.ID]; found {
			prev.Timestamp = time.Now()
			if val.Output != nil {
				prev.State = *val.Output
			}
			if val.AveragePower != nil {
				prev.AveragePower = *val.AveragePower
			}
			if val.Voltage != nil {
				prev.Voltage = *val.Voltage
			}
			if val.Current != nil {
				prev.Current = *val.Current
			}
			if val.AverageEnergy != nil {
				prev.AverageEnergy.Total = val.AverageEnergy.Total
				prev.AverageEnergy.ByMinute = val.AverageEnergy.ByMinute
				prev.AverageEnergy.MinuteTimestamp = val.AverageEnergy.MinuteTimestamp
			}
			if val.Temperature != nil && val.Temperature.Centigrade != nil {
				prev.Temperature = *val.Temperature.Centigrade
			}
		}
	}

	dev.log.Infof("device name: %s", name)

	var ids []int
	for _, val := range inputs {
		ids = append(ids, val.ID)
	}
	sort.Ints(ids)
	for _, id := range ids {
		val := inputs[id]
		dev.log.Infof("input id: %d, name: %q, type: %s, invert: %t, ts: %s, state: %t", id, val.Name, val.Type, val.Invert, val.Timestamp.Format("2006/01/02 15:04:05"), val.State)
	}

	ids = nil
	for _, val := range scripts {
		ids = append(ids, val.ID)
	}
	sort.Ints(ids)
	for _, id := range ids {
		val := scripts[id]
		dev.log.Infof("script id: %d, name: %q, ts: %s, enable: %t, running %t", id, val.Name, val.Timestamp.Format("2006/01/02 15:04:05"), val.Enable, val.Running)
	}

	ids = nil
	for _, val := range switches {
		ids = append(ids, val.ID)
	}
	sort.Ints(ids)
	for _, id := range ids {
		val := switches[id]
		dev.log.Infof("switch id: %d, name: %q, ts: %s, state: %t, temp: %0.1f C, voltage: %.1f V, current: %.1f A, avg.power: %.1f W, avg.energy.total: %.3f Wh", id, val.Name, val.Timestamp.Format("2006/01/02 15:04:05"), val.State, val.Temperature, val.Voltage, val.Current, val.AveragePower, val.AverageEnergy.Total)
	}
}
