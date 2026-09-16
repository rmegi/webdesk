package main

import (
	"encoding/json"
	"os"
	"os/exec"
)

// checkResult tells the server what the machine still needs before a session
// can start. It is printed by `webdesk-agent check`.
type checkResult struct {
	Virtual  bool     `json:"virtual"`        // no X11 desktop on the monitor, so a virtual one will be used
	Missing  []string `json:"missing"`        // commands that aren't installed; "desktop" means no desktop environment
	Packages []string `json:"packages"`       // apt packages providing them, when apt is available
	Sudo     string   `json:"sudo,omitempty"` // "nopasswd", "password" or "none"; only checked when something is missing
}

// aptPackages maps each required command to the Debian packages providing it.
var aptPackages = map[string][]string{
	"ffmpeg":      {"ffmpeg"},
	"Xvfb":        {"xvfb"},
	"dbus-launch": {"dbus-x11"},
	"desktop":     {"xfce4-session", "xfwm4", "xfce4-panel", "xfdesktop4", "xfce4-terminal"},
}

func runCheck() {
	result := checkResult{Missing: []string{}, Packages: []string{}}
	need := func(command string) {
		if _, err := exec.LookPath(command); err != nil {
			result.Missing = append(result.Missing, command)
		}
	}

	need("ffmpeg")
	running, haveVirtual := runningVirtualDesktop()
	_, live := findLiveDisplay(running.Display)
	result.Virtual = !live
	if !live && !haveVirtual {
		need("Xvfb")
		need("dbus-launch")
		if findDesktopSession() == "" {
			result.Missing = append(result.Missing, "desktop")
		}
	}

	if len(result.Missing) > 0 {
		if _, err := exec.LookPath("apt-get"); err == nil {
			for _, missing := range result.Missing {
				result.Packages = append(result.Packages, aptPackages[missing]...)
			}
		}
		result.Sudo = sudoAccess()
	}
	json.NewEncoder(os.Stdout).Encode(result)
}

func sudoAccess() string {
	if _, err := exec.LookPath("sudo"); err != nil {
		return "none"
	}
	if exec.Command("sudo", "-n", "true").Run() == nil {
		return "nopasswd"
	}
	return "password"
}
