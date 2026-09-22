package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"

	"github.com/pion/webrtc/v4"
)

// errKeyframeRestart means the capture stopped so a fresh keyframe can be made.
var errKeyframeRestart = errors.New("restarting the encoder for a keyframe")

// capture grabs the X11 display with ffmpeg, encodes it as H.264 and has
// ffmpeg send RTP to a local UDP socket, which we forward into the track.
// Receiving ready-made RTP avoids buffering a frame to find NAL boundaries.
// The cursor is left out of the video; the viewer draws it (see cursor.go).
// Returns when ctx is cancelled, ffmpeg exits, or a keyframe is asked for.
func capture(ctx context.Context, display string, width, height, fps int, track *webrtc.TrackLocalStaticRTP, keyframe <-chan struct{}) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(4 << 20) // keyframes arrive as bursts of packets

	// yuv420p needs even dimensions.
	width, height = width&^1, height&^1
	port := conn.LocalAddr().(*net.UDPAddr).Port
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "x11grab", "-draw_mouse", "0", "-framerate", strconv.Itoa(fps),
		"-video_size", fmt.Sprintf("%dx%d", width, height), "-i", display,
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-pix_fmt", "yuv420p",
		// A keyframe a second, so a lost one is never missed for long.
		"-g", strconv.Itoa(fps), "-crf", "26", "-maxrate", "4M", "-bufsize", "2M",
		"-f", "rtp", "-payload_type", "96", fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", port),
	)
	cmd.Stderr = os.Stderr
	killWithParent(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	slog.Info("capture started", "display", display, "size", fmt.Sprintf("%dx%d", width, height), "fps", fps)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	forwarded := make(chan error, 1)
	go func() { forwarded <- forwardRTP(conn, track) }()

	stopEncoder := func() {
		_ = cmd.Process.Kill()
		<-exited
		conn.Close()
		<-forwarded
	}

	select {
	case <-ctx.Done():
		stopEncoder()
		return ctx.Err()
	case <-keyframe:
		stopEncoder()
		return errKeyframeRestart
	case err := <-forwarded:
		_ = cmd.Process.Kill()
		<-exited
		return err
	case err := <-exited:
		conn.Close()
		<-forwarded
		if err != nil {
			return fmt.Errorf("ffmpeg: %w", err)
		}
		return errors.New("ffmpeg exited")
	}
}

// forwardRTP passes ffmpeg's RTP packets to the viewer until the socket closes.
func forwardRTP(conn *net.UDPConn, track *webrtc.TrackLocalStaticRTP) error {
	buf := make([]byte, 1600)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		if _, err := track.Write(buf[:n]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
	}
}
