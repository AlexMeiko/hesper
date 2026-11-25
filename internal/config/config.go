package config

import (
	"os"
	"path/filepath"
	"runtime"
)

const AppName = "sql-manager"

// Paths holds resolved OS-appropriate local directories for app.db, result snapshots, and logs.
type Paths struct {
	ConfigDir string
	CacheDir  string
	StateDir  string
	DBPath    string
	ResultDir string
	LogDir    string
	Portable  bool
}

// ResolvePaths computes XDG / AppData / Portable directories and ensures they exist.
func ResolvePaths() (Paths, error) {
	if customRoot := os.Getenv("SQL_MANAGER_DATA_DIR"); customRoot != "" {
		return ensurePaths(customRoot, customRoot, customRoot, false)
	}

	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		if _, statErr := os.Stat(filepath.Join(exeDir, "portable.flag")); statErr == nil {
			dataRoot := filepath.Join(exeDir, "data")
			return ensurePaths(dataRoot, filepath.Join(dataRoot, "cache"), filepath.Join(dataRoot, "state"), true)
		}
	}

	var cfgBase, cacheBase, stateBase string
	switch runtime.GOOS {
	case "windows":
		cfgBase = os.Getenv("APPDATA")
		cacheBase = os.Getenv("LOCALAPPDATA")
		stateBase = cacheBase
	default:
		home, _ := os.UserHomeDir()
		cfgBase = envOrDefault("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		cacheBase = envOrDefault("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
		stateBase = envOrDefault("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	}

	if cfgBase == "" {
		cfgBase = os.TempDir()
	}
	if cacheBase == "" {
		cacheBase = cfgBase
	}
	if stateBase == "" {
		stateBase = cfgBase
	}

	return ensurePaths(
		filepath.Join(cfgBase, AppName),
		filepath.Join(cacheBase, AppName),
		filepath.Join(stateBase, AppName),
		false,
	)
}

func ensurePaths(cfgDir, cacheDir, stateDir string, portable bool) (Paths, error) {
	p := Paths{
		ConfigDir: cfgDir,
		CacheDir:  cacheDir,
		StateDir:  stateDir,
		DBPath:    filepath.Join(cfgDir, "app.db"),
		ResultDir: filepath.Join(cacheDir, "results"),
		LogDir:    filepath.Join(stateDir, "logs"),
		Portable:  portable,
	}
	for _, dir := range []string{p.ConfigDir, p.CacheDir, p.StateDir, p.ResultDir, p.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Paths{}, err
		}
	}
	return p, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
