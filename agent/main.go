// webdesk agent: shares this machine's X11 screen with a browser over WebRTC
// and replays the viewer's mouse and keyboard input.
//
// The webdesk server starts it over SSH as the logged-in user and talks to it
// on stdin/stdout, one JSON message per line. Logs go to stderr.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

type config struct {
	display     string
	virtualSize string
	fps         int
	icePort     int
	iceHostIP   string
}

// message is the signaling envelope exchanged with the server and the viewer.
type message struct {
	Type       string                     `json:"type"`
	SDP        *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate  *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
	ICEServers []webrtc.ICEServer         `json:"iceServers,omitempty"`
	Display    string                     `json:"display,omitempty"`
	Virtual    bool                       `json:"virtual,omitempty"`
	Message    string                     `json:"message,omitempty"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return fallback
}

func main() {
	var cfg config
	flag.StringVar(&cfg.display, "display", "auto", `X11 display to share, or "auto" for the user's desktop on the monitor, else a virtual desktop`)
	flag.StringVar(&cfg.virtualSize, "virtual-size", envOr("WEBDESK_VIRTUAL_SIZE", "1920x1080"), "screen size of a virtual desktop")
	flag.IntVar(&cfg.fps, "fps", envInt("WEBDESK_FPS", 30), "capture frame rate")
	flag.IntVar(&cfg.icePort, "ice-port", envInt("WEBDESK_ICE_PORT", 0), "fixed UDP+TCP port for WebRTC traffic (0 = random ports)")
	flag.StringVar(&cfg.iceHostIP, "ice-host-ip", os.Getenv("WEBDESK_ICE_HOST_IP"), "advertise this IP instead of local ones (e.g. behind Docker port mapping)")
	flag.Parse()

	if flag.Arg(0) == "check" {
		runCheck()
		return
	}

	out := &output{enc: json.NewEncoder(os.Stdout)}

	virtual := false
	if cfg.display == "auto" {
		display, isVirtual, err := chooseDisplay(cfg.virtualSize, func(status string) {
			out.send(message{Type: "status", Message: status})
		})
		if err != nil {
			out.fail(err)
		}
		cfg.display, virtual = display, isVirtual
	}
	input, err := NewInput(cfg.display)
	if err != nil {
		out.fail(fmt.Errorf("can't open display %s: %w", cfg.display, err))
	}
	defer input.Close()
	api, err := newWebRTCAPI(cfg)
	if err != nil {
		out.fail(fmt.Errorf("webrtc setup: %w", err))
	}

	slog.Info("ready", "display", cfg.display, "virtual", virtual)
	out.send(message{Type: "ready", Display: cfg.display, Virtual: virtual})

	var iceServers []webrtc.ICEServer
	var session *Session
	lines := bufio.NewScanner(os.Stdin)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		var msg message
		if err := json.Unmarshal(lines.Bytes(), &msg); err != nil {
			slog.Warn("bad message", "err", err)
			continue
		}
		switch msg.Type {
		case "config":
			iceServers = msg.ICEServers
		case "offer":
			if msg.SDP == nil {
				continue
			}
			if session != nil {
				session.Close()
			}
			session, err = newSession(api, iceServers, input, cfg, out.send, *msg.SDP)
			if err != nil {
				slog.Error("session setup failed", "err", err)
			}
		case "candidate":
			if session != nil && msg.Candidate != nil {
				session.AddCandidate(*msg.Candidate)
			}
		case "logout":
			if session != nil {
				session.Close()
			}
			if desktop, ok := runningVirtualDesktop(); ok && virtual {
				stopVirtualDesktop(desktop)
			}
			out.send(message{Type: "bye", Message: "Logged out of the virtual desktop."})
			return
		}
	}

	// stdin closed: the viewer left or the SSH session ended.
	if session != nil {
		session.Close()
	}
	slog.Info("exiting")
}

// output writes newline-delimited JSON to stdout from any goroutine.
type output struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (o *output) send(msg message) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.enc.Encode(msg); err != nil {
		slog.Warn("stdout write failed", "err", err)
	}
}

// fail reports a problem the viewer should see, then exits.
func (o *output) fail(err error) {
	slog.Error("fatal", "err", err)
	o.send(message{Type: "error", Message: err.Error()})
	os.Exit(1)
}

func newWebRTCAPI(cfg config) (*webrtc.API, error) {
	media := &webrtc.MediaEngine{}
	if err := media.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(media, interceptors); err != nil {
		return nil, err
	}

	var settings webrtc.SettingEngine
	if cfg.icePort != 0 {
		udpMux, err := retry(func() (*ice.MultiUDPMuxDefault, error) { return ice.NewMultiUDPMuxFromPort(cfg.icePort) })
		if err != nil {
			return nil, fmt.Errorf("udp port %d: %w", cfg.icePort, err)
		}
		settings.SetICEUDPMux(udpMux)
		tcpListener, err := retry(func() (*net.TCPListener, error) { return net.ListenTCP("tcp", &net.TCPAddr{Port: cfg.icePort}) })
		if err != nil {
			return nil, fmt.Errorf("tcp port %d: %w", cfg.icePort, err)
		}
		settings.SetICETCPMux(webrtc.NewICETCPMux(nil, tcpListener, 8))
		settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4})
	}
	if cfg.iceHostIP != "" {
		err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{cfg.iceHostIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		})
		if err != nil {
			return nil, err
		}
	}

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(media),
		webrtc.WithInterceptorRegistry(interceptors),
		webrtc.WithSettingEngine(settings),
	), nil
}

// retry rides out a previous agent that is still releasing a fixed port.
func retry[T any](fn func() (T, error)) (T, error) {
	var v T
	var err error
	for range 15 {
		if v, err = fn(); err == nil {
			return v, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return v, err
}
