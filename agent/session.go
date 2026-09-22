package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// How often a viewer's plea for a keyframe may restart the encoder.
const keyframeInterval = 2 * time.Second

// Session is one viewer's WebRTC connection: the screen as a video track out,
// and an "input" data channel carrying mouse and keyboard in, cursor shapes out.
type Session struct {
	pc       *webrtc.PeerConnection
	track    *webrtc.TrackLocalStaticRTP
	input    *Input
	cfg      config
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	started  atomic.Bool
	stopped  chan struct{} // closed when stream returns
	keyframe chan struct{} // the viewer is missing a keyframe

	mu           sync.Mutex
	cursor       *xgb.Conn
	clipboard    *Clipboard
	closed       bool
	lastKeyframe time.Time
}

func newSession(api *webrtc.API, iceServers []webrtc.ICEServer, input *Input, cfg config, send func(message), offer webrtc.SessionDescription) (*Session, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		pc:       pc,
		input:    input,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		stopped:  make(chan struct{}),
		keyframe: make(chan struct{}, 1),
	}

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
	go s.readRTCP(sender)

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
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { s.handle(msg.Data) })
		dc.OnOpen(func() {
			s.startCursor(dc)
			s.startClipboard(dc)
		})
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

// startCursor sends cursor shapes to the viewer, which draws them at the local
// pointer. A missing XFIXES extension only costs us the cursor.
func (s *Session) startCursor(dc *webrtc.DataChannel) {
	conn, err := watchCursor(s.cfg.display, func(payload string) {
		if err := dc.SendText(payload); err != nil {
			slog.Debug("cursor send failed", "err", err)
		}
	})
	if err != nil {
		slog.Warn("cursor updates unavailable", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		conn.Close()
		return
	}
	s.cursor = conn
}

// handle routes a message from the viewer: clipboard text, or an input event.
func (s *Session) handle(data []byte) {
	var msg struct {
		T    string `json:"t"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	if msg.T != "clipboard" {
		s.input.Handle(data)
		return
	}
	s.mu.Lock()
	clipboard := s.clipboard
	s.mu.Unlock()
	if clipboard != nil {
		clipboard.SetFromViewer(msg.Text)
	}
}

// startClipboard keeps the machine's clipboard and the viewer's in step.
// Without XFIXES we simply go without it.
func (s *Session) startClipboard(dc *webrtc.DataChannel) {
	clipboard, err := NewClipboard(s.cfg.display, func(payload string) {
		if err := dc.SendText(payload); err != nil {
			slog.Debug("clipboard send failed", "err", err)
		}
	})
	if err != nil {
		slog.Warn("clipboard sharing unavailable", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		clipboard.Close()
		return
	}
	s.clipboard = clipboard
}

// readRTCP watches the viewer's reports, which keeps the interceptors working
// and tells us when it has lost the picture and needs a new keyframe.
func (s *Session) readRTCP(sender *webrtc.RTPSender) {
	for {
		packets, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, packet := range packets {
			switch packet.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.requestKeyframe()
			}
		}
	}
}

// requestKeyframe restarts the encoder, the only way to get an immediate
// keyframe out of ffmpeg. Rate-limited, because viewers ask repeatedly while
// they wait for one.
func (s *Session) requestKeyframe() {
	s.mu.Lock()
	if time.Since(s.lastKeyframe) < keyframeInterval {
		s.mu.Unlock()
		return
	}
	s.lastKeyframe = time.Now()
	s.mu.Unlock()

	select {
	case s.keyframe <- struct{}{}:
	default: // one is already pending
	}
}

// stream runs the screen capture for as long as the session lives,
// restarting ffmpeg if it dies or a keyframe is needed.
func (s *Session) stream() {
	defer close(s.stopped)
	for s.ctx.Err() == nil {
		width, height, err := s.input.ScreenSize()
		if err == nil {
			err = capture(s.ctx, s.cfg.display, width, height, s.cfg.fps, s.track, s.keyframe)
		}
		if s.ctx.Err() != nil {
			return
		}
		if errors.Is(err, errKeyframeRestart) {
			continue // straight back up: the viewer is waiting for the picture
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
		s.mu.Lock()
		s.closed = true
		cursor := s.cursor
		clipboard := s.clipboard
		s.cursor, s.clipboard = nil, nil
		s.mu.Unlock()
		if cursor != nil {
			cursor.Close()
		}
		if clipboard != nil {
			clipboard.Close()
		}

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
