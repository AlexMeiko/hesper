package service_test

import (
	"context"
	"path/filepath"
	"testing"

	"sql-manager/internal/driver"
	"sql-manager/internal/driver/fake"
	"sql-manager/internal/dto"
	"sql-manager/internal/entity"
	"sql-manager/internal/repository"
	"sql-manager/internal/resultbuf"
	"sql-manager/internal/service"
	"sql-manager/internal/session"
)

func newQueryServiceFixture(t *testing.T) (*service.QueryService, *repository.LocalDB) {
	t.Helper()
	ctx := context.Background()
	tmpDir := t.TempDir()

	repo, err := repository.OpenLocalDB(filepath.Join(tmpDir, "app.db"))
	if err != nil {
		t.Fatalf("open local repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	for _, c := range []entity.ConnectionEntity{
		{ID: "prod-1", Name: "生产库", Driver: string(dto.EngineFake), EnvColor: "red", ConfigJSON: "{}"},
		{ID: "dev-1", Name: "开发库", Driver: string(dto.EngineFake), EnvColor: "green", ConfigJSON: "{}"},
		{ID: "ro-1", Name: "只读库", Driver: string(dto.EngineFake), EnvColor: "green", ReadOnly: true, ConfigJSON: "{}"},
	} {
		if err := repo.UpsertConnection(ctx, c); err != nil {
			t.Fatalf("save connection %s: %v", c.ID, err)
		}
	}

	reg := driver.NewRegistry()
	reg.Register(fake.NewOpener(fake.FaultBehavior{}))
	sessMgr := session.NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EngineFake,
			Database:     db,
			ReadOnly:     connID == "ro-1",
		}, nil
	})
	t.Cleanup(sessMgr.CloseAll)

	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: filepath.Join(tmpDir, "results")})
	t.Cleanup(resMgr.CloseAll)

	return service.NewQueryService(sessMgr, resMgr, repo), repo
}

// Pure unit test (mock driver/fake + local SQLite app.db — NOT real DB acceptance):
// Verifies QueryService.ExecuteSQL enforces backend single-source-of-truth confirmation
// on production (envColor == "red") connections for the exact parsed statement(s),
// enforces tab connection ownership, tracks ExecutedAll accurately, and fails closed.
func TestQueryService_ProductionHighRiskConfirmationGate(t *testing.T) {
	ctx := context.Background()
	sqlScript := "-- 中文备注\nSELECT 1;DELETE FROM orders"
	cursorAfterSemi := len("-- 中文备注\nSELECT 1;")

	issueTokenForDelete := func(t *testing.T, svc *service.QueryService) *dto.ExecuteSQLResponse {
		t.Helper()
		resp, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-1",
			ConnectionID: "prod-1",
			SQL:          sqlScript,
			CursorOffset: cursorAfterSemi,
			RunMode:      "current",
			PageSize:     50,
		})
		if err != nil {
			t.Fatalf("ExecuteSQL returned unexpected error: %v", err)
		}
		if !resp.RequiresConfirm || len(resp.HighRiskStatements) != 1 || resp.ConfirmToken == "" {
			t.Fatalf("expected RequiresConfirm with 1 statement and non-empty ConfirmToken, got: %+v", resp)
		}
		return resp
	}

	t.Run("cursor_highrisk_requires_token", func(t *testing.T) {
		svc, _ := newQueryServiceFixture(t)
		resp := issueTokenForDelete(t, svc)
		if resp.HighRiskStatements[0].Kind != "delete_no_where" || resp.HighRiskStatements[0].SQL != "DELETE FROM orders" {
			t.Fatalf("unexpected high-risk payload: %+v", resp.HighRiskStatements[0])
		}
		if len(resp.Statements) != 0 {
			t.Fatalf("statement must NOT execute prior to confirmation, got %d statements", len(resp.Statements))
		}
	})

	t.Run("mismatched_token_rejected", func(t *testing.T) {
		svc, _ := newQueryServiceFixture(t)
		issued := issueTokenForDelete(t, svc)

		respMismatch, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-mismatch",
			ConnectionID: "prod-1",
			SQL:          "DROP TABLE orders",
			RunMode:      "all",
			PageSize:     50,
			ConfirmToken: issued.ConfirmToken,
		})
		if err != nil {
			t.Fatalf("ExecuteSQL mismatch check returned unexpected error: %v", err)
		}
		if !respMismatch.RequiresConfirm || len(respMismatch.Statements) != 0 || respMismatch.ConfirmToken == "" || respMismatch.ConfirmToken == issued.ConfirmToken {
			t.Fatalf("mismatched ConfirmToken MUST NOT bypass confirmation, got: %+v", respMismatch)
		}
	})

	t.Run("valid_token_executes_partial_buffer", func(t *testing.T) {
		svc, _ := newQueryServiceFixture(t)
		issued := issueTokenForDelete(t, svc)

		respConfirmed, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-2",
			ConnectionID: "prod-1",
			SQL:          sqlScript,
			CursorOffset: cursorAfterSemi,
			RunMode:      "current",
			PageSize:     50,
			ConfirmToken: issued.ConfirmToken,
		})
		if err != nil {
			t.Fatalf("confirmed ExecuteSQL failed: %v", err)
		}
		if respConfirmed.RequiresConfirm {
			t.Fatalf("confirmed request must not re-prompt confirmation")
		}
		if respConfirmed.Error != nil {
			t.Fatalf("confirmed ExecuteSQL returned structured error: %+v", respConfirmed.Error)
		}
		if len(respConfirmed.Statements) != 1 || respConfirmed.Statements[0].SQL != "DELETE FROM orders" || respConfirmed.Statements[0].Error != nil {
			t.Fatalf("expected DELETE FROM orders executed without error, got %+v", respConfirmed.Statements)
		}
		if respConfirmed.ExecutedAll {
			t.Fatalf("ExecutedAll must be false when runMode=current executed only 1 of 2 statements")
		}
	})

	t.Run("safe_statement_no_confirm_and_single_statement_executed_all", func(t *testing.T) {
		svc, _ := newQueryServiceFixture(t)

		respSafe, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-3",
			ConnectionID: "prod-1",
			SQL:          sqlScript,
			CursorOffset: len("-- 中文备注\nSEL"),
			RunMode:      "current",
			PageSize:     50,
		})
		if err != nil {
			t.Fatalf("safe ExecuteSQL failed: %v", err)
		}
		if respSafe.RequiresConfirm || respSafe.Error != nil || respSafe.ExecutedAll ||
			len(respSafe.Statements) != 1 || respSafe.Statements[0].Error != nil {
			t.Fatalf("expected safe single-of-two execution without error and ExecutedAll=false, got %+v", respSafe)
		}

		respSingle, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-4",
			ConnectionID: "prod-1",
			SQL:          "-- 仅一条查询\nSELECT 1;",
			CursorOffset: 0,
			RunMode:      "current",
			PageSize:     50,
		})
		if err != nil || respSingle.Error != nil || !respSingle.ExecutedAll ||
			len(respSingle.Statements) != 1 || respSingle.Statements[0].Error != nil {
			t.Fatalf("expected single-statement runMode=current without error and ExecutedAll=true, err=%v resp=%+v", err, respSingle)
		}
	})

	t.Run("wrapped_highrisk_sql_requires_confirm", func(t *testing.T) {
		svc, _ := newQueryServiceFixture(t)

		respWrapped, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-wrapped",
			ConnectionID: "prod-1",
			SQL:          "/*! DROP */ TABLE orders",
			RunMode:      "all",
			PageSize:     50,
		})
		if err != nil || !respWrapped.RequiresConfirm || respWrapped.ConfirmToken == "" || len(respWrapped.Statements) != 0 {
			t.Fatalf("expected wrapped high-risk SQL to trigger production confirmation, err=%v resp=%+v", err, respWrapped)
		}
		if len(respWrapped.HighRiskStatements) != 1 || respWrapped.HighRiskStatements[0].Kind != "drop" {
			t.Fatalf("expected drop high-risk match, got %+v", respWrapped.HighRiskStatements)
		}
	})

	t.Run("connection_ownership_mismatch_rejected", func(t *testing.T) {
		svc, _ := newQueryServiceFixture(t)

		// Bind tab-1 to prod-1 first so this subtest does not rely on any prior subtest's session state
		respBind, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-bind",
			ConnectionID: "prod-1",
			SQL:          "SELECT 1",
			RunMode:      "all",
			PageSize:     50,
		})
		if err != nil || respBind.Error != nil {
			t.Fatalf("failed to bind tab-1 to prod-1: err=%v resp=%+v", err, respBind)
		}

		respSpoofed, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-spoof",
			ConnectionID: "dev-1",
			SQL:          "DROP TABLE orders",
			RunMode:      "all",
			PageSize:     50,
		})
		if err != nil {
			t.Fatalf("expected structured AppError response on connection mismatch, got: %v", err)
		}
		if respSpoofed.Error == nil || respSpoofed.Error.Code != dto.ErrInvalidArgument || len(respSpoofed.Statements) != 0 {
			t.Fatalf("mismatched tab ConnectionID MUST be rejected with ErrInvalidArgument, got: %+v", respSpoofed)
		}
	})

	t.Run("read_only_denied_not_executed_all", func(t *testing.T) {
		svc, _ := newQueryServiceFixture(t)

		respDenied, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-ro",
			ExecID:       "exec-ro-deny",
			ConnectionID: "ro-1",
			SQL:          "INSERT INTO orders VALUES (1)",
			RunMode:      "all",
			PageSize:     50,
		})
		if err != nil {
			t.Fatalf("unexpected raw error on read-only rejection: %v", err)
		}
		if respDenied.Error == nil || respDenied.Error.Code != dto.ErrReadOnlyDenied {
			t.Fatalf("expected ErrReadOnlyDenied, got: %+v", respDenied)
		}
		if respDenied.ExecutedAll {
			t.Fatalf("ExecutedAll MUST be false when statement execution fails/is denied")
		}
	})

	t.Run("fail_closed_on_repo_error", func(t *testing.T) {
		svc, repo := newQueryServiceFixture(t)
		_ = repo.Close()

		respFailClosed, err := svc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID:        "tab-1",
			ExecID:       "exec-closed",
			ConnectionID: "prod-1",
			SQL:          "DROP TABLE orders",
			RunMode:      "all",
			PageSize:     50,
		})
		if err != nil {
			t.Fatalf("expected structured AppError response instead of raw Go error, got: %v", err)
		}
		if respFailClosed.Error == nil || len(respFailClosed.Statements) != 0 {
			t.Fatalf("expected fail-closed refusal when GetConnection errors, got: %+v", respFailClosed)
		}
	})
}
