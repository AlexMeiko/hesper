package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

func TestClassifySQLiteCommitErrorLocalSemantics(t *testing.T) {
	t.Run("nil error is Committed", func(t *testing.T) {
		res := ClassifySQLiteCommitError(nil)
		if res.Outcome != driver.CommitOutcomeCommitted || res.Err != nil {
			t.Fatalf("expected Committed, got %+v", res)
		}
	})

	t.Run("context canceled or deadline exceeded is Unknown", func(t *testing.T) {
		for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
			res := ClassifySQLiteCommitError(err)
			if res.Outcome != driver.CommitOutcomeUnknown || res.Err == nil || res.Err.Code != dto.ErrCommitOutcomeUnknown || res.Err.RetrySafe {
				t.Fatalf("expected Unknown(RetrySafe=false) for %v, got %+v", err, res)
			}
		}
	})

	t.Run("unknown generic error is conservatively Unknown", func(t *testing.T) {
		res := ClassifySQLiteCommitError(errors.New("disk I/O failure"))
		if res.Outcome != driver.CommitOutcomeUnknown || res.Err == nil || res.Err.Code != dto.ErrCommitOutcomeUnknown || res.Err.RetrySafe {
			t.Fatalf("expected Unknown, got %+v", res)
		}
	})

	t.Run("real SQLite deferred FK constraint failure on COMMIT is NotCommitted", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "commit_fk.db")
		if err := os.WriteFile(dbPath, nil, 0600); err != nil {
			t.Fatalf("create db file: %v", err)
		}
		opener := NewOpener()
		rawConn, err := opener.Open(context.Background(), driver.ResolvedConfig{
			Engine:   dto.EngineSQLite,
			FilePath: dbPath,
		})
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		defer rawConn.Close()
		sc := rawConn.(*Conn)

		for _, ddl := range []string{
			`PRAGMA foreign_keys = ON`,
			`CREATE TABLE parent (id INTEGER PRIMARY KEY)`,
			`CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		} {
			if _, err := sc.conn.ExecContext(context.Background(), ddl); err != nil {
				t.Fatalf("setup ddl %q: %v", ddl, err)
			}
		}

		tx, err := sc.BeginTableWrite(context.Background(), driver.WriteTarget{Database: "main", Table: "child"}, false)
		if err != nil {
			t.Fatalf("BeginTableWrite: %v", err)
		}
		if _, err := tx.Exec(context.Background(), driver.BoundStatement{
			SQL:  `INSERT INTO "main"."child" ("id", "pid") VALUES (?, ?)`,
			Args: []any{int64(1), int64(999)}, // deferred FK violation fires at COMMIT!
		}); err != nil {
			t.Fatalf("deferred insert should not fail before COMMIT: %v", err)
		}

		commitRes := tx.Commit(context.Background())
		if commitRes.Outcome != driver.CommitOutcomeNotCommitted {
			t.Fatalf("expected NotCommitted on real SQLITE_CONSTRAINT_FOREIGNKEY during COMMIT, got %+v", commitRes)
		}
		// Calling tx.Rollback through the wrapper MUST succeed cleanly (never require raw ROLLBACK bypass).
		if err := tx.Rollback(context.Background()); err != nil {
			t.Fatalf("tx.Rollback after rejected COMMIT failed: %v", err)
		}
		// Duplicate Commit MUST still be rejected.
		dupRes := tx.Commit(context.Background())
		if dupRes.Outcome != driver.CommitOutcomeNotCommitted || dupRes.Err == nil || dupRes.Err.Code != dto.ErrTxAborted {
			t.Fatalf("expected duplicate Commit rejected, got %+v", dupRes)
		}

		// Key verification: WHILE connection #1 (sc) is STILL OPEN, a second connection MUST be able to
		// acquire BEGIN IMMEDIATE and commit a write without hitting SQLITE_BUSY!
		rawConn2, err := opener.Open(context.Background(), driver.ResolvedConfig{
			Engine:   dto.EngineSQLite,
			FilePath: dbPath,
		})
		if err != nil {
			t.Fatalf("open second sqlite conn: %v", err)
		}
		defer rawConn2.Close()
		sc2 := rawConn2.(*Conn)
		if _, err := sc2.conn.ExecContext(context.Background(), `BEGIN IMMEDIATE; INSERT INTO parent (id) VALUES (42); COMMIT;`); err != nil {
			t.Fatalf("second connection could not write while first connection remained open (write lock leaked): %v", err)
		}

		// Verify zero child rows committed.
		var cnt int
		if err := sc.conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM child`).Scan(&cnt); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("count child: %v", err)
		}
		if cnt != 0 {
			t.Fatalf("expected 0 committed rows after deferred FK failure, got %d", cnt)
		}
	})
}

func TestSQLitePreCanceledCommitPreservesCleanupFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cleanup_fail.db")
	if err := os.WriteFile(dbPath, nil, 0600); err != nil {
		t.Fatalf("create db file: %v", err)
	}
	rawConn, err := NewOpener().Open(context.Background(), driver.ResolvedConfig{
		Engine:   dto.EngineSQLite,
		FilePath: dbPath,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sc := rawConn.(*Conn)
	if _, err := sc.conn.ExecContext(context.Background(), `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	tx, err := sc.BeginTableWrite(context.Background(), driver.WriteTarget{Database: "main", Table: "t"}, false)
	if err != nil {
		t.Fatalf("BeginTableWrite: %v", err)
	}
	// Close the underlying conn so pre-canceled Commit's cleanup ROLLBACK fails deterministically.
	_ = sc.Close()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	res := tx.Commit(canceledCtx)
	if res.Outcome != driver.CommitOutcomeNotCommitted {
		t.Fatalf("expected NotCommitted on pre-canceled Commit, got %+v", res)
	}
	if res.Err == nil || !strings.Contains(res.Err.Detail, "cleanup rollback failed:") {
		t.Fatalf("expected cleanup rollback failure preserved in Detail, got %+v", res.Err)
	}
	stx := tx.(*sqliteTableWriteTx)
	if stx.done {
		t.Fatalf("expected t.done == false when pre-cancel cleanup rollback failed")
	}
	// Follow-up Rollback must NOT falsely return nil while cleanup failed.
	if err := tx.Rollback(context.Background()); err == nil {
		t.Fatalf("expected follow-up Rollback to surface error on closed conn, not silent nil")
	}
	// Duplicate Commit must still be rejected.
	dup := tx.Commit(context.Background())
	if dup.Outcome != driver.CommitOutcomeNotCommitted || dup.Err == nil || dup.Err.Code != dto.ErrTxAborted {
		t.Fatalf("expected duplicate Commit blocked, got %+v", dup)
	}
}
