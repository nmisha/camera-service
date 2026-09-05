// Package onvif implements a minimal ONVIF front end: WS-Discovery, and the
// subset of Device/Media/PTZ SOAP actions needed for a client to discover
// the camera, fetch its RTSP URL, and drive PTZ. No WS-Security -- this is
// meant for a trusted LAN, matching the rest of this project's stance.
package onvif

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"camera-service/internal/inf"
)

type StreamInfo struct {
	Name   string
	Width  int
	Height int
}

type Server struct {
	DeviceName  string // advertised in GetDeviceInformation
	ServiceHost string // IP/hostname this service is reachable at (advertised in XAddrs and stream URIs)
	HTTPPort    int
	RTSPPort    int
	Streams     []StreamInfo

	CameraHost  string // camera's address (IP or hostname), for sending PTZ/settings commands over INF
	Credentials inf.Credentials
}

// ---------- WS-Discovery ----------

const (
	discoveryAddr = "239.255.255.250:3702"
)

func (s *Server) ListenAndServeDiscovery(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp4", discoveryAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	log.Printf("onvif ws-discovery listening on %s", discoveryAddr)

	buf := make([]byte, 8192)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		msg := string(buf[:n])
		if !strings.Contains(msg, "Probe") {
			continue
		}
		msgID := extractTag(msg, "MessageID")
		reply := s.probeMatch(msgID)
		uconn, err := net.DialUDP("udp4", nil, from)
		if err != nil {
			continue
		}
		uconn.Write([]byte(reply))
		uconn.Close()
	}
}

func (s *Server) deviceServiceURL() string {
	return fmt.Sprintf("http://%s:%d/onvif/device_service", s.ServiceHost, s.HTTPPort)
}

func (s *Server) probeMatch(relatesTo string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"
  xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing"
  xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
  <soap:Header>
    <wsa:MessageID>uuid:%s</wsa:MessageID>
    <wsa:RelatesTo>%s</wsa:RelatesTo>
    <wsa:To>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</wsa:To>
    <wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches</wsa:Action>
  </soap:Header>
  <soap:Body>
    <d:ProbeMatches>
      <d:ProbeMatch>
        <wsa:EndpointReference><wsa:Address>uuid:%s</wsa:Address></wsa:EndpointReference>
        <d:Types>dn:NetworkVideoTransmitter</d:Types>
        <d:XAddrs>%s</d:XAddrs>
        <d:MetadataVersion>1</d:MetadataVersion>
      </d:ProbeMatch>
    </d:ProbeMatches>
  </soap:Body>
</soap:Envelope>`, newUUID(), relatesTo, newUUID(), s.deviceServiceURL())
}

// extractTag returns the text content of the first <tag> or <ns:tag>
// element found (namespace prefix optional, since real SOAP clients almost
// always send one, e.g. <tt:IrCutFilter> or <wsa:MessageID>).
func extractTag(xml, tag string) string {
	re := regexp.MustCompile(`<(?:\w+:)?` + regexp.QuoteMeta(tag) + `(?:\s[^>]*)?>([^<]*)<`)
	m := re.FindStringSubmatch(xml)
	if m == nil {
		return ""
	}
	return m[1]
}

func newUUID() string {
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), time.Now().Unix())
}

// ---------- SOAP HTTP services ----------

func (s *Server) ListenAndServeHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/onvif/device_service", s.handleDevice)
	mux.HandleFunc("/onvif/media_service", s.handleMedia)
	mux.HandleFunc("/onvif/ptz_service", s.handlePTZ)
	mux.HandleFunc("/onvif/imaging_service", s.handleImaging)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", s.HTTPPort), Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("onvif soap listening on :%d", s.HTTPPort)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func soapReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope">
  <soap:Body>%s</soap:Body>
</soap:Envelope>`, body)
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	switch {
	case strings.Contains(body, "GetDeviceInformation"):
		// Real values decoded from the camera's own DeviceInfo where
		// available (see CAMERA-PROTOCOL.md section 8.1); fall back to the
		// configured DeviceName if the camera can't be reached right now.
		model, firmware, serial := s.DeviceName, "unknown", s.DeviceName
		if di, err := s.getDeviceInfo(); err != nil {
			log.Printf("device info: %v (using fallback values)", err)
		} else {
			if di.Model != "" {
				model = di.Model
			}
			if di.Firmware != "" {
				firmware = di.Firmware
			}
			if di.SerialID != "" {
				serial = di.SerialID
			}
		}
		soapReply(w, fmt.Sprintf(`<tds:GetDeviceInformationResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
  <tds:Manufacturer>Hankvision</tds:Manufacturer>
  <tds:Model>%s</tds:Model>
  <tds:FirmwareVersion>%s</tds:FirmwareVersion>
  <tds:SerialNumber>%s</tds:SerialNumber>
  <tds:HardwareId>1</tds:HardwareId>
</tds:GetDeviceInformationResponse>`, model, firmware, serial))
	case strings.Contains(body, "GetCapabilities"):
		soapReply(w, fmt.Sprintf(`<tds:GetCapabilitiesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
  <tds:Capabilities>
    <tt:Device><tt:XAddr>http://%s:%d/onvif/device_service</tt:XAddr></tt:Device>
    <tt:Media><tt:XAddr>http://%s:%d/onvif/media_service</tt:XAddr></tt:Media>
    <tt:PTZ><tt:XAddr>http://%s:%d/onvif/ptz_service</tt:XAddr></tt:PTZ>
    <tt:Imaging><tt:XAddr>http://%s:%d/onvif/imaging_service</tt:XAddr></tt:Imaging>
  </tds:Capabilities>
</tds:GetCapabilitiesResponse>`, s.ServiceHost, s.HTTPPort, s.ServiceHost, s.HTTPPort, s.ServiceHost, s.HTTPPort, s.ServiceHost, s.HTTPPort))
	case strings.Contains(body, "GetServices"):
		soapReply(w, fmt.Sprintf(`<tds:GetServicesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
  <tds:Service><tds:Namespace>http://www.onvif.org/ver10/device/wsdl</tds:Namespace><tds:XAddr>http://%s:%d/onvif/device_service</tds:XAddr><tds:Version><tds:Major>2</tds:Major><tds:Minor>5</tds:Minor></tds:Version></tds:Service>
  <tds:Service><tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace><tds:XAddr>http://%s:%d/onvif/media_service</tds:XAddr><tds:Version><tds:Major>2</tds:Major><tds:Minor>5</tds:Minor></tds:Version></tds:Service>
  <tds:Service><tds:Namespace>http://www.onvif.org/ver10/ptz/wsdl</tds:Namespace><tds:XAddr>http://%s:%d/onvif/ptz_service</tds:XAddr><tds:Version><tds:Major>2</tds:Major><tds:Minor>5</tds:Minor></tds:Version></tds:Service>
  <tds:Service><tds:Namespace>http://www.onvif.org/ver20/imaging/wsdl</tds:Namespace><tds:XAddr>http://%s:%d/onvif/imaging_service</tds:XAddr><tds:Version><tds:Major>2</tds:Major><tds:Minor>5</tds:Minor></tds:Version></tds:Service>
</tds:GetServicesResponse>`, s.ServiceHost, s.HTTPPort, s.ServiceHost, s.HTTPPort, s.ServiceHost, s.HTTPPort, s.ServiceHost, s.HTTPPort))
	case strings.Contains(body, "GetSystemDateAndTime"):
		now := time.Now().UTC()
		soapReply(w, fmt.Sprintf(`<tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
  <tds:SystemDateAndTime>
    <tt:DateTimeType>NTP</tt:DateTimeType>
    <tt:UTCDateTime>
      <tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>
      <tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date>
    </tt:UTCDateTime>
  </tds:SystemDateAndTime>
</tds:GetDateAndTimeResponse>`, now.Hour(), now.Minute(), now.Second(), now.Year(), now.Month(), now.Day()))
	default:
		http.Error(w, "unsupported action", http.StatusNotImplemented)
	}
}

func (s *Server) profileToken(i int) string { return fmt.Sprintf("profile_%d", i) }

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	switch {
	case strings.Contains(body, "GetProfiles"):
		var sb strings.Builder
		for i, st := range s.Streams {
			fmt.Fprintf(&sb, `<trt:Profiles token="%s" fixed="true" xmlns:tt="http://www.onvif.org/ver10/schema">
        <tt:Name>%s</tt:Name>
        <tt:VideoEncoderConfiguration token="venc_%d">
          <tt:Name>%s</tt:Name>
          <tt:Encoding>H264</tt:Encoding>
          <tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>
        </tt:VideoEncoderConfiguration>
      </trt:Profiles>`, s.profileToken(i), st.Name, i, st.Name, st.Width, st.Height)
		}
		soapReply(w, fmt.Sprintf(`<trt:GetProfilesResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl">%s</trt:GetProfilesResponse>`, sb.String()))
	case strings.Contains(body, "GetStreamUri"):
		token := extractTag(body, "ProfileToken")
		name := s.Streams[0].Name
		for i, st := range s.Streams {
			if s.profileToken(i) == token {
				name = st.Name
			}
		}
		uri := fmt.Sprintf("rtsp://%s:%d/%s", s.ServiceHost, s.RTSPPort, name)
		soapReply(w, fmt.Sprintf(`<trt:GetStreamUriResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
  <trt:MediaUri><tt:Uri>%s</tt:Uri><tt:InvalidAfterConnect>false</tt:InvalidAfterConnect><tt:InvalidAfterReboot>false</tt:InvalidAfterReboot><tt:Timeout>PT60S</tt:Timeout></trt:MediaUri>
</trt:GetStreamUriResponse>`, uri))
	default:
		http.Error(w, "unsupported action", http.StatusNotImplemented)
	}
}

// ---------- PTZ ----------
//
// Translates ONVIF ContinuousMove/Stop into INF command 15. Stop/Up/Down/
// Left/Right are confirmed against real hardware movement (see
// CAMERA-PROTOCOL.md section 6); diagonals and presets are not.

func (s *Server) sendPTZ(code, p1, p2, p3 byte) error {
	conn, err := inf.Dial(s.CameraHost, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Login(s.Credentials); err != nil {
		return err
	}
	return conn.SendPTZ(code, p1, p2, p3, 0)
}

func speedByte(v float64) byte {
	if v < -1 {
		v = -1
	}
	if v > 1 {
		v = 1
	}
	return byte(v*127 + 128)
}

func (s *Server) handlePTZ(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	switch {
	case strings.Contains(body, "ContinuousMove"):
		x := extractAttr(body, "x")
		y := extractAttr(body, "y")
		var code byte = inf.PTZStop
		switch {
		case y > 0:
			code = inf.PTZUp
		case y < 0:
			code = inf.PTZDown
		case x < 0:
			code = inf.PTZLeft
		case x > 0:
			code = inf.PTZRight
		}
		if err := s.sendPTZ(code, speedByte(x), speedByte(y), 0); err != nil {
			log.Printf("ptz continuousmove: %v", err)
		}
		soapReply(w, `<tptz:ContinuousMoveResponse xmlns:tptz="http://www.onvif.org/ver10/ptz/wsdl"/>`)
	case strings.Contains(body, "Stop"):
		if err := s.sendPTZ(inf.PTZStop, 0, 0, 0); err != nil {
			log.Printf("ptz stop: %v", err)
		}
		soapReply(w, `<tptz:StopResponse xmlns:tptz="http://www.onvif.org/ver10/ptz/wsdl"/>`)
	case strings.Contains(body, "GetConfigurations"):
		soapReply(w, `<tptz:GetConfigurationsResponse xmlns:tptz="http://www.onvif.org/ver10/ptz/wsdl"/>`)
	default:
		http.Error(w, "unsupported action", http.StatusNotImplemented)
	}
}

// extractAttr pulls a float attribute like x="0.5" out of a PTZSpeed/Vector element.
func extractAttr(xml, attr string) float64 {
	needle := attr + `="`
	i := strings.Index(xml, needle)
	if i < 0 {
		return 0
	}
	rest := xml[i+len(needle):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return 0
	}
	v, err := strconv.ParseFloat(rest[:end], 64)
	if err != nil {
		return 0
	}
	return v
}
