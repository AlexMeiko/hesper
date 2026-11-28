package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"hesper/internal/changeset"
	"hesper/internal/driver"
	"hesper/internal/driver/fake"
	"hesper/internal/dto"
	"hesper/internal/entity"
	"hesper/internal/repository"
	"hesper/internal/service"
	"hesper/internal/session"
)

type fakeTimeoutNetErr struct{}

func (fakeTimeoutNetErr) Error() string   { return "dial tcp 127.0.0.1:3306: i/o timeout" }
func (fakeTimeoutNetErr) Timeout() bool   { return true }
func (fakeTimeoutNetErr) Temporary() bool { return true }

type fakeResolveControl struct {
	mu           sync.Mutex
	resolveError error
}

func (c *fakeResolveControl) setErr(err error) {
	c.mu.Lock()
	c.resolveError = err
	c.mu.Unlock()
}

func (c *fakeResolveControl) getErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolveError
}

func setupFakeChangeHarness(t *testing.T, behavior fake.FaultBehavior, envColor string) (
	*service.ChangeService,
	*fake.Opener,
	*session.Manager,
	*changeset.PlanStore,
	*atomic.Uint64,
	*fakeResolveControl,
) {
	t.Helper()
	dir := t.TempDir()
	repo, err := repository.OpenLocalDB(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatalf("open local db: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertConnection(context.Background(), entity.ConnectionEntity{
		ID:         "fake-conn",
		Name:       "Fake Test",
		Driver:     string(dto.EngineFake),
		EnvColor:   envColor,
		ConfigJSON: `{"host":"127.0.0.1","port":3306,"defaultDatabase":"demo"}`,
	}); err != nil {
		t.Fatalf("save conn: %v", err)
	}

	reg := driver.NewRegistry()
	opener := fake.NewOpener(behavior)
	reg.Register(opener)

	cfgVer := &atomic.Uint64{}
	cfgVer.Store(1)
	ctrl := &fakeResolveControl{}

	sessMgr := session.NewManager(reg, func(ctx context.Context, connID, db string) (driver.ResolvedConfig, error) {
		if rerr := ctrl.getErr(); rerr != nil {
			return driver.ResolvedConfig{}, rerr
		}
		return driver.ResolvedConfig{
			ConnectionID:  connID,
			Engine:        dto.EngineFake,
			Host:          "127.0.0.1",
			Port:          3306,
			Database:      db,
			ConfigVersion: cfgVer.Load(),
		}, nil
	})

	store := changeset.NewPlanStore(changeset.PlanStoreOptions{})
	svc := service.NewChangeService(sessMgr, repo, store)
	return svc, opener, sessMgr, store, cfgVer, ctrl
}

func TestChangeServiceSSHChallengeRetryExecutesInsertExactlyOnce(t *testing.T) {
	svc, opener, _, store, cfgVer, ctrl := setupFakeChangeHarness(t, fake.FaultBehavior{}, "red")

	// Preview succeeds (and reports IsProduction=true for red EnvColor).
	prev, err := svc.PreviewGridChanges(context.Background(), dto.PreviewGridChangesRequest{
		ConnectionID: "fake-conn",
		Database:     "demo",
		Table:        "users",
		Inserts: []dto.GridRowInsert{
			{RowKey: "ins-1", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "Alice"}}},
		},
	})
	if err != nil {
		t.Fatalf("PreviewGridChanges failed: %v", err)
	}
	if !prev.IsProduction || prev.InsertCount != 1 || prev.PlanID == "" {
		t.Fatalf("unexpected preview response: %+v", prev)
	}

	// Simulate SSH auth challenge on first Apply -> returns err (for runWithSSHChallenges) and releases Preparing -> Ready!
	ctrl.setErr(&dto.AppError{
		Code:    dto.ErrAuthFailed,
		Message: "需要输入 SSH 密码",
		SSHAuthPrompt: &dto.SSHAuthPrompt{
			ConnectionID: "fake-conn",
			AuthMethod:   "password",
			HostPort:     "bastion.internal:22",
			Username:     "ops",
		},
	})
	_, err = svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	})
	var ae *dto.AppError
	if !errors.As(err, &ae) || ae.Code != dto.ErrAuthFailed || ae.SSHAuthPrompt == nil {
		t.Fatalf("expected ErrAuthFailed + SSHAuthPrompt via error channel, got %v", err)
	}
	st, hasPayload, ok := store.StateOf(prev.PlanID)
	if !ok || st != changeset.PlanReady || !hasPayload {
		t.Fatalf("expected plan released back to Ready with payload intact, got st=%s payload=%v ok=%v", st, hasPayload, ok)
	}
	if len(opener.ExecutedStatements()) != 0 {
		t.Fatalf("expected 0 executed statements on SSH challenge, got %d", len(opener.ExecutedStatements()))
	}

	// Second Apply with SSH secret reuses SAME planID (configVer NOT bumped) and executes INSERT once!
	ctrl.setErr(nil)
	applyResp, err := svc.ApplyGridChangesWithSSHSecret(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	}, "ssh-pw", true)
	if err != nil {
		t.Fatalf("second Apply with SSH secret failed: %v", err)
	}
	if applyResp.Outcome != dto.GridApplyCommitted || applyResp.AppliedCount != 1 {
		t.Fatalf("expected Committed AppliedCount=1, got %+v", applyResp)
	}
	if cfgVer.Load() != 1 {
		t.Fatalf("expected configVer unchanged, got %d", cfgVer.Load())
	}
	if len(opener.ExecutedStatements()) != 1 {
		t.Fatalf("expected INSERT executed exactly once, got %d", len(opener.ExecutedStatements()))
	}

	// Third repeat Apply with SAME planID returns cached Committed without executing INSERT again!
	repeatResp, err := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	})
	if err != nil || repeatResp.Outcome != dto.GridApplyCommitted || repeatResp.AppliedCount != 1 {
		t.Fatalf("expected cached Committed on repeat Apply, got resp=%+v err=%v", repeatResp, err)
	}
	if len(opener.ExecutedStatements()) != 1 {
		t.Fatalf("repeat Apply MUST NOT re-execute INSERT, got %d statements", len(opener.ExecutedStatements()))
	}
}

func TestChangeServiceRetryWhitelistOnlyAllowsChallengesAndExplicitTimeouts(t *testing.T) {
	svc, opener, _, store, _, ctrl := setupFakeChangeHarness(t, fake.FaultBehavior{}, "green")

	t.Run("explicit dial timeout wrapped in AppError releases Preparing -> Ready for retry", func(t *testing.T) {
		prev, err := svc.PreviewGridChanges(context.Background(), dto.PreviewGridChangesRequest{
			ConnectionID: "fake-conn", Database: "demo", Table: "users",
			Inserts: []dto.GridRowInsert{{RowKey: "ins-to", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "RetryOK"}}}},
		})
		if err != nil {
			t.Fatalf("Preview failed: %v", err)
		}

		// Simulate driver returning ErrConnectionFailed wrapping a net.Error timeout.
		ctrl.setErr(dto.NewAppError(dto.ErrConnectionFailed, "拨号超时", "i/o timeout").WithCause(fakeTimeoutNetErr{}))
		_, err = svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
			ConnectionID: "fake-conn", PlanID: prev.PlanID,
		})
		if err == nil {
			t.Fatalf("expected timeout error via error channel for retry")
		}
		var aeTimeout *dto.AppError
		if !errors.As(err, &aeTimeout) || !aeTimeout.RetrySafe {
			t.Fatalf("expected recoverable dial timeout error to carry RetrySafe=true, got %+v", aeTimeout)
		}
		st, hasPayload, ok := store.StateOf(prev.PlanID)
		if !ok || st != changeset.PlanReady || !hasPayload {
			t.Fatalf("expected dial timeout to release plan back to Ready, got st=%s payload=%v ok=%v", st, hasPayload, ok)
		}

		// Subsequent retry with SAME planID succeeds!
		ctrl.setErr(nil)
		resp, err := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
			ConnectionID: "fake-conn", PlanID: prev.PlanID,
		})
		if err != nil || resp.Outcome != dto.GridApplyCommitted || resp.AppliedCount != 1 {
			t.Fatalf("expected retry after dial timeout to commit, got resp=%+v err=%v", resp, err)
		}
	})

	t.Run("non-timeout ErrConnectionFailed (e.g. missing SQLite file) finalizes NotCommitted instead of Ready", func(t *testing.T) {
		prev, err := svc.PreviewGridChanges(context.Background(), dto.PreviewGridChangesRequest{
			ConnectionID: "fake-conn", Database: "demo", Table: "users",
			Inserts: []dto.GridRowInsert{{RowKey: "ins-perm", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "NoRetry"}}}},
		})
		if err != nil {
			t.Fatalf("Preview failed: %v", err)
		}

		ctrl.setErr(dto.NewAppError(dto.ErrConnectionFailed, "SQLite 数据库文件不存在", "/tmp/deleted.db"))
		resp, err := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
			ConnectionID: "fake-conn", PlanID: prev.PlanID,
		})
		ctrl.setErr(nil)
		if err != nil {
			t.Fatalf("definitive non-recoverable NotCommitted must return (resp, nil) for Wails frontend, got err=%v", err)
		}
		if resp.Outcome != dto.GridApplyNotCommitted || resp.Error == nil || resp.Error.Code != dto.ErrConnectionFailed || resp.Error.RetrySafe {
			t.Fatalf("expected NotCommitted + ErrConnectionFailed (RetrySafe=false) in resp, got %+v", resp)
		}
		st, hasPayload, ok := store.StateOf(prev.PlanID)
		if !ok || st != changeset.PlanFinished || hasPayload {
			t.Fatalf("non-timeout ErrConnectionFailed MUST finalize to Finished (NOT Ready), got st=%s payload=%v ok=%v", st, hasPayload, ok)
		}
		if opener.Opened.Load() != opener.Closed.Load() {
			t.Fatalf("expected no leaked connections")
		}
	})
}

func TestChangeServiceBeginTableWriteFailureFinalizesNotCommitted(t *testing.T) {
	svc, opener, _, store, _, _ := setupFakeChangeHarness(t, fake.FaultBehavior{
		BeginWriteErr: dto.NewAppError(dto.ErrTxAborted, "lock wait timeout exceeded", "SQLITE_BUSY"),
	}, "green")

	prev, err := svc.PreviewGridChanges(context.Background(), dto.PreviewGridChangesRequest{
		ConnectionID: "fake-conn",
		Database:     "demo",
		Table:        "users",
		Inserts: []dto.GridRowInsert{
			{RowKey: "ins-1", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "Bob"}}},
		},
	})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}

	resp, err := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	})
	if err != nil {
		t.Fatalf("expected (resp, nil) on terminal BeginTableWrite failure so Wails frontend receives outcome, got err=%v", err)
	}
	if resp.Outcome != dto.GridApplyNotCommitted || resp.Error == nil || resp.Error.Code != dto.ErrTxAborted {
		t.Fatalf("expected NotCommitted + ErrTxAborted, got %+v", resp)
	}

	// Plan MUST be finalized (NOT stuck in Preparing) with payload dropped and lease released.
	st, hasPayload, ok := store.StateOf(prev.PlanID)
	if !ok || st != changeset.PlanFinished || hasPayload {
		t.Fatalf("expected Finished without payload after BeginTableWrite err, got st=%s payload=%v ok=%v", st, hasPayload, ok)
	}
	if opener.Opened.Load() != opener.Closed.Load() {
		t.Fatalf("expected lease closed after BeginTableWrite failure: opened=%d closed=%d", opener.Opened.Load(), opener.Closed.Load())
	}

	// Re-applying a Finished(NotCommitted) plan returns structured NotCommitted + ErrPlanInvalidated.
	resp2, err2 := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	})
	if err2 != nil || resp2.Outcome != dto.GridApplyNotCommitted || resp2.Error == nil || resp2.Error.Code != dto.ErrPlanInvalidated {
		t.Fatalf("expected (NotCommitted + ErrPlanInvalidated, nil) on repeat Apply, got resp=%+v err=%v", resp2, err2)
	}
}

func TestChangeServiceCommitPartialBytesUnknownSurvivesDisconnectAndNeverReexecutes(t *testing.T) {
	svc, opener, sessMgr, store, _, _ := setupFakeChangeHarness(t, fake.FaultBehavior{
		DropOnCommitPartialBytes: true,
	}, "green")

	prev, err := svc.PreviewGridChanges(context.Background(), dto.PreviewGridChangesRequest{
		ConnectionID: "fake-conn",
		Database:     "demo",
		Table:        "users",
		Inserts: []dto.GridRowInsert{
			{RowKey: "ins-unk", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "Mystery"}}},
		},
	})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}

	resp, err := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	})
	if err != nil {
		t.Fatalf("expected (resp, nil) for terminal GridApplyUnknown so Wails frontend receives outcome, got err=%v", err)
	}
	if resp.Outcome != dto.GridApplyUnknown || resp.Error == nil || resp.Error.Code != dto.ErrCommitOutcomeUnknown || resp.Error.RetrySafe {
		t.Fatalf("expected GridApplyUnknown + ERR_COMMIT_OUTCOME_UNKNOWN (RetrySafe=false), got %+v", resp)
	}
	st, hasPayload, ok := store.StateOf(prev.PlanID)
	if !ok || st != changeset.PlanFinished || hasPayload {
		t.Fatalf("expected Finished with dropped payload, got st=%s payload=%v ok=%v", st, hasPayload, ok)
	}

	// Disconnect (CloseConnection) MUST NOT erase Finished(Unknown) records from PlanStore!
	sessMgr.CloseConnection("fake-conn")

	// Calling Apply again with the same planID MUST return cached Unknown and NEVER re-execute!
	resp2, err2 := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	})
	if err2 != nil || resp2.Outcome != dto.GridApplyUnknown || resp2.Error == nil || resp2.Error.Code != dto.ErrCommitOutcomeUnknown || resp2.Error.RetrySafe {
		t.Fatalf("expected cached (Unknown, nil) after disconnect, got resp=%+v err=%v", resp2, err2)
	}
	if len(opener.ExecutedStatements()) != 1 {
		t.Fatalf("Unknown plan MUST NEVER re-execute, got %d statements", len(opener.ExecutedStatements()))
	}
}

func TestChangeServiceDisconnectInvalidatesReadyPlan(t *testing.T) {
	svc, opener, sessMgr, _, _, _ := setupFakeChangeHarness(t, fake.FaultBehavior{}, "green")

	prev, err := svc.PreviewGridChanges(context.Background(), dto.PreviewGridChangesRequest{
		ConnectionID: "fake-conn",
		Database:     "demo",
		Table:        "users",
		Inserts: []dto.GridRowInsert{
			{RowKey: "ins-disc", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "Late"}}},
		},
	})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}

	// Disconnect bumps LifecycleEpoch.
	sessMgr.CloseConnection("fake-conn")

	resp, err := svc.ApplyGridChanges(context.Background(), dto.ApplyGridChangesRequest{
		ConnectionID: "fake-conn",
		PlanID:       prev.PlanID,
	})
	if err != nil {
		t.Fatalf("expected (resp, nil) for terminal invalidated plan, got err=%v", err)
	}
	if resp.Outcome != dto.GridApplyNotCommitted || resp.Error == nil || resp.Error.Code != dto.ErrPlanInvalidated {
		t.Fatalf("expected NotCommitted + ErrPlanInvalidated after CloseConnection, got %+v", resp)
	}
	if len(opener.ExecutedStatements()) != 0 {
		t.Fatalf("expected 0 executed statements, got %d", len(opener.ExecutedStatements()))
	}
}
