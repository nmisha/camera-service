package onvif

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"camera-service/internal/inf"
)

// defaultInfo mirrors the JSON returned by "get DefaultInfo" (command 100),
// see camera-analysis/settings-snapshot/get-DefaultInfo.json and
// CAMERA-PROTOCOL.md section 7. Only fields this service reads or writes
// are declared; unknown fields are preserved via json.RawMessage so a
// round-trip never drops data we don't understand.
type defaultInfo map[string]json.RawMessage

type adcSection struct {
	DayNightCtrlType int `json:"day_night_ctrl_type"`
	Dton             int `json:"dton"`
	Ntod             int `json:"ntod"`
	Polarity         int `json:"polarity"`
}

const settingsAttempts = 3

func (s *Server) getDefaultInfo() (defaultInfo, error) {
	var raw []byte
	err := inf.WithConn(s.CameraHost, s.Credentials, 5*time.Second, settingsAttempts, func(conn *inf.Conn) error {
		r, err := conn.SettingsGet("DefaultInfo")
		if err != nil {
			return err
		}
		raw = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The response body is padded with trailing NULs to a fixed size; trim
	// them so json.Unmarshal doesn't choke on trailing garbage.
	if i := strings.IndexByte(string(raw), 0); i >= 0 {
		raw = raw[:i]
	}
	var di defaultInfo
	if err := json.Unmarshal(raw, &di); err != nil {
		return nil, fmt.Errorf("imaging: parse DefaultInfo: %w", err)
	}
	return di, nil
}

// setDefaultInfo writes back the full document (never a partial/template
// one, per CAMERA-PROTOCOL.md's explicit warning).
func (s *Server) setDefaultInfo(di defaultInfo) error {
	body, err := json.Marshal(di)
	if err != nil {
		return err
	}
	return inf.WithConn(s.CameraHost, s.Credentials, 5*time.Second, settingsAttempts, func(conn *inf.Conn) error {
		return conn.SettingsSet("DefaultInfo", body)
	})
}

// ircutFromAdc always reflects the camera's current *actual* state via
// adc.polarity, which is confirmed against real hardware: polarity=1 gives
// color video, polarity=0 forces monochrome/IR mode, even in bright
// daylight (tested 05.09.2026), with a several-second mechanical delay
// before the switch is visible. Whether that state is auto-selected or
// manually forced is a separate question -- see day_night_ctrl_type in the
// Extension element, NOT folded into this enum, so a GET taken while in
// AUTO mode still reports what the picture actually looks like right now
// rather than masking it behind the literal string "AUTO".
func ircutFromAdc(adc adcSection) string {
	if adc.Polarity == 1 {
		return "ON"
	}
	return "OFF"
}

func (s *Server) handleImaging(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	switch {
	case strings.Contains(body, "GetImagingSettings"):
		di, err := s.getDefaultInfo()
		if err != nil {
			log.Printf("imaging get: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var adc adcSection
		if raw, ok := di["adc"]; ok {
			json.Unmarshal(raw, &adc)
		}
		soapReply(w, fmt.Sprintf(`<timg:GetImagingSettingsResponse xmlns:timg="http://www.onvif.org/ver20/imaging/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
  <timg:ImagingSettings>
    <tt:IrCutFilter>%s</tt:IrCutFilter>
    <tt:Extension><tt:DayNightCtrlType>%d</tt:DayNightCtrlType></tt:Extension>
  </timg:ImagingSettings>
</timg:GetImagingSettingsResponse>`, ircutFromAdc(adc), adc.DayNightCtrlType))
	case strings.Contains(body, "SetImagingSettings"):
		// AUTO/ON/OFF are all confirmed against real hardware (see
		// ircutFromAdc and CAMERA-PROTOCOL.md section 7). Anything else is
		// refused rather than guessing an untested value. Note: AUTO only
		// touches day_night_ctrl_type and deliberately leaves polarity
		// alone, so a previously-forced ON/OFF may be overridden later by
		// the camera's own auto day/night logic once AUTO is selected.
		requested := extractTag(body, "IrCutFilter")
		if requested != "" && requested != "AUTO" && requested != "ON" && requested != "OFF" {
			http.Error(w, "IrCutFilter must be AUTO, ON, or OFF", http.StatusNotImplemented)
			return
		}
		di, err := s.getDefaultInfo()
		if err != nil {
			log.Printf("imaging set (read-before-write): %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if requested == "AUTO" || requested == "ON" || requested == "OFF" {
			var adc adcSection
			if raw, ok := di["adc"]; ok {
				json.Unmarshal(raw, &adc)
			}
			switch requested {
			case "AUTO":
				adc.DayNightCtrlType = 0
			case "ON":
				adc.Polarity = 1
			case "OFF":
				adc.Polarity = 0
			}
			patched, _ := json.Marshal(adc)
			di["adc"] = patched
		}
		if err := s.setDefaultInfo(di); err != nil {
			log.Printf("imaging set: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		soapReply(w, `<timg:SetImagingSettingsResponse xmlns:timg="http://www.onvif.org/ver20/imaging/wsdl"/>`)
	default:
		http.Error(w, "unsupported action", http.StatusNotImplemented)
	}
}
