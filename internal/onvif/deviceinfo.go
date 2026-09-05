package onvif

import (
	"bytes"
	"fmt"
	"time"

	"camera-service/internal/inf"
)

// deviceInfoFields is the decoded layout of "get DeviceInfo" (command 100),
// reverse-engineered by static analysis of a saved response -- see
// CAMERA-PROTOCOL.md section 8.1. Offsets are relative to the start of the
// data (body[84:] in the wire protocol, already stripped by SettingsGet).
type deviceInfoFields struct {
	SensorBoard string // offset 0,  32 bytes: e.g. "V6202IR-SC1345"
	Firmware    string // offset 32, 32 bytes: e.g. "V2.1.0.14231_Tuya-220808"
	SerialID    string // offset 64, 32 bytes: e.g. "E71CFAB35702" (purpose unconfirmed)
	MAC         string // offset 128, 6 bytes
	Model       string // offset 134, 12 bytes: e.g. "IPW-F2A2D1E1"
}

func trimNUL(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func parseDeviceInfo(data []byte) deviceInfoFields {
	var f deviceInfoFields
	if len(data) >= 32 {
		f.SensorBoard = trimNUL(data[0:32])
	}
	if len(data) >= 64 {
		f.Firmware = trimNUL(data[32:64])
	}
	if len(data) >= 96 {
		f.SerialID = trimNUL(data[64:96])
	}
	if len(data) >= 146 {
		f.MAC = fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
			data[128], data[129], data[130], data[131], data[132], data[133])
		f.Model = trimNUL(data[134:146])
	}
	return f
}

func (s *Server) getDeviceInfo() (deviceInfoFields, error) {
	var raw []byte
	err := inf.WithConn(s.CameraHost, s.Credentials, 5*time.Second, settingsAttempts, func(conn *inf.Conn) error {
		r, err := conn.SettingsGet("DeviceInfo")
		if err != nil {
			return err
		}
		raw = r
		return nil
	})
	if err != nil {
		return deviceInfoFields{}, err
	}
	return parseDeviceInfo(raw), nil
}
