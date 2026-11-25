package session_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"sql-manager/internal/driver"
	"sql-manager/internal/driver/fake"
	"sql-manager/internal/dto"
	"sql-manager/internal/session"
	"sql-manager/internal/sqlsplit"
)

func newFakeSessionManager(b fake.FaultBehavior) (*session.Manager, *fake.Opener) {
	reg := driver.NewRegistry()
	opener := fake.NewOpener(b)
	reg.Register(opener)
	mgr := session.NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EngineFake,
			Database:     db,
		}, nil
	})
	return mgr, opener
}

// Pure unit test (mock driver — NOT real MySQL acceptance):
// Verifies that when a cancel cannot be confirmed, TabSession stops reusing the connection
// and transitions to SessionLost rather than letting a queued query B run on an uncertain connection.
func TestUnconfirmedCancelMarksSessionLost(t *testing.T) {
	mgr, _ := newFakeSessionManager(fake.FaultBehavior{
		QueryDelay:      120 * time.Millisecond,
		CancelDelay:     40 * time.Millisecond,
		CancelConfirmed: false, // simulates failed/unconfirmed cancel
	})
	defer mgr.CloseAll()

	ctx := context.Background()
	ts, err := mgr.GetOrCreateTabSession(ctx, "tab-1", "c1", "db1", "")
	if err != nil {
		t.Fatalf("open tab session: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st := sqlsplit.Split("SELECT 1", sqlsplit.DialectMySQL)[0]
		_ = ts.ExecuteStatement(ctx, "exec-A", st, func(s driver.RowStream) error {
			for {
				_, ok, err := s.Next(nil)
				if err != nil || !ok {
					return err
				}
			}
		})
	}()

	time.Sleep(20 * time.Millisecond)
	cancelResp := ts.CancelExecution(ctx, "exec-A")
	wg.Wait()

	if cancelResp.Status != "session_lost" {
		t.Fatalf("expected session_lost cancel status, got %+v", cancelResp)
	}
	if ts.State() != dto.SessionLost {
		t.Fatalf("expected tab session state SessionLost, got %s", ts.State())
	}

	// Next queued statement B must be refused with ErrSessionLost instead of silently reusing connection.
	stB := sqlsplit.Split("SELECT 2", sqlsplit.DialectMySQL)[0]
	outB := ts.ExecuteStatement(ctx, "exec-B", stB, nil)
	if outB.Error == nil || outB.Error.Code != dto.ErrSessionLost {
		t.Fatalf("expected ErrSessionLost on subsequent execution, got %+v", outB)
	}
}

// Pure unit test (mock driver — NOT real MySQL acceptance):
// Verifies autocommit write or explicit COMMIT dropping after wire write triggers ErrCommitOutcomeUnknown.
func TestDropAfterWriteReturnsCommitOutcomeUnknown(t *testing.T) {
	mgr, _ := newFakeSessionManager(fake.FaultBehavior{
		DropAfterWrite: true,
	})
	defer mgr.CloseAll()

	ctx := context.Background()
	ts, err := mgr.GetOrCreateTabSession(ctx, "tab-w", "c1", "db1", "")
	if err != nil {
		t.Fatalf("open tab session: %v", err)
	}

	stmt := sqlsplit.Split("INSERT INTO t VALUES (1)", sqlsplit.DialectMySQL)[0]
	out := ts.ExecuteStatement(ctx, "exec-w1", stmt, nil)
	if out.Error == nil || out.Error.Code != dto.ErrCommitOutcomeUnknown {
		t.Fatalf("expected ErrCommitOutcomeUnknown, got %+v", out)
	}
	if out.Error.RetrySafe {
		t.Fatal("RetrySafe must be false for CommitOutcomeUnknown")
	}
	if ts.State() != dto.SessionCommitUnknown {
		t.Fatalf("expected SessionCommitUnknown, got %s", ts.State())
	}
}
