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
	"sql-manager/internal/driver/sqlite"
	"sql-manager/internal/dto"
	"sql-manager/internal/repository"
	"sql-manager/internal/resultbuf"
	"sql-manager/internal/secret"
	"sql-manager/internal/service"
	"sql-manager/internal/session"
	"sql-manager/internal/tunnel"
)

func init() {
	application.RegisterEvent[dto.SessionStateEvent]("session:state_changed")
}

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

	// 3. Static driver registry — all 4 Gate 1 engines explicitly registered.
	reg := driver.NewRegistry()
	reg.Register(mysql.NewOpener(dto.EngineMySQL))
	reg.Register(mysql.NewOpener(dto.EngineMariaDB))
	reg.Register(postgres.NewOpener())
	reg.Register(sqlite.NewOpener())

	// 4. Execute-Once result snapshot buffer (memory -> disk spill with hard cap + TTL).
	resMgr := resultbuf.NewManager(resultbuf.Options{
		SpillDir: paths.ResultDir,
	})

	// 5. Pure Go application services, dedicated-connection session manager, and SSH tunnel pool.
	connSvc := service.NewConnectionService(localDB, secretStore, nil, resMgr)
	sessMgr := session.NewManager(reg, connSvc.ResolveConfig)
	tunnelMgr := tunnel.NewManager(localDB, sessMgr.MarkConnectionLost)
	connSvc.SetSessionManager(sessMgr)
	connSvc.SetTunnelManager(tunnelMgr)

	schemaSvc := service.NewSchemaService(sessMgr)
	querySvc := service.NewQueryService(sessMgr, resMgr, localDB)
	querySvc.SetSSHSecretPreparer(connSvc.PrepareRuntimeSSHCredential)

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
		Description: "Lightweight cross-platform desktop database workbench for MySQL, PostgreSQL, MariaDB & SQLite",
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
			tunnelMgr.CloseAll()
			resMgr.CloseAll()
			_ = localDB.Close()
		},
	})

	sessMgr.SetOnStateChanged(func(evt dto.SessionStateEvent) {
		app.Event.Emit("session:state_changed", evt)
	})

	mainWin := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "SQL Manager",
		Width:            1360,
		Height:           860,
		MinWidth:         960,
		MinHeight:        640,
		BackgroundColour: application.NewRGB(248, 250, 252),
		URL:              "/",
	})

	binding.AttachSQLiteFilePicker(connBinding, func() (string, error) {
		return app.Dialog.OpenFile().
			AttachToWindow(mainWin).
			SetTitle("选择 SQLite 数据库文件").
			AddFilter("SQLite 数据库 (*.db, *.sqlite, *.sqlite3)", "*.db;*.sqlite;*.sqlite3;*.db3").
			AddFilter("所有文件 (*.*)", "*").
			PromptForSingleSelection()
	})

	binding.AttachLocalFilePicker(connBinding, func(kind string) (string, error) {
		dlg := app.Dialog.OpenFile().AttachToWindow(mainWin)
		switch kind {
		case "tls_ca":
			dlg.SetTitle("选择 TLS CA 证书文件").
				AddFilter("证书文件 (*.pem, *.crt, *.cer)", "*.pem;*.crt;*.cer")
		case "tls_cert":
			dlg.SetTitle("选择客户端 TLS 证书文件").
				AddFilter("证书文件 (*.pem, *.crt, *.cer)", "*.pem;*.crt;*.cer")
		case "tls_key":
			dlg.SetTitle("选择客户端 TLS 私钥文件").
				AddFilter("私钥文件 (*.key, *.pem)", "*.key;*.pem")
		case "ssh_key":
			dlg.SetTitle("选择 SSH 私钥文件").
				ShowHiddenFiles(true)
		default:
			dlg.SetTitle("选择文件")
		}
		return dlg.AddFilter("所有文件 (*.*)", "*").PromptForSingleSelection()
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
