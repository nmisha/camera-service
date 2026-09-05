// camd bridges the camera's proprietary INF protocol (TCP/90) to RTSP and
// ONVIF, so any ONVIF-aware client or NVR can find, view, and (PTZ) control
// the camera without needing the vendor app or cloud.
//
// The camera itself refuses to stream video to a client running on the same
// host (confirmed by tracing hiapp; see CAMERA-PROTOCOL.md), so this service
// cannot run on the camera -- it dials out over the real network, same as
// any other viewer. It's meant to run as a small always-on container on the
// LAN instead.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"camera-service/internal/inf"
	"camera-service/internal/onvif"
	"camera-service/internal/rtsp"
	"camera-service/internal/stream"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	cameraHost := env("CAMERA_HOST", "10.1.0.21")
	// CAMERA_USER/CAMERA_PASSWORD: the verified stock login is mode 0
	// (user "admin", no password check at all -- CAMERA-PROTOCOL.md section
	// 3), so CAMERA_PASSWORD defaults to empty. Set it only if this
	// camera's login has been reconfigured to require real credentials
	// (mode 1, also verified to work for admin/admin).
	credentials := inf.Credentials{
		User:     env("CAMERA_USER", "admin"),
		Password: env("CAMERA_PASSWORD", ""),
	}
	serviceHost := env("SERVICE_HOST", "") // must be reachable by clients; no safe default
	rtspPort := envInt("RTSP_PORT", 554)
	onvifPort := envInt("ONVIF_PORT", 80)
	deviceName := env("DEVICE_NAME", "IPW-F2A2D1E1")
	enableDiscovery := env("WS_DISCOVERY", "true") == "true"

	if serviceHost == "" {
		log.Fatal("SERVICE_HOST must be set to this service's own LAN-reachable address")
	}

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		cancel()
	}()

	main10 := stream.New("camera10", inf.StreamMain, cameraHost, credentials)
	sub10 := stream.New("camera10_sub", inf.StreamSub, cameraHost, credentials)
	go main10.Run(ctx)
	go sub10.Run(ctx)

	rtspSrv := &rtsp.Server{
		Port: rtspPort,
		Streams: map[string]*stream.State{
			main10.Name: main10,
			sub10.Name:  sub10,
		},
	}

	onvifSrv := &onvif.Server{
		DeviceName:  deviceName,
		ServiceHost: serviceHost,
		HTTPPort:    onvifPort,
		RTSPPort:    rtspPort,
		CameraHost:  cameraHost,
		Credentials: credentials,
		Streams: []onvif.StreamInfo{
			{Name: main10.Name, Width: 1920, Height: 1080},
			{Name: sub10.Name, Width: 640, Height: 360},
		},
	}

	errCh := make(chan error, 3)
	go func() { errCh <- rtspSrv.ListenAndServe(ctx) }()
	go func() { errCh <- onvifSrv.ListenAndServeHTTP(ctx) }()
	if enableDiscovery {
		go func() { errCh <- onvifSrv.ListenAndServeDiscovery(ctx) }()
	}

	for {
		select {
		case err := <-errCh:
			if err != nil {
				log.Printf("service error: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}
