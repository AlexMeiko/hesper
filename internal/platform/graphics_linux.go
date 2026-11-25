//go:build linux

package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type graphicsPrefs struct {
	Mode string `json:"mode"` // "auto" | "disable_dmabuf" | "software"
}

// InitGraphicsEnv runs BEFORE GTK/WebKit initialization to protect Linux Wayland + NVIDIA
// setups (Ubuntu 24.04+, Debian 13+, CachyOS) against blank windows or Error 71 crashes.
func InitGraphicsEnv(args []string, configDir string) {
	mode := "auto"
	if configDir != "" {
		if raw, err := os.ReadFile(filepath.Join(configDir, "graphics.json")); err == nil {
			var prefs graphicsPrefs
			if json.Unmarshal(raw, &prefs) == nil && prefs.Mode != "" {
				mode = prefs.Mode
			}
		}
	}

	for _, a := range args {
		switch a {
		case "--disable-dmabuf":
			mode = "disable_dmabuf"
		case "--software-gl", "--safe-graphics":
			mode = "software"
		}
	}

	switch mode {
	case "software":
		setEnvIfUnset("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
		setEnvIfUnset("WEBKIT_DISABLE_COMPOSITING_MODE", "1")
	case "disable_dmabuf":
		setEnvIfUnset("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	default:
		if isWaylandSession() && hasProprietaryNvidia() {
			setEnvIfUnset("__NV_DISABLE_EXPLICIT_SYNC", "1")
		}
	}
}

func isWaylandSession() bool {
	return strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") || os.Getenv("WAYLAND_DISPLAY") != ""
}

func hasProprietaryNvidia() bool {
	_, err := os.Stat("/proc/driver/nvidia/version")
	return err == nil
}

func setEnvIfUnset(key, val string) {
	if _, ok := os.LookupEnv(key); !ok {
		_ = os.Setenv(key, val)
	}
}
