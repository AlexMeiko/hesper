package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"hesper/internal/dto"
	"hesper/internal/repository"
	"hesper/internal/secret"
	"hesper/internal/service"
)

func TestConnectionService_ErrorCharsetValidation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	repo, err := repository.OpenLocalDB(filepath.Join(tmpDir, "app.db"))
	if err != nil {
		t.Fatalf("open local repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	svc := service.NewConnectionService(repo, secret.NewFileOnlyStore(tmpDir), nil, nil)

	// Invalid charset rejected by both SaveConnection and TestConnection.
	for _, bad := range []string{"GKB", "SQL_ASCII"} {
		req := dto.SaveConnectionRequest{
			Name:   "pg-bad",
			Driver: dto.EnginePostgres,
			Params: dto.ConnectionParams{Host: "127.0.0.1", Port: 5432, ErrorCharset: bad},
		}
		var ae *dto.AppError
		if _, err := svc.SaveConnection(ctx, req); !errors.As(err, &ae) || ae.Code != dto.ErrInvalidArgument {
			t.Fatalf("SaveConnection(%q) err=%v, want ErrInvalidArgument", bad, err)
		}
		if res := svc.TestConnection(ctx, req); res.Success || res.Error == nil || res.Error.Code != dto.ErrInvalidArgument {
			t.Fatalf("TestConnection(%q) = %+v, want ErrInvalidArgument", bad, res)
		}
	}

	// Valid PG charset trimmed & persisted through ResolveConfig.
	pg, err := svc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name:   "pg-ok",
		Driver: dto.EnginePostgres,
		Params: dto.ConnectionParams{Host: "127.0.0.1", Port: 5432, ErrorCharset: " GB2312 "},
	})
	if err != nil || pg.Params.ErrorCharset != "GB2312" {
		t.Fatalf("SaveConnection PG: err=%v charset=%q", err, pg.Params.ErrorCharset)
	}
	if cfg, err := svc.ResolveConfig(ctx, pg.ID, ""); err != nil || cfg.ErrorCharset != "GB2312" {
		t.Fatalf("ResolveConfig: err=%v charset=%q", err, cfg.ErrorCharset)
	}

	// Non-PostgreSQL drivers clear ErrorCharset instead of rejecting.
	my, err := svc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name:   "my-ok",
		Driver: dto.EngineMySQL,
		Params: dto.ConnectionParams{Host: "127.0.0.1", Port: 3306, ErrorCharset: "GKB"},
	})
	if err != nil || my.Params.ErrorCharset != "" {
		t.Fatalf("SaveConnection MySQL: err=%v charset=%q", err, my.Params.ErrorCharset)
	}
}
