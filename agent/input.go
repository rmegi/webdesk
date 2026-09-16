package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// Browser MouseEvent.button → X11 button. X11 uses 4-7 for the wheel.
var browserButtons = map[int]byte{0: 1, 1: 2, 2: 3, 3: 8, 4: 9}

const (
	wheelUp, wheelDown, wheelLeft, wheelRight byte = 4, 5, 6, 7
	maxWheelClicks                                 = 20
)

// inputEvent is one message on the "input" data channel.
type inputEvent struct {
	T    string  `json:"t"` // move | button | wheel | key
	X    float64 `json:"x"` // 0..1 across the screen
	Y    float64 `json:"y"`
	B    int     `json:"b"` // browser button index
	Down bool    `json:"down"`
	DX   int     `json:"dx"` // wheel notches
	DY   int     `json:"dy"`
	Code string  `json:"code"` // KeyboardEvent.code
}

// Input replays viewer events into the X server through the XTEST extension.
type Input struct {
	conn    *xgb.Conn
	root    xproto.Window
	injects bool // the X server really acts on injected input

	mu      sync.Mutex
	width   int
	height  int
	keys    map[xproto.Keycode]bool
	buttons map[byte]bool
}

func NewInput(display string) (*Input, error) {
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, err
	}
	if err := xtest.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("XTEST extension unavailable: %w", err)
	}
	in := &Input{
		conn:    conn,
		root:    xproto.Setup(conn).DefaultScreen(conn).Root,
		keys:    map[xproto.Keycode]bool{},
		buttons: map[byte]bool{},
	}
	// Errors from fire-and-forget requests arrive on the event queue; drain it.
	go func() {
		for {
			ev, xerr := conn.WaitForEvent()
			if ev == nil && xerr == nil {
				return // connection closed
			}
			if xerr != nil {
				slog.Debug("x11 error", "err", xerr)
			}
		}
	}()
	if _, _, err := in.ScreenSize(); err != nil {
		conn.Close()
		return nil, err
	}
	in.injects = in.probeInjection()
	return in, nil
}

func (in *Input) Close() { in.conn.Close() }

// CanInject reports whether this display acts on the input we send it.
func (in *Input) CanInject() bool { return in.injects }

// probeInjection nudges the pointer by a pixel and asks the X server where the
// pointer ended up. Xwayland and servers with XTEST switched off accept the
// requests and quietly ignore them, which would leave the viewer with a screen
// they can look at but not touch.
func (in *Input) probeInjection() bool {
	if extensions, err := xproto.ListExtensions(in.conn).Reply(); err == nil {
		names := make([]string, 0, len(extensions.Names))
		for _, name := range extensions.Names {
			names = append(names, name.Name)
		}
		slog.Info("x server", "xwayland", slices.Contains(names, "XWAYLAND"), "xtest", slices.Contains(names, "XTEST"))
	}

	before, err := xproto.QueryPointer(in.conn, in.root).Reply()
	if err != nil {
		return false
	}
	target := before.RootX + 1
	if int(target) >= in.width {
		target = before.RootX - 1
	}
	xtest.FakeInput(in.conn, xproto.MotionNotify, 0, 0, in.root, target, before.RootY, 0)
	// QueryPointer is a round trip, so the move has been handled by now.
	after, err := xproto.QueryPointer(in.conn, in.root).Reply()
	if err != nil {
		return false
	}
	xtest.FakeInput(in.conn, xproto.MotionNotify, 0, 0, in.root, before.RootX, before.RootY, 0)
	return after.RootX == target
}

// ScreenSize reads the current root window size, so resolution changes are picked up.
func (in *Input) ScreenSize() (int, int, error) {
	geo, err := xproto.GetGeometry(in.conn, xproto.Drawable(in.root)).Reply()
	if err != nil {
		return 0, 0, err
	}
	in.mu.Lock()
	in.width, in.height = int(geo.Width), int(geo.Height)
	in.mu.Unlock()
	return int(geo.Width), int(geo.Height), nil
}

func (in *Input) Handle(data []byte) {
	var ev inputEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()

	switch ev.T {
	case "move":
		x := int16(min(max(ev.X, 0), 1) * float64(in.width-1))
		y := int16(min(max(ev.Y, 0), 1) * float64(in.height-1))
		xtest.FakeInput(in.conn, xproto.MotionNotify, 0, 0, in.root, x, y, 0)
	case "button":
		if b, ok := browserButtons[ev.B]; ok {
			in.setButton(b, ev.Down)
		}
	case "wheel":
		in.scroll(ev.DY, wheelUp, wheelDown)
		in.scroll(ev.DX, wheelLeft, wheelRight)
	case "key":
		code, ok := evdevCodes[ev.Code]
		if !ok {
			slog.Debug("unmapped key", "code", ev.Code)
			return
		}
		in.setKey(xproto.Keycode(code+8), ev.Down)
	}
}

// ReleaseAll lifts every key and button the viewer left held, e.g. on disconnect.
func (in *Input) ReleaseAll() {
	in.mu.Lock()
	defer in.mu.Unlock()
	for key := range in.keys {
		in.setKey(key, false)
	}
	for b := range in.buttons {
		in.setButton(b, false)
	}
}

func (in *Input) setKey(key xproto.Keycode, down bool) {
	if in.keys[key] == down {
		return
	}
	kind := byte(xproto.KeyRelease)
	if down {
		kind = xproto.KeyPress
		in.keys[key] = true
	} else {
		delete(in.keys, key)
	}
	xtest.FakeInput(in.conn, kind, byte(key), 0, in.root, 0, 0, 0)
}

func (in *Input) setButton(b byte, down bool) {
	if in.buttons[b] == down {
		return
	}
	kind := byte(xproto.ButtonRelease)
	if down {
		kind = xproto.ButtonPress
		in.buttons[b] = true
	} else {
		delete(in.buttons, b)
	}
	xtest.FakeInput(in.conn, kind, b, 0, in.root, 0, 0, 0)
}

// scroll sends n wheel clicks: negative n on the neg button, positive on pos.
func (in *Input) scroll(n int, neg, pos byte) {
	button := pos
	if n < 0 {
		button, n = neg, -n
	}
	for range min(n, maxWheelClicks) {
		xtest.FakeInput(in.conn, xproto.ButtonPress, button, 0, in.root, 0, 0, 0)
		xtest.FakeInput(in.conn, xproto.ButtonRelease, button, 0, in.root, 0, 0, 0)
	}
}
