// ptztest sends one raw INF command-15 PTZ command to the camera, waits a
// short (default 400ms) pulse, then always sends STOP before exiting --
// even on error. This is a hardware-safety tool for manually verifying the
// PTZ wire format from CAMERA-PROTOCOL.md against real motor movement; it
// is not meant to be left running unattended.
package main

import (
	"flag"
	"log"
	"os"
	"time"

	"camera-service/internal/inf"
)

func main() {
	host := flag.String("host", "10.1.0.21", "camera host")
	user := flag.String("user", envOr("CAMERA_USER", "admin"), "camera login user")
	password := flag.String("password", envOr("CAMERA_PASSWORD", ""), "camera login password (empty = mode-0 no-password login)")
	code := flag.String("code", "stop", "stop|up|down|left|right")
	speed := flag.Int("speed", 60, "param1 (speed), 0-255")
	pulse := flag.Duration("pulse", 400*time.Millisecond, "how long to move before auto-STOP")
	flag.Parse()

	var c byte
	switch *code {
	case "stop":
		c = inf.PTZStop
	case "up":
		c = inf.PTZUp
	case "down":
		c = inf.PTZDown
	case "left":
		c = inf.PTZLeft
	case "right":
		c = inf.PTZRight
	default:
		log.Fatalf("unknown code %q", *code)
	}

	conn, err := inf.Dial(*host, 5*time.Second)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.Login(inf.Credentials{User: *user, Password: *password}); err != nil {
		log.Fatalf("login: %v", err)
	}

	log.Printf("sending code=%s speed=%d", *code, *speed)
	if err := conn.SendPTZ(c, byte(*speed), 0, 0, 0); err != nil {
		log.Printf("send failed: %v", err)
	}

	if c != inf.PTZStop {
		time.Sleep(*pulse)
		log.Printf("auto-STOP after %s", *pulse)
		if err := conn.SendPTZ(inf.PTZStop, 0, 0, 0, 0); err != nil {
			log.Printf("stop failed: %v", err)
		}
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
