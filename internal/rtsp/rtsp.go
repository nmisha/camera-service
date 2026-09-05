// Package rtsp implements just enough of RTSP (over TCP, interleaved) to
// serve a single live H.264 track per mount point. No UDP transport, no
// audio, no seeking -- this mirrors what the camera itself provides.
package rtsp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"camera-service/internal/stream"
)

const (
	rtpPayloadType = 96
	fuChunk        = 16000 // keeps interleaved frames well under the 16-bit length limit
)

type Server struct {
	Port    int
	Streams map[string]*stream.State // keyed by mount name, e.g. "IPW-F2A2D1E1"
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.Port))
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	log.Printf("rtsp listening on :%d", s.Port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) matchStream(url string) *stream.State {
	// Check longer/more specific names first (e.g. "IPW-F2A2D1E1_sub" contains "IPW-F2A2D1E1").
	var best *stream.State
	for name, st := range s.Streams {
		if strings.Contains(url, name) {
			if best == nil || len(name) > len(best.Name) {
				best = st
			}
		}
	}
	return best
}

type request struct {
	Method  string
	URL     string
	CSeq    int
	Session string
}

func readRequest(r *bufio.Reader) (*request, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return nil, fmt.Errorf("rtsp: bad request line %q", line)
	}
	req := &request{Method: parts[0], URL: parts[1]}
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		if v, ok := header(line, "CSeq"); ok {
			req.CSeq, _ = strconv.Atoi(v)
		}
		if v, ok := header(line, "Session"); ok {
			req.Session = strings.SplitN(v, ";", 2)[0]
		}
	}
	return req, nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func header(line, name string) (string, bool) {
	prefix := name + ":"
	if len(line) <= len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(line[len(prefix):]), true
}

var sessionCounter uint32

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	var bound *stream.State
	sessionID := fmt.Sprintf("%08x", atomic.AddUint32(&sessionCounter, 1))
	playing := false

	for !playing {
		req, err := readRequest(r)
		if err != nil {
			return
		}
		if bound == nil {
			bound = s.matchStream(req.URL)
		}

		var resp string
		switch req.Method {
		case "OPTIONS":
			resp = fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %d\r\nPublic: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN\r\n\r\n", req.CSeq)
		case "DESCRIBE":
			if bound == nil {
				resp = fmt.Sprintf("RTSP/1.0 404 Not Found\r\nCSeq: %d\r\n\r\n", req.CSeq)
				break
			}
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			sps, pps, err := bound.Params(pctx)
			cancel()
			if err != nil {
				resp = fmt.Sprintf("RTSP/1.0 503 Service Unavailable\r\nCSeq: %d\r\n\r\n", req.CSeq)
				break
			}
			sdp := buildSDP(bound.Name, s.Port, sps, pps)
			resp = fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %d\r\nContent-Base: rtsp://0.0.0.0:%d/%s/\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
				req.CSeq, s.Port, bound.Name, len(sdp), sdp)
		case "SETUP":
			resp = fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %d\r\nSession: %s\r\nTransport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n\r\n", req.CSeq, sessionID)
		case "PLAY":
			resp = fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %d\r\nSession: %s\r\nRange: npt=0.000-\r\n\r\n", req.CSeq, sessionID)
			playing = true
		case "TEARDOWN":
			resp = fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %d\r\nSession: %s\r\n\r\n", req.CSeq, sessionID)
			conn.Write([]byte(resp))
			return
		default:
			resp = fmt.Sprintf("RTSP/1.0 501 Not Implemented\r\nCSeq: %d\r\n\r\n", req.CSeq)
		}
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
	}

	if bound == nil {
		return
	}
	streamLoop(ctx, conn, bound)
}

func buildSDP(name string, port int, sps, pps []byte) string {
	spsB64 := base64.StdEncoding.EncodeToString(sps)
	ppsB64 := base64.StdEncoding.EncodeToString(pps)
	profile := ""
	if len(sps) >= 4 {
		profile = fmt.Sprintf("%02x%02x%02x", sps[1], sps[2], sps[3])
	}
	return fmt.Sprintf(
		"v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=%s\r\nt=0 0\r\n"+
			"m=video 0 RTP/AVP %d\r\nc=IN IP4 0.0.0.0\r\n"+
			"a=rtpmap:%d H264/90000\r\n"+
			"a=fmtp:%d packetization-mode=1;profile-level-id=%s;sprop-parameter-sets=%s,%s\r\n"+
			"a=control:trackID=0\r\n",
		name, rtpPayloadType, rtpPayloadType, rtpPayloadType, profile, spsB64, ppsB64)
}

func streamLoop(ctx context.Context, conn net.Conn, st *stream.State) {
	var seq uint16
	ssrc := uint32(time.Now().UnixNano())
	var gen uint64
	var originMS uint32
	haveOrigin := false

	for {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		nals, ts, newGen, err := st.Wait(wctx, gen)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// wctx deadline: no new frame in 5s, keep waiting on the same generation.
			continue
		}
		gen = newGen
		if !haveOrigin {
			originMS = ts
			haveOrigin = true
		}
		rtpTS := (ts - originMS) * 90

		for i, nal := range nals {
			last := i == len(nals)-1
			if err := sendNAL(conn, nal, &seq, rtpTS, ssrc, last); err != nil {
				return
			}
		}
	}
}

func sendNAL(conn net.Conn, nal []byte, seq *uint16, ts, ssrc uint32, isLastNAL bool) error {
	if len(nal) <= fuChunk {
		return sendRTP(conn, nal, seq, ts, ssrc, isLastNAL)
	}
	header := nal[0]
	nri := header & 0x60
	nalType := header & 0x1F
	p := nal[1:]
	first := true
	for len(p) > 0 {
		take := len(p)
		if take > fuChunk {
			take = fuChunk
		}
		last := take == len(p)
		chunk := make([]byte, 2+take)
		chunk[0] = nri | 28
		var b byte
		if first {
			b |= 0x80
		}
		if last {
			b |= 0x40
		}
		chunk[1] = b | nalType
		copy(chunk[2:], p[:take])
		if err := sendRTP(conn, chunk, seq, ts, ssrc, last && isLastNAL); err != nil {
			return err
		}
		p = p[take:]
		first = false
	}
	return nil
}

func sendRTP(conn net.Conn, payload []byte, seq *uint16, ts, ssrc uint32, marker bool) error {
	rtp := make([]byte, 12+len(payload))
	rtp[0] = 0x80
	m := byte(0)
	if marker {
		m = 0x80
	}
	rtp[1] = m | rtpPayloadType
	rtp[2] = byte(*seq >> 8)
	rtp[3] = byte(*seq)
	*seq++
	rtp[4] = byte(ts >> 24)
	rtp[5] = byte(ts >> 16)
	rtp[6] = byte(ts >> 8)
	rtp[7] = byte(ts)
	rtp[8] = byte(ssrc >> 24)
	rtp[9] = byte(ssrc >> 16)
	rtp[10] = byte(ssrc >> 8)
	rtp[11] = byte(ssrc)
	copy(rtp[12:], payload)

	total := len(rtp)
	frame := make([]byte, 4+total)
	frame[0] = '$'
	frame[1] = 0
	frame[2] = byte(total >> 8)
	frame[3] = byte(total)
	copy(frame[4:], rtp)
	_, err := conn.Write(frame)
	return err
}
