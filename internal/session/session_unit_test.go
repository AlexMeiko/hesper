package session_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hesper/internal/driver"
	"hesper/internal/driver/fake"
	"hesper/internal/dto"
	"hesper/internal/session"
	"hesper/internal/sqlsplit"
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

// Pure unit test (mock driver/fake — NOT real DB acceptance):
// Verifies TabSession.ExecuteStatement routes parenthesized queries, EXPLAIN ANALYZE DML,
// MariaDB ANALYZE DML, and WITH ... SELECT ... FOR UPDATE through Query (preserving result sets),
// and enforces client read-only guardrails on MySQL executable comments and mutating ANALYZE statements.
func TestExecuteStatement_QueryRoutingAndReadOnlyGuardrail(t *testing.T) {
	ctx := context.Background()

	// 1. Verify Query path (IsResultSet == true && onStream invoked)
	mgrRW, _ := newFakeSessionManager(fake.FaultBehavior{})
	defer mgrRW.CloseAll()

	tsRW, err := mgrRW.GetOrCreateTabSession(ctx, "tab-route", "c-rw", "db1", "")
	if err != nil {
		t.Fatalf("open rw tab session: %v", err)
	}

	queryCases := []struct {
		name    string
		sql     string
		dialect sqlsplit.Dialect
	}{
		{"paren_union", "(SELECT 1) UNION (SELECT 2)", sqlsplit.DialectPostgres},
		{"paren_single", "(SELECT * FROM t)", sqlsplit.DialectMySQL},
		{"explain_analyze_delete", "EXPLAIN ANALYZE DELETE FROM t WHERE id = 1", sqlsplit.DialectPostgres},
		{"explain_parens_analyze_update", "EXPLAIN (ANALYZE, BUFFERS) UPDATE t SET a = 1 WHERE id = 1", sqlsplit.DialectPostgres},
		{"mariadb_analyze_delete", "ANALYZE DELETE FROM t WHERE id = 1", sqlsplit.DialectMySQL},
		{"with_select_for_update", "WITH c AS (SELECT 1) SELECT * FROM c FOR UPDATE", sqlsplit.DialectPostgres},
		{"with_cte_delete_then_select", "WITH d AS (DELETE FROM t WHERE id = 1) SELECT 1", sqlsplit.DialectPostgres},
	}

	for _, tc := range queryCases {
		t.Run("query_"+tc.name, func(t *testing.T) {
			stmts := sqlsplit.Split(tc.sql, tc.dialect)
			if len(stmts) != 1 {
				t.Fatalf("expected 1 statement, got %d", len(stmts))
			}
			streamCalled := false
			out := tsRW.ExecuteStatement(ctx, "exec-"+tc.name, stmts[0], func(s driver.RowStream) error {
				streamCalled = true
				for {
					_, ok, err := s.Next(nil)
					if err != nil || !ok {
						return err
					}
				}
			})
			if out.Error != nil {
				t.Fatalf("unexpected error: %+v", out.Error)
			}
			if !out.IsResultSet || !streamCalled {
				t.Fatalf("expected Query path (IsResultSet=true, streamCalled=true), got IsResultSet=%v streamCalled=%v",
					out.IsResultSet, streamCalled)
			}
		})
	}

	// 1b. Verify Exec path (IsResultSet == false && !streamCalled) so AffectedRows is preserved
	execCases := []struct {
		name    string
		sql     string
		dialect sqlsplit.Dialect
	}{
		{"update_returning_ident", "UPDATE t SET returning_flag = 1 WHERE id = 1", sqlsplit.DialectPostgres},
		{"cte_returning_outer_update", "WITH d AS (DELETE FROM t WHERE id = 1 RETURNING id) UPDATE audit SET done = 1 WHERE id IN (SELECT id FROM d)", sqlsplit.DialectPostgres},
	}
	for _, tc := range execCases {
		t.Run("exec_"+tc.name, func(t *testing.T) {
			st := sqlsplit.Split(tc.sql, tc.dialect)[0]
			streamCalled := false
			out := tsRW.ExecuteStatement(ctx, "exec-"+tc.name, st, func(s driver.RowStream) error {
				streamCalled = true
				return nil
			})
			if out.Error != nil {
				t.Fatalf("unexpected error: %+v", out.Error)
			}
			if out.IsResultSet || streamCalled {
				t.Fatalf("expected Exec path (IsResultSet=false, streamCalled=false), got IsResultSet=%v streamCalled=%v",
					out.IsResultSet, streamCalled)
			}
		})
	}

	// 2. Verify client read-only guardrail blocks executable comment DROP / SET escape / EXPLAIN ANALYZE / MariaDB ANALYZE DML / malformed WITH write
	regRO := driver.NewRegistry()
	regRO.Register(fake.NewOpener(fake.FaultBehavior{}))
	mgrRO := session.NewManager(regRO, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EngineFake,
			Database:     db,
			ReadOnly:     true,
		}, nil
	})
	defer mgrRO.CloseAll()

	tsRO, err := mgrRO.GetOrCreateTabSession(ctx, "tab-ro", "c-ro", "db1", "")
	if err != nil {
		t.Fatalf("open ro tab session: %v", err)
	}

	blockedCases := []struct {
		name    string
		sql     string
		dialect sqlsplit.Dialect
	}{
		{"exec_comment_drop", "/*! DROP */ TABLE t", sqlsplit.DialectMySQL},
		{"exec_comment_set_rw", "/*! SET */ TRANSACTION READ WRITE", sqlsplit.DialectMySQL},
		{"explain_analyze_delete", "EXPLAIN ANALYZE DELETE FROM t WHERE id = 1", sqlsplit.DialectPostgres},
		{"mariadb_analyze_delete", "ANALYZE DELETE FROM t WHERE id = 1", sqlsplit.DialectMySQL},
		{"malformed_with_delete", "WITH unrecognized_syntax DELETE FROM t", sqlsplit.DialectPostgres},
	}

	for _, tc := range blockedCases {
		t.Run("ro_block_"+tc.name, func(t *testing.T) {
			stmts := sqlsplit.Split(tc.sql, tc.dialect)
			if len(stmts) != 1 {
				t.Fatalf("expected 1 statement, got %d", len(stmts))
			}
			out := tsRO.ExecuteStatement(ctx, "ro-"+tc.name, stmts[0], nil)
			if out.Error == nil || out.Error.Code != dto.ErrReadOnlyDenied {
				t.Fatalf("expected ErrReadOnlyDenied, got %+v", out)
			}
		})
	}

	// Read-only session MUST still allow parenthesized UNION and WITH ... SELECT ... FOR UPDATE via Query
	for _, tc := range []struct {
		name    string
		sql     string
		dialect sqlsplit.Dialect
	}{
		{"ro_paren_union", "(SELECT 1) UNION (SELECT 2)", sqlsplit.DialectPostgres},
		{"ro_with_for_update", "WITH c AS (SELECT 1) SELECT * FROM c FOR UPDATE", sqlsplit.DialectPostgres},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := sqlsplit.Split(tc.sql, tc.dialect)[0]
			out := tsRO.ExecuteStatement(ctx, tc.name, st, func(s driver.RowStream) error {
				for {
					_, ok, err := s.Next(nil)
					if err != nil || !ok {
						return err
					}
				}
			})
			if out.Error != nil || !out.IsResultSet {
				t.Fatalf("expected read-only query to succeed with result set, got %+v", out)
			}
		})
	}
}

func TestTabSessionSplitErrorAndChainAndMySQLEnd(t *testing.T) {
	ctx := context.Background()

	// 1. SplitError guard + COMMIT/END/ROLLBACK AND CHAIN session state transitions.
	mgr, _ := newFakeSessionManager(fake.FaultBehavior{})
	defer mgr.CloseAll()

	ts, err := mgr.GetOrCreateTabSession(ctx, "tab-chain", "conn-chain", "demo", "")
	if err != nil {
		t.Fatalf("GetOrCreateTabSession: %v", err)
	}

	// Unclosed compound block with SplitError MUST be rejected without executing on driver!
	unclosed := sqlsplit.Split("CREATE FUNCTION f() RETURNS INT LANGUAGE SQL BEGIN ATOMIC SELECT 1;", sqlsplit.DialectPostgres)[0]
	outBlocked := ts.ExecuteStatement(ctx, "e-split-err", unclosed, nil)
	if outBlocked.Error == nil || outBlocked.Error.Code != dto.ErrInvalidArgument {
		t.Fatalf("expected ErrInvalidArgument for SplitError statement, got %+v", outBlocked)
	}

	// BEGIN -> InTx -> END AND CHAIN -> stays InTx -> ROLLBACK AND CHAIN -> stays InTx -> END WORK AND NO CHAIN -> Idle!
	if out := ts.ExecuteStatement(ctx, "e-begin", sqlsplit.Split("BEGIN", sqlsplit.DialectPostgres)[0], nil); out.Error != nil || out.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected InTx after BEGIN, got %+v", out)
	}
	if out := ts.ExecuteStatement(ctx, "e-end-chain", sqlsplit.Split("END AND CHAIN", sqlsplit.DialectPostgres)[0], nil); out.Error != nil || out.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected InTx after PG END AND CHAIN, got %+v", out)
	}
	if out := ts.ExecuteStatement(ctx, "e-rb-chain", sqlsplit.Split("ROLLBACK AND CHAIN", sqlsplit.DialectPostgres)[0], nil); out.Error != nil || out.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected InTx after PG ROLLBACK AND CHAIN, got %+v", out)
	}
	if out := ts.ExecuteStatement(ctx, "e-end-no-chain", sqlsplit.Split("END WORK AND NO CHAIN", sqlsplit.DialectPostgres)[0], nil); out.Error != nil || out.SessionState != dto.SessionAliveIdle {
		t.Fatalf("expected Idle after PG END WORK AND NO CHAIN, got %+v", out)
	}

	// 2. MySQL standalone END is KindOther (NOT KindCommit): wire drop after write MUST NOT report SessionCommitUnknown!
	dropMgr, _ := newFakeSessionManager(fake.FaultBehavior{DropAfterWrite: true})
	defer dropMgr.CloseAll()

	tsMy, err := dropMgr.GetOrCreateTabSession(ctx, "tab-my-end", "conn-my", "demo", "")
	if err != nil {
		t.Fatalf("GetOrCreateTabSession mysql: %v", err)
	}

	myEnd := sqlsplit.Split("END", sqlsplit.DialectMySQL)[0]
	outMyEnd := tsMy.ExecuteStatement(ctx, "e-my-end", myEnd, nil)
	if outMyEnd.Error == nil || outMyEnd.Error.Code == dto.ErrCommitOutcomeUnknown || outMyEnd.SessionState == dto.SessionCommitUnknown {
		t.Fatalf("MySQL standalone END must NOT enter commit-unknown tracking, got %+v", outMyEnd)
	}
}
