package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// liveDisplay is an X11 desktop the user is running on the machine's monitor.
type liveDisplay struct {
	display    string
	xauthority string
}

// findLiveDisplay looks for an X11 desktop the current user is logged into on
// the machine's monitor by reading the environment of that user's processes.
// Wayland sessions don't count (their Xwayland display only shows X11 apps),
// and skip excludes webdesk's own virtual desktop.
func findLiveDisplay(skip string) (liveDisplay, bool) {
	uid := uint32(os.Getuid())
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	sort.Slice(dirs, func(i, j int) bool { return pidOf(dirs[i]) < pidOf(dirs[j]) })

	var typed, untyped []map[string]string
	waylandDisplays := map[string]bool{}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "environ"))
		if err != nil {
			continue
		}
		env := parseEnviron(raw)
		display := env["DISPLAY"]
		// No display, an SSH-forwarded one like localhost:10, or our own.
		if !strings.HasPrefix(display, ":") || display == skip {
			continue
		}
		switch env["XDG_SESSION_TYPE"] {
		case "wayland":
			waylandDisplays[display] = true
		case "x11":
			typed = append(typed, env)
		default:
			// startx and Xvfb sessions often don't set XDG_SESSION_TYPE.
			untyped = append(untyped, env)
		}
	}

	for _, env := range append(typed, untyped...) {
		if !waylandDisplays[env["DISPLAY"]] {
			return liveDisplay{display: env["DISPLAY"], xauthority: env["XAUTHORITY"]}, true
		}
	}
	return liveDisplay{}, false
}

// chooseDisplay picks what the viewer sees: the user's X11 desktop on the
// monitor if there is one, otherwise webdesk's virtual desktop, starting it if
// needed. It exports XAUTHORITY so our X connection and ffmpeg can connect.
func chooseDisplay(size string, status func(string)) (display string, virtual bool, err error) {
	running, haveVirtual := runningVirtualDesktop()
	if live, ok := findLiveDisplay(running.Display); ok {
		if live.xauthority != "" {
			os.Setenv("XAUTHORITY", live.xauthority)
		}
		return live.display, false, nil
	}

	os.Setenv("XAUTHORITY", virtualAuthPath())
	if haveVirtual {
		return running.Display, true, nil
	}
	status("Starting a virtual desktop…")
	started, err := startVirtualDesktop(size)
	return started.Display, true, err
}

func parseEnviron(raw []byte) map[string]string {
	env := map[string]string{}
	for _, entry := range strings.Split(string(raw), "\x00") {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	return env
}

func pidOf(dir string) int {
	pid, _ := strconv.Atoi(filepath.Base(dir))
	return pid
}
