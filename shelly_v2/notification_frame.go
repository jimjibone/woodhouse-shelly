package shelly_v2

import (
	"encoding/json"
	"fmt"
	"strings"
)

type NotificationFrame struct {
	Src          string                     `json:"src"`
	Dst          string                     `json:"dst"`
	Method       string                     `json:"method"`
	Params       map[string]json.RawMessage `json:"params"`
	NotifyStatus *NotifyStatus              `json:"-"`
	NotifyEvent  *NotifyEvent               `json:"-"`
}

// NotifyEvent carries component events, e.g. the presses of a button input.
type NotifyEvent struct {
	Timestamp float64
	Events    []ComponentEvent
}

// ComponentEvent is one event of a component. For inputs the component is
// "input:<id>" and the event one of btn_down, btn_up, single_push,
// double_push, triple_push and long_push.
type ComponentEvent struct {
	Component string  `json:"component"`
	ID        int     `json:"id"`
	Event     string  `json:"event"`
	Timestamp float64 `json:"ts"`
}

type NotifyStatus struct {
	Timestamp float64
	Inputs    []GetStatusResponseInput
	Scripts   []GetStatusResponseScript
	Switches  []GetStatusResponseSwitch
}

func (frame *NotificationFrame) String() string {
	str := fmt.Sprintf("{Src:%s, Dst:%s, Method:%s", frame.Src, frame.Dst, frame.Method)
	if frame.NotifyStatus != nil {
		str += ", NotifyStatus:" + frame.NotifyStatus.String()
	} else if frame.NotifyEvent != nil {
		str += fmt.Sprintf(", NotifyEvent:%+v", *frame.NotifyEvent)
	} else {
		str += fmt.Sprintf(", Params:%s", frame.Params)
	}
	str += "}"
	return str
}

func (status *NotifyStatus) String() string {
	return fmt.Sprintf("{Timestamp:%f, Inputs:%v, Scripts:%v, Switches:%v}", status.Timestamp, status.Inputs, status.Scripts, status.Switches)
}

func (m *NotificationFrame) UnmarshalJSON(data []byte) error {
	type Tmp NotificationFrame
	tmp := Tmp{}
	err := json.Unmarshal(data, &tmp)
	if err != nil {
		return err
	}

	switch tmp.Method {
	case "NotifyStatus":
		tmp.NotifyStatus = &NotifyStatus{}

		for field, data := range tmp.Params {
			if strings.Contains(field, "ts") {
				var msg float64
				err = json.Unmarshal(data, &msg)
				if err != nil {
					return err
				}
				tmp.NotifyStatus.Timestamp = msg
			} else if strings.Contains(field, "input") {
				msg := GetStatusResponseInput{}
				err = json.Unmarshal(data, &msg)
				if err != nil {
					return err
				}
				tmp.NotifyStatus.Inputs = append(tmp.NotifyStatus.Inputs, msg)
			} else if strings.Contains(field, "switch") {
				msg := GetStatusResponseSwitch{}
				err = json.Unmarshal(data, &msg)
				if err != nil {
					return err
				}
				tmp.NotifyStatus.Switches = append(tmp.NotifyStatus.Switches, msg)
			}
		}

	case "NotifyEvent":
		tmp.NotifyEvent = &NotifyEvent{}

		if data, found := tmp.Params["ts"]; found {
			err = json.Unmarshal(data, &tmp.NotifyEvent.Timestamp)
			if err != nil {
				return err
			}
		}
		if data, found := tmp.Params["events"]; found {
			err = json.Unmarshal(data, &tmp.NotifyEvent.Events)
			if err != nil {
				return err
			}
		}

	default:
		return fmt.Errorf("unknown notification method %q", tmp.Method)
	}

	*m = NotificationFrame(tmp)

	return nil
}
