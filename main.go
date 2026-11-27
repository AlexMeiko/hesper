package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"

	"hesper/internal/bootstrap"
	"hesper/internal/config"
	"hesper/internal/platform"
)

//go:embed all:frontend/dist
var embeddedAssets embed.FS

func main() {
	paths, err := config.ResolvePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hesper: resolve data paths failed: %v\n", err)
		os.Exit(1)
	}

	// Must run BEFORE GTK/WebKit initialization on Linux (Wayland + NVIDIA safety).
	platform.InitGraphicsEnv(os.Args[1:], paths.ConfigDir)

	distFS, err := fs.Sub(embeddedAssets, "frontend/dist")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hesper: embedded frontend/dist missing: %v\n", err)
		os.Exit(1)
	}

	if err := bootstrap.Run(paths, distFS); err != nil {
		fmt.Fprintf(os.Stderr, "hesper: fatal error: %v\n", err)
		os.Exit(1)
	}
}
