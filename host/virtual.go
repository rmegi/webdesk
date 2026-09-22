package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jezek/xgb"
)

// When the user has no X11 desktop on the machine's monitor (no monitor, or a
// Wayland desktop), webdesk runs its own X11 desktop on Xvfb. It keeps running
// after the viewer disconnects, so reconnecting returns to the same windows.

// Desktop sessions that work on a virtual display, in order of preference.
var desktopSessions = []string{"startlxde-pi", "startxfce4", "mate-session", "startlxqt", "startlxde", "openbox-session"}

type virtualDesktop struct {
	Display    string `json:"display"`
	XvfbPID    int    `json:"xvfbPid"`
	SessionPID int    `json:"sessionPid"`
}

func webdeskDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "webdesk")
}

func virtualStatePath() string { return filepath.Join(webdeskDir(), "virtual-desktop.json") }

func virtualAuthPath() string { return filepath.Join(webdeskDir(), "Xauthority") }

func findDesktopSession() string {
	for _, name := range desktopSessions {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}

// runningVirtualDesktop returns webdesk's virtual desktop if it is still up.
func runningVirtualDesktop() (virtualDesktop, bool) {
	var desktop virtualDesktop
	data, err := os.ReadFile(virtualStatePath())
	if err != nil || json.Unmarshal(data, &desktop) != nil {
		return virtualDesktop{}, false
	}
	if !isProcess(desktop.XvfbPID, "Xvfb") {
		os.Remove(virtualStatePath())
		return virtualDesktop{}, false
	}
	return desktop, true
}

func startVirtualDesktop(size string) (virtualDesktop, error) {
	session := findDesktopSession()
	if session == "" {
		return virtualDesktop{}, errors.New("no desktop environment is installed for a virtual desktop")
	}
	if err := os.MkdirAll(webdeskDir(), 0o700); err != nil {
		return virtualDesktop{}, err
	}

	number := freeDisplayNumber()
	display := ":" + strconv.Itoa(number)
	cookie := make([]byte, 16)
	rand.Read(cookie)
	if err := writeXauthority(virtualAuthPath(), number, cookie); err != nil {
		return virtualDesktop{}, err
	}

	log, err := os.OpenFile(filepath.Join(webdeskDir(), "virtual-desktop.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return virtualDesktop{}, err
	}
	defer log.Close()

	xvfb := exec.Command("Xvfb", display, "-screen", "0", size+"x24", "-nolisten", "tcp", "-auth", virtualAuthPath())
	if err := startDetached(xvfb, log); err != nil {
		return virtualDesktop{}, fmt.Errorf("start Xvfb: %w", err)
	}
	if err := waitForDisplay(display, 10*time.Second); err != nil {
		stopProcessGroup(xvfb.Process.Pid)
		return virtualDesktop{}, err
	}

	args := []string{session}
	if _, err := exec.LookPath("dbus-launch"); err == nil {
		args = []string{"dbus-launch", "--exit-with-session", session}
	}
	desktop := exec.Command(args[0], args[1:]...)
	// Force the X11 backends. On a machine that is also running a Wayland
	// session, GTK and Qt apps otherwise connect to that compositor — lxpanel
	// crashes with "invalid cast from GdkWaylandDisplay" and the screen stays black.
	desktop.Env = append(withoutEnv(os.Environ(),
		"DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "XDG_SESSION_TYPE",
		"GDK_BACKEND", "QT_QPA_PLATFORM", "CLUTTER_BACKEND", "SDL_VIDEODRIVER"),
		"DISPLAY="+display, "XAUTHORITY="+virtualAuthPath(), "XDG_SESSION_TYPE=x11",
		"GDK_BACKEND=x11", "QT_QPA_PLATFORM=xcb", "CLUTTER_BACKEND=x11", "SDL_VIDEODRIVER=x11")
	if err := startDetached(desktop, log); err != nil {
		stopProcessGroup(xvfb.Process.Pid)
		return virtualDesktop{}, fmt.Errorf("start %s: %w", session, err)
	}

	started := virtualDesktop{Display: display, XvfbPID: xvfb.Process.Pid, SessionPID: desktop.Process.Pid}
	data, _ := json.Marshal(started)
	return started, os.WriteFile(virtualStatePath(), data, 0o600)
}

// stopVirtualDesktop ends the desktop session and its display.
func stopVirtualDesktop(desktop virtualDesktop) {
	stopProcessGroup(desktop.SessionPID)
	stopProcessGroup(desktop.XvfbPID)
	os.Remove(virtualStatePath())
	os.Remove(virtualAuthPath())
}

// startDetached runs cmd in its own session with output to log, so it outlives
// the host and the SSH connection.
func startDetached(cmd *exec.Cmd, log *os.File) error {
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it if it exits while the host is still running
	return nil
}

// stopProcessGroup asks a detached process group to exit, then forces it.
func stopProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	syscall.Kill(-pid, syscall.SIGTERM)
	for range 20 {
		if syscall.Kill(-pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
}

func waitForDisplay(display string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := xgb.NewConnDisplay(display)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("virtual display %s didn't start: %w", display, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// freeDisplayNumber finds an unused X display number, starting above the
// numbers desktops and SSH forwarding usually take.
func freeDisplayNumber() int {
	for n := 20; ; n++ {
		_, lockErr := os.Stat(fmt.Sprintf("/tmp/.X%d-lock", n))
		_, socketErr := os.Stat(fmt.Sprintf("/tmp/.X11-unix/X%d", n))
		if errors.Is(lockErr, os.ErrNotExist) && errors.Is(socketErr, os.ErrNotExist) {
			return n
		}
	}
}

// writeXauthority writes a one-entry Xauthority file with an MIT-MAGIC-COOKIE-1
// for the display, so only this user can connect to it.
func writeXauthority(path string, display int, cookie []byte) error {
	var buf bytes.Buffer
	field := func(data []byte) {
		binary.Write(&buf, binary.BigEndian, uint16(len(data)))
		buf.Write(data)
	}
	binary.Write(&buf, binary.BigEndian, uint16(0xffff)) // FamilyWild: any host
	field(nil)
	field([]byte(strconv.Itoa(display)))
	field([]byte("MIT-MAGIC-COOKIE-1"))
	field(cookie)
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// isProcess reports whether pid is a live (not zombie) process running name.
func isProcess(pid int, name string) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	start, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if start < 0 || end < start || end+2 >= len(stat) {
		return false
	}
	return string(stat[start+1:end]) == name && stat[end+2] != 'Z'
}

func withoutEnv(env []string, keys ...string) []string {
	var kept []string
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(keys, key) {
			kept = append(kept, entry)
		}
	}
	return kept
}
