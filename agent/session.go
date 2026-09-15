package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Session is one viewer's WebRTC connection: a screen video track out,
// an "input" data channel in.
type Session struct {
	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticRTP
	input   *Input
	cfg     config
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	started atomic.Bool
	stopped chan struct{} // closed when stream returns
}

func newSession(api *webrtc.API, iceServers []webrtc.ICEServer, input *Input, cfg config, send func(message), offer webrtc.SessionDescription) (*Session, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{pc: pc, input: input, cfg: cfg, ctx: ctx, cancel: cancel, stopped: make(chan struct{})}

	s.track, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "screen", "webdesk")
	if err != nil {
		s.Close()
		return nil, err
	}
	sender, err := pc.AddTrack(s.track)
	if err != nil {
		s.Close()
		return nil, err
	}
	go drainRTCP(sender)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidate := c.ToJSON()
		send(message{Type: "candidate", Candidate: &candidate})
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Info("peer connection", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			if s.started.CompareAndSwap(false, true) {
				go s.stream()
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			go s.Close()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "input" {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { input.Handle(msg.Data) })
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		s.Close()
		return nil, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.Close()
		return nil, err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		s.Close()
		return nil, err
	}
	send(message{Type: "answer", SDP: pc.LocalDescription()})
	return s, nil
}

func (s *Session) AddCandidate(candidate webrtc.ICECandidateInit) {
	if err := s.pc.AddICECandidate(candidate); err != nil {
		slog.Warn("bad ICE candidate", "err", err)
	}
}

// stream runs the screen capture for as long as the session lives,
// restarting ffmpeg if it dies.
func (s *Session) stream() {
	defer close(s.stopped)
	for s.ctx.Err() == nil {
		width, height, err := s.input.ScreenSize()
		if err == nil {
			err = capture(s.ctx, s.cfg.display, width, height, s.cfg.fps, s.track)
		}
		if s.ctx.Err() != nil {
			return
		}
		slog.Warn("capture stopped, restarting", "err", err)
		select {
		case <-s.ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (s *Session) Close() {
	s.once.Do(func() {
		s.cancel()
		// Wait for ffmpeg to be reaped; the agent may exit right after Close.
		if s.started.Load() {
			select {
			case <-s.stopped:
			case <-time.After(3 * time.Second):
				slog.Warn("capture did not stop in time")
			}
		}
		s.input.ReleaseAll()
		if err := s.pc.Close(); err != nil {
			slog.Warn("closing peer connection", "err", err)
		}
		slog.Info("session closed")
	})
}

// drainRTCP reads incoming RTCP so the interceptors (NACK, reports) keep working.
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}
