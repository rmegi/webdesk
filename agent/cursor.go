package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// The viewer draws the remote cursor itself, at the local pointer, so it never
// lags a network round trip behind the mouse. The agent only sends the cursor's
// picture when its shape changes, and the capture leaves the cursor out of the
// video entirely.

// Viewers cache cursors by serial; clearing ours just resends the images.
const maxCursorCache = 128

type cursorMessage struct {
	T      string `json:"t"`
	Serial uint32 `json:"serial"`
	Width  int    `json:"w,omitempty"`
	Height int    `json:"h,omitempty"`
	XHot   int    `json:"xhot,omitempty"`
	YHot   int    `json:"yhot,omitempty"`
	PNG    string `json:"png,omitempty"` // base64; left out when the viewer already has this serial
}

// watchCursor reports the cursor shape to the viewer until its X connection is
// closed. Closing the returned connection stops it.
func watchCursor(display string, send func(string)) (*xgb.Conn, error) {
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, err
	}
	if err := xfixes.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("XFIXES extension unavailable: %w", err)
	}
	// XFIXES requires a version handshake before any other request.
	if _, err := xfixes.QueryVersion(conn, 5, 0).Reply(); err != nil {
		conn.Close()
		return nil, err
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	xfixes.SelectCursorInput(conn, root, xfixes.CursorNotifyMaskDisplayCursor)

	go func() {
		sent := map[uint32]bool{}
		report := func() {
			msg, err := currentCursor(conn, sent)
			if err != nil {
				slog.Debug("cursor image", "err", err)
				return
			}
			if data, err := json.Marshal(msg); err == nil {
				send(string(data))
			}
		}
		report()
		for {
			event, xerr := conn.WaitForEvent()
			if event == nil && xerr == nil {
				return // connection closed
			}
			if _, ok := event.(xfixes.CursorNotifyEvent); ok {
				report()
			}
		}
	}()
	return conn, nil
}

func currentCursor(conn *xgb.Conn, sent map[uint32]bool) (cursorMessage, error) {
	reply, err := xfixes.GetCursorImage(conn).Reply()
	if err != nil {
		return cursorMessage{}, err
	}
	msg := cursorMessage{T: "cursor", Serial: reply.CursorSerial}
	if sent[reply.CursorSerial] {
		return msg, nil
	}

	image, err := encodeCursor(int(reply.Width), int(reply.Height), reply.CursorImage)
	if err != nil {
		return cursorMessage{}, err
	}
	if len(sent) >= maxCursorCache {
		clear(sent)
	}
	sent[reply.CursorSerial] = true
	msg.Width, msg.Height = int(reply.Width), int(reply.Height)
	msg.XHot, msg.YHot = int(reply.Xhot), int(reply.Yhot)
	msg.PNG = image
	return msg, nil
}

// encodeCursor turns XFIXES premultiplied ARGB pixels into a base64 PNG.
func encodeCursor(width, height int, pixels []uint32) (string, error) {
	if width <= 0 || height <= 0 || len(pixels) < width*height {
		return "", fmt.Errorf("cursor is %dx%d with %d pixels", width, height, len(pixels))
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := range width * height {
		pixel := pixels[i]
		alpha := uint8(pixel >> 24)
		red, green, blue := uint8(pixel>>16), uint8(pixel>>8), uint8(pixel)
		if alpha > 0 && alpha < 255 { // undo the premultiplication
			red = unpremultiply(red, alpha)
			green = unpremultiply(green, alpha)
			blue = unpremultiply(blue, alpha)
		}
		img.SetNRGBA(i%width, i/width, color.NRGBA{R: red, G: green, B: blue, A: alpha})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func unpremultiply(value, alpha uint8) uint8 {
	return uint8(min(int(value)*255/int(alpha), 255))
}
