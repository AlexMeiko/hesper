package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"sql-manager/internal/binding"
	"sql-manager/internal/config"
	"sql-manager/internal/driver"
	"sql-manager/internal/driver/mysql"
	"sql-manager/internal/driver/postgres"
	"sql-manager/internal/dto"
	"sql-manager/internal/repository"
	"sql-manager/internal/resultbuf"
	"sql-manager/internal/secret"
	"sql-manager/internal/service"
	"sql-manager/internal/session"
)

// Run is the application Composition Root.
// Wails v3 application lifecycle, dependency wiring, and window creation live strictly here.
func Run(paths config.Paths, assets fs.FS) error {
	// 1. Local metadata database (app.db) — completely isolated from user target DBs.
	localDB, err := repository.OpenLocalDB(paths.DBPath)
	if err != nil {
		return fmt.Errorf("open local repository: %w", err)
	}

	// 2. Credential store (OS Keyring with encrypted file fallback).
	secretStore := secret.NewStore(paths.ConfigDir)

	// 3. Static driver registry — Phase 1A/1B registers real MySQL 8.4 and PostgreSQL drivers.
	reg := driver.NewRegistry()
	reg.Register(mysql.NewOpener(dto.EngineMySQL))
	reg.Register(postgres.NewOpener())

	// 4. Execute-Once result snapshot buffer (memory -> disk spill with hard cap + TTL).
	resMgr := resultbuf.NewManager(resultbuf.Options{
		SpillDir: paths.ResultDir,
	})

	// 5. Pure Go application services & dedicated-connection session manager.
	connSvc := service.NewConnectionService(localDB, secretStore, nil, resMgr)
	sessMgr := session.NewManager(reg, connSvc.ResolveConfig)
	connSvc.SetSessionManager(sessMgr)

	schemaSvc := service.NewSchemaService(sessMgr)
	querySvc := service.NewQueryService(sessMgr, resMgr, localDB)

	// 6. Thin Wails v3 boundary adapters.
	connBinding := binding.NewConnectionBinding(connSvc)
	schemaBinding := binding.NewSchemaBinding(schemaSvc)
	queryBinding := binding.NewQueryBinding(querySvc)
	streamBinding := binding.NewStreamBinding(querySvc)

	// Optional port override for headless `-tags server` mode.
	serverPort := 8080
	if p := os.Getenv("SQL_MANAGER_PORT"); p != "" {
		if parsed, convErr := strconv.Atoi(p); convErr == nil && parsed > 0 {
			serverPort = parsed
		}
	}

	// 7. Create Wails v3 application.
	app := application.New(application.Options{
		Name:        "SQL Manager",
		Description: "Lightweight cross-platform desktop database workbench for MySQL & PostgreSQL",
		LogLevel:    slog.LevelWarn,
		Services: []application.Service{
			application.NewService(connBinding),
			application.NewService(schemaBinding),
			application.NewService(queryBinding),
			application.NewServiceWithOptions(streamBinding, application.ServiceOptions{
				Route: "/stream",
			}),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		MarshalError: marshalAppError,
		Server: application.ServerOptions{
			Host:         "127.0.0.1",
			Port:         serverPort,
			ReadTimeout:  10 * time.Minute,
			WriteTimeout: 10 * time.Minute,
			IdleTimeout:  10 * time.Minute,
		},
		OnShutdown: func() {
			sessMgr.CloseAll()
			resMgr.CloseAll()
			_ = localDB.Close()
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "SQL Manager",
		Width:            1360,
		Height:           860,
		MinWidth:         960,
		MinHeight:        640,
		BackgroundColour: application.NewRGB(248, 250, 252),
		URL:              "/",
	})

	return app.Run()
}

// marshalAppError guarantees structured *dto.AppError JSON reaches the Vue frontend cleanly.
func marshalAppError(err error) []byte {
	if err == nil {
		return nil
	}
	var appErr *dto.AppError
	if errors.As(err, &appErr) && appErr != nil {
		if data, mErr := json.Marshal(appErr); mErr == nil {
			return data
		}
	}
	fallback := dto.NewAppError(dto.ErrInternal, err.Error(), "")
	data, _ := json.Marshal(fallback)
	return data
}
