// Package inf implements the camera's proprietary "INF" TCP/90 protocol:
// login, video/audio subscription and PTZ control. See CAMERA-PROTOCOL.md
// in the repository root for the reverse-engineered wire format.
package inf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const Port = 90

// Stream identifiers for command 3 (subscribe), body[2].
const (
	StreamMain = 4
	StreamSub  = 5
)

// PTZ direction codes for command 15, body[0] (see CAMERA-PROTOCOL.md section 6).
// Stop/Up/Down/Left/Right confirmed against real hardware movement
// (05.09.2026); diagonals and presets are not.
const (
	PTZStop  = 0x00
	PTZUp    = 0x55 // 'U'
	PTZDown  = 0x44 // 'D'
	PTZLeft  = 0x4c // 'L'
	PTZRight = 0x52 // 'R'
)

type Conn struct {
	nc net.Conn
}

func Dial(host string, timeout time.Duration) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(Port)), timeout)
	if err != nil {
		return nil, err
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return &Conn{nc: nc}, nil
}

func (c *Conn) Close() error { return c.nc.Close() }

func (c *Conn) SetDeadline(t time.Time) error { return c.nc.SetDeadline(t) }

// Credentials for command-1 login. The stock/verified path is mode 0
// (User="admin", Password="") which the camera accepts without checking a
// password at all -- see CAMERA-PROTOCOL.md section 3. Set Password to use
// mode 1 (real credential check) instead, e.g. if this camera's login has
// been reconfigured to require it.
type Credentials struct {
	User     string
	Password string
}

// DefaultCredentials matches the factory-default, verified no-password
// login. Override via CAMERA_USER/CAMERA_PASSWORD (see cmd/camd/main.go)
// rather than hardcoding different credentials here.
var DefaultCredentials = Credentials{User: "admin"}

// WithConn dials, logs in, and runs fn on a fresh connection, retrying the
// whole sequence up to attempts times on any error (including login).
//
// The settings channel (used by fn for SettingsGet/SettingsSet) has been
// observed to drop the connection (EOF / connection reset) intermittently,
// with no correlation to which field or operation was involved -- see
// CAMERA-PROTOCOL.md section 7. In practice a retry on a fresh connection
// almost always succeeds within 1-2 attempts. This is safe to retry blindly
// for get/set (the caller always sends back a full, freshly-read document,
// so a duplicate write is a no-op) -- it is NOT used for PTZ, where a
// spurious retry could double a movement command.
func WithConn(host string, creds Credentials, timeout time.Duration, attempts int, fn func(*Conn) error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		conn, err := Dial(host, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		if err := conn.Login(creds); err != nil {
			conn.Close()
			lastErr = err
			continue
		}
		err = fn(conn)
		conn.Close()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func (c *Conn) sendPacket(command byte, payload []byte) error {
	header := make([]byte, 12)
	copy(header, "INF")
	header[3] = command
	binary.BigEndian.PutUint16(header[4:], 1)
	binary.BigEndian.PutUint16(header[6:], 1)
	binary.BigEndian.PutUint32(header[8:], uint32(len(payload)))
	if _, err := c.nc.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.nc.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

const maxPacket = 2 * 1024 * 1024

// ReadPacket reads one INF frame: 12-byte header then the declared body.
func (c *Conn) ReadPacket() (command byte, payload []byte, err error) {
	header := make([]byte, 12)
	if _, err = io.ReadFull(c.nc, header); err != nil {
		return 0, nil, err
	}
	if string(header[0:3]) != "INF" {
		return 0, nil, errors.New("inf: bad magic")
	}
	size := binary.BigEndian.Uint32(header[8:])
	if size > maxPacket {
		return 0, nil, fmt.Errorf("inf: packet too large: %d", size)
	}
	payload = make([]byte, size)
	if size > 0 {
		if _, err = io.ReadFull(c.nc, payload); err != nil {
			return 0, nil, err
		}
	}
	return header[3], payload, nil
}

// Login sends command 1. With creds.Password == "" this is the stock "mode
// 0" login (no password check), matching the behavior of the vendor's own
// client and verified against the real device; a non-empty Password
// switches to mode 1 (real credential check, also verified for admin/admin
// -- see CAMERA-PROTOCOL.md section 3).
func (c *Conn) Login(creds Credentials) error {
	user := creds.User
	if user == "" {
		user = "admin"
	}
	body := make([]byte, 68)
	copy(body, user)
	mode := uint32(0)
	if creds.Password != "" {
		copy(body[32:], creds.Password)
		mode = 1
	}
	binary.BigEndian.PutUint32(body[64:], mode)
	if err := c.sendPacket(1, body); err != nil {
		return err
	}
	cmd, reply, err := c.ReadPacket()
	if err != nil {
		return err
	}
	if cmd != 2 || len(reply) < 6 || reply[4] != 0 || reply[5] != 0 {
		return fmt.Errorf("inf: login rejected (cmd=%d)", cmd)
	}
	return nil
}

// Subscribe requests a video stream: streamID is StreamMain or StreamSub.
func (c *Conn) Subscribe(streamID byte) error {
	body := make([]byte, 24)
	body[1] = 1
	body[2] = streamID
	return c.sendPacket(3, body)
}

// SettingsGet issues command 100 ("get") for the named settings function
// (e.g. "DefaultInfo", "DeviceInfo") and returns the raw JSON/binary payload
// starting at body offset 84, per CAMERA-PROTOCOL.md section 7. Verified
// against the real device for DefaultInfo/DeviceInfo.
func (c *Conn) SettingsGet(name string) ([]byte, error) {
	return c.settingsCall("get", name, nil)
}

// SettingsSet issues command 100 ("set"). data should be a full,
// previously-fetched-and-modified document, never a template or partial
// structure -- the camera's set branches are not fully mapped, and CAMERA-
// PROTOCOL.md explicitly warns against speculative writes. NOT verified
// against the real device as of this writing; the only live test performed
// was a true no-op round trip (unmodified DefaultInfo written back).
func (c *Conn) SettingsSet(name string, data []byte) error {
	_, err := c.settingsCall("set", name, data)
	return err
}

func (c *Conn) settingsCall(op, name string, data []byte) ([]byte, error) {
	body := make([]byte, 84+len(data))
	copy(body, op)
	copy(body[16:], name)
	copy(body[84:], data)
	if err := c.sendPacket(100, body); err != nil {
		return nil, err
	}
	cmd, reply, err := c.ReadPacket()
	if err != nil {
		return nil, err
	}
	if cmd != 101 || len(reply) < 84 {
		return nil, fmt.Errorf("inf: settings %s %s: unexpected reply (cmd=%d, len=%d)", op, name, cmd, len(reply))
	}
	status := reply[80:84]
	if status[0] != 0 || status[1] != 0 || status[2] != 0 || status[3] != 0 {
		return nil, fmt.Errorf("inf: settings %s %s: non-zero status %x", op, name, status)
	}
	return reply[84:], nil
}

// SendPTZ issues command 15. Channel is little-endian per the vendor's own
// handler (documented quirk: this field breaks big-endian consistency).
func (c *Conn) SendPTZ(code, param1, param2, param3 byte, channel int16) error {
	body := make([]byte, 6)
	body[0] = code
	body[1] = param1
	body[2] = param2
	body[3] = param3
	binary.LittleEndian.PutUint16(body[4:], uint16(channel))
	return c.sendPacket(15, body)
}

// Frame is one decoded command-6 video/JPEG payload.
type Frame struct {
	Marker      byte // 0xD1 = H.264 IDR w/ SPS+PPS, 0xD2 = subsequent, 0x00 = JPEG
	Type        byte
	SubType     byte
	ClaimedFPS  byte
	Width       uint16
	Height      uint16
	FrameNumber uint32
	TimestampMS uint32
	Media       []byte
}

// ParseFrame decodes a command-6 payload (24-byte header + media).
func ParseFrame(payload []byte) (*Frame, error) {
	if len(payload) < 24 {
		return nil, errors.New("inf: truncated frame header")
	}
	f := &Frame{
		Marker:      payload[0],
		Type:        payload[1],
		SubType:     payload[2],
		ClaimedFPS:  payload[3],
		Width:       binary.BigEndian.Uint16(payload[4:]),
		Height:      binary.BigEndian.Uint16(payload[6:]),
		FrameNumber: binary.BigEndian.Uint32(payload[8:]),
		TimestampMS: binary.BigEndian.Uint32(payload[16:]),
		Media:       payload[24:],
	}
	frameSize := binary.BigEndian.Uint32(payload[12:])
	if int(frameSize) != len(f.Media) {
		return nil, fmt.Errorf("inf: frame size mismatch (%d != %d)", frameSize, len(f.Media))
	}
	return f, nil
}
