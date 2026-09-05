// Package stream keeps the camera's own INF connection alive for one video
// stream (main or sub) and republishes decoded frames to any number of RTSP
// readers.
package stream

import (
	"context"
	"log"
	"sync"
	"time"

	"camera-service/internal/inf"
)

// State holds the latest frame for one stream (main or sub) and lets
// multiple RTSP client goroutines wait for the next one without each
// holding their own INF subscription (the camera's own INF video sender
// closes the session if the peer is local to the device itself, so this
// service always dials out over the real network -- see CAMERA-PROTOCOL.md
// and the project README for why).
type State struct {
	Name        string
	StreamID    byte
	InfHost     string
	Credentials inf.Credentials

	mu         sync.Mutex
	generation uint64
	waitCh     chan struct{}
	nals       [][]byte
	timestamp  uint32
	width      uint16
	height     uint16

	paramsMu  sync.Mutex
	paramsCh  chan struct{}
	paramsSet bool
	sps, pps  []byte
}

func New(name string, streamID byte, infHost string, creds inf.Credentials) *State {
	return &State{
		Name:        name,
		StreamID:    streamID,
		InfHost:     infHost,
		Credentials: creds,
		waitCh:      make(chan struct{}),
		paramsCh:    make(chan struct{}),
	}
}

// Run dials the camera and republishes frames until ctx is cancelled,
// reconnecting on any error.
func (s *State) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.runOnce(ctx); err != nil {
			log.Printf("%s: %v, reconnecting", s.Name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *State) runOnce(ctx context.Context) error {
	conn, err := inf.Dial(s.InfHost, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	if err := conn.Login(s.Credentials); err != nil {
		return err
	}
	if err := conn.Subscribe(s.StreamID); err != nil {
		return err
	}
	log.Printf("%s: subscribed, waiting for frames", s.Name)

	for {
		cmd, payload, err := conn.ReadPacket()
		if err != nil {
			return err
		}
		if cmd != 6 {
			continue
		}
		frame, err := inf.ParseFrame(payload)
		if err != nil {
			continue
		}
		nals := splitAnnexB(frame.Media)
		if len(nals) == 0 {
			continue
		}
		if frame.Marker == 0xD1 {
			s.captureParams(nals)
		}
		s.publish(nals, frame.TimestampMS, frame.Width, frame.Height)
	}
}

func (s *State) captureParams(nals [][]byte) {
	var sps, pps []byte
	for _, n := range nals {
		switch n[0] & 0x1F {
		case 7:
			sps = append([]byte(nil), n...)
		case 8:
			pps = append([]byte(nil), n...)
		}
	}
	if sps == nil || pps == nil {
		return
	}
	s.paramsMu.Lock()
	defer s.paramsMu.Unlock()
	s.sps, s.pps = sps, pps
	if !s.paramsSet {
		s.paramsSet = true
		close(s.paramsCh)
	}
}

// Params blocks until SPS/PPS have been seen at least once, or ctx is done.
func (s *State) Params(ctx context.Context) (sps, pps []byte, err error) {
	s.paramsMu.Lock()
	ready := s.paramsSet
	ch := s.paramsCh
	s.paramsMu.Unlock()
	if !ready {
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	s.paramsMu.Lock()
	defer s.paramsMu.Unlock()
	return s.sps, s.pps, nil
}

func (s *State) publish(nals [][]byte, ts uint32, w, h uint16) {
	s.mu.Lock()
	s.nals = nals
	s.timestamp = ts
	s.width, s.height = w, h
	s.generation++
	ch := s.waitCh
	s.waitCh = make(chan struct{})
	s.mu.Unlock()
	close(ch)
}

// Wait blocks until a frame newer than lastGen is published, then returns it.
func (s *State) Wait(ctx context.Context, lastGen uint64) (nals [][]byte, ts uint32, gen uint64, err error) {
	s.mu.Lock()
	if s.generation != lastGen {
		nals, ts, gen = s.nals, s.timestamp, s.generation
		s.mu.Unlock()
		return
	}
	ch := s.waitCh
	s.mu.Unlock()

	select {
	case <-ch:
	case <-ctx.Done():
		return nil, 0, lastGen, ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nals, s.timestamp, s.generation, nil
}

func (s *State) Size() (width, height uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.width, s.height
}

// splitAnnexB splits an Annex B byte stream (0x000001 / 0x00000001 start
// codes) into individual NAL units, trimming trailing zero padding.
func splitAnnexB(buf []byte) [][]byte {
	var out [][]byte
	i := 0
	for i+3 <= len(buf) {
		sc := 0
		if i+4 <= len(buf) && buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 0 && buf[i+3] == 1 {
			sc = 4
		} else if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
			sc = 3
		}
		if sc == 0 {
			i++
			continue
		}
		start := i + sc
		j := start
		for j+3 <= len(buf) {
			if buf[j] == 0 && buf[j+1] == 0 && (buf[j+2] == 1 || (j+4 <= len(buf) && buf[j+2] == 0 && buf[j+3] == 1)) {
				break
			}
			j++
		}
		end := len(buf)
		if j+3 <= len(buf) {
			end = j
		}
		for end > start && buf[end-1] == 0 {
			end--
		}
		if end > start {
			out = append(out, buf[start:end])
		}
		i = end
	}
	return out
}
