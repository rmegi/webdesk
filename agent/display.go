package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// findDisplay locates the desktop session the current user is logged into on
// the machine's own screen by reading the environment of that user's processes.
// It exports XAUTHORITY so both our X connection and ffmpeg can authenticate.
func findDisplay() (string, error) {
	uid := uint32(os.Getuid())
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	sort.Slice(dirs, func(i, j int) bool { return pidOf(dirs[i]) < pidOf(dirs[j]) })

	var fallback map[string]string
	sawWayland := false
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
		switch {
		case env["XDG_SESSION_TYPE"] == "wayland":
			sawWayland = true
		case !strings.HasPrefix(env["DISPLAY"], ":"):
			// No display, or an SSH-forwarded one like localhost:10.
		case env["XDG_SESSION_TYPE"] == "x11":
			return useDisplay(env), nil
		case fallback == nil:
			// startx or Xvfb sessions often don't set XDG_SESSION_TYPE.
			fallback = env
		}
	}

	if sawWayland {
		return "", errors.New("this desktop session is Wayland, and webdesk supports X11 for now. Log out and pick an Xorg session at the login screen")
	}
	if fallback != nil {
		return useDisplay(fallback), nil
	}
	name := strconv.Itoa(int(uid))
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return "", fmt.Errorf("no desktop session found for %s. Log in on the machine's screen first", name)
}

func useDisplay(env map[string]string) string {
	if xauth := env["XAUTHORITY"]; xauth != "" {
		os.Setenv("XAUTHORITY", xauth)
	}
	return env["DISPLAY"]
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
