package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// X11 has no clipboard daemon: whichever program copied something owns the
// CLIPBOARD selection and hands the text over when another program pastes. So
// the agent keeps a window of its own to hold the viewer's clipboard for the
// machine, and asks the current owner for text whenever someone else copies.

const maxClipboardBytes = 1 << 20 // clipboards here are text; 1 MiB is plenty

type clipboardMessage struct {
	T    string `json:"t"`
	Text string `json:"text"`
}

type Clipboard struct {
	conn   *xgb.Conn
	window xproto.Window
	send   func(string)

	clipboard xproto.Atom
	utf8      xproto.Atom
	targets   xproto.Atom
	property  xproto.Atom
	incr      xproto.Atom

	mu      sync.Mutex
	owned   string // text we hand to programs on the machine
	lastOut string // last text sent to the viewer, to avoid echoing it back
}

func NewClipboard(display string, send func(string)) (*Clipboard, error) {
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, err
	}
	if err := xfixes.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("XFIXES extension unavailable: %w", err)
	}
	if _, err := xfixes.QueryVersion(conn, 5, 0).Reply(); err != nil {
		conn.Close()
		return nil, err
	}

	screen := xproto.Setup(conn).DefaultScreen(conn)
	window, err := xproto.NewWindowId(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// An unmapped 1x1 window is enough to own a selection and be asked for it.
	err = xproto.CreateWindowChecked(conn, screen.RootDepth, window, screen.Root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOutput, screen.RootVisual,
		xproto.CwEventMask, []uint32{uint32(xproto.EventMaskPropertyChange)}).Check()
	if err != nil {
		conn.Close()
		return nil, err
	}

	c := &Clipboard{conn: conn, window: window, send: send}
	for _, wanted := range []struct {
		name string
		atom *xproto.Atom
	}{
		{"CLIPBOARD", &c.clipboard},
		{"UTF8_STRING", &c.utf8},
		{"TARGETS", &c.targets},
		{"WEBDESK_CLIPBOARD", &c.property},
		{"INCR", &c.incr},
	} {
		reply, err := xproto.InternAtom(conn, false, uint16(len(wanted.name)), wanted.name).Reply()
		if err != nil {
			conn.Close()
			return nil, err
		}
		*wanted.atom = reply.Atom
	}

	xfixes.SelectSelectionInput(conn, window, c.clipboard, xfixes.SelectionEventMaskSetSelectionOwner)
	slog.Debug("clipboard watching", "display", display, "window", window, "selection", c.clipboard)
	go c.run()
	c.requestText(xproto.TimeCurrentTime) // pick up what's already on the clipboard
	return c, nil
}

func (c *Clipboard) Close() { c.conn.Close() }

func (c *Clipboard) run() {
	for {
		event, xerr := c.conn.WaitForEvent()
		if event == nil && xerr == nil {
			return // connection closed
		}
		if xerr != nil {
			slog.Debug("clipboard x11 error", "err", xerr)
			continue
		}
		switch ev := event.(type) {
		case xfixes.SelectionNotifyEvent:
			// Something on the machine copied; ask it for the text.
			slog.Debug("clipboard owner changed", "owner", ev.Owner, "ours", c.window)
			if ev.Owner != c.window {
				c.requestText(ev.Timestamp)
			}
		case xproto.SelectionNotifyEvent:
			c.readText(ev)
		case xproto.SelectionRequestEvent:
			c.answer(ev)
		case xproto.SelectionClearEvent:
			c.mu.Lock()
			c.owned = ""
			c.mu.Unlock()
		}
	}
}

// requestText asks the selection's owner to write the text into our window.
func (c *Clipboard) requestText(when xproto.Timestamp) {
	xproto.ConvertSelection(c.conn, c.window, c.clipboard, c.utf8, c.property, when)
}

// readText collects the text the owner wrote and passes it to the viewer.
func (c *Clipboard) readText(ev xproto.SelectionNotifyEvent) {
	if ev.Property == 0 {
		return // the owner had nothing in a form we asked for
	}
	reply, err := xproto.GetProperty(c.conn, true, c.window, c.property, xproto.GetPropertyTypeAny, 0, maxClipboardBytes/4).Reply()
	if err != nil {
		slog.Debug("clipboard read failed", "err", err)
		return
	}
	if reply.Type == c.incr {
		slog.Warn("clipboard is too large to copy across")
		return
	}
	slog.Debug("clipboard read from machine", "bytes", len(reply.Value), "type", reply.Type)
	text := string(reply.Value)
	if text == "" {
		return
	}

	c.mu.Lock()
	unchanged := text == c.lastOut || text == c.owned
	c.lastOut = text
	c.mu.Unlock()
	if unchanged {
		return
	}
	if data, err := json.Marshal(clipboardMessage{T: "clipboard", Text: text}); err == nil {
		slog.Debug("clipboard sent to viewer", "bytes", len(text))
		c.send(string(data))
	}
}

// SetFromViewer puts the viewer's clipboard on the machine's, by owning the
// selection: programs on the machine ask us for it when they paste.
func (c *Clipboard) SetFromViewer(text string) {
	if text == "" {
		return
	}
	c.mu.Lock()
	if text == c.owned {
		c.mu.Unlock()
		return
	}
	c.owned = text
	c.lastOut = text // it came from the viewer; don't send it straight back
	c.mu.Unlock()
	slog.Debug("clipboard taken from viewer", "bytes", len(text))
	xproto.SetSelectionOwner(c.conn, c.window, c.clipboard, xproto.TimeCurrentTime)
}

// answer hands our text to a program on the machine that is pasting.
func (c *Clipboard) answer(ev xproto.SelectionRequestEvent) {
	c.mu.Lock()
	text := c.owned
	c.mu.Unlock()

	property := ev.Property
	if property == 0 {
		property = ev.Target // very old clients leave it unset
	}
	switch ev.Target {
	case c.targets:
		offered := []xproto.Atom{c.targets, c.utf8, xproto.AtomString}
		data := make([]byte, 4*len(offered))
		for i, atom := range offered {
			binary.LittleEndian.PutUint32(data[i*4:], uint32(atom))
		}
		xproto.ChangeProperty(c.conn, xproto.PropModeReplace, ev.Requestor, property, xproto.AtomAtom, 32, uint32(len(offered)), data)
	case c.utf8, xproto.AtomString:
		xproto.ChangeProperty(c.conn, xproto.PropModeReplace, ev.Requestor, property, ev.Target, 8, uint32(len(text)), []byte(text))
	default:
		property = 0 // we can't provide that form
	}

	notify := xproto.SelectionNotifyEvent{
		Time:      ev.Time,
		Requestor: ev.Requestor,
		Selection: ev.Selection,
		Target:    ev.Target,
		Property:  property,
	}
	xproto.SendEvent(c.conn, false, ev.Requestor, 0, string(notify.Bytes()))
}
