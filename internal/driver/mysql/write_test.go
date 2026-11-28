package mysql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

func makeMySQLErr(num uint16, state string, msg string) *mysqldriver.MySQLError {
	var st [5]byte
	copy(st[:], state)
	return &mysqldriver.MySQLError{
		Number:   num,
		SQLState: st,
		Message:  msg,
	}
}

func TestClassifyMySQLCommitError(t *testing.T) {
	t.Run("nil error is Committed", func(t *testing.T) {
		res := ClassifyMySQLCommitError(nil, true)
		if res.Outcome != driver.CommitOutcomeCommitted || res.Err != nil {
			t.Fatalf("expected Committed, got %+v", res)
		}
	})

	t.Run("zero bytes written is NotCommitted", func(t *testing.T) {
		res := ClassifyMySQLCommitError(io.ErrUnexpectedEOF, false)
		if res.Outcome != driver.CommitOutcomeNotCommitted {
			t.Fatalf("expected NotCommitted when writeSent=false, got %+v", res)
		}
	})

	t.Run("sql.ErrTxDone is NotCommitted even when writeSent=true from auto-ROLLBACK", func(t *testing.T) {
		res := ClassifyMySQLCommitError(sql.ErrTxDone, true)
		if res.Outcome != driver.CommitOutcomeNotCommitted || res.Err == nil || res.Err.Code != dto.ErrTxAborted {
			t.Fatalf("expected NotCommitted + ErrTxAborted for sql.ErrTxDone, got %+v", res)
		}
	})

	notCommittedCases := []struct {
		name string
		err  error
	}{
		{name: "1062 duplicate entry", err: makeMySQLErr(1062, "23000", "Duplicate entry")},
		{name: "1205 lock wait timeout", err: makeMySQLErr(1205, "HY000", "Lock wait timeout exceeded")},
		{name: "1213 deadlock", err: makeMySQLErr(1213, "40001", "Deadlock found")},
		{name: "1290 server read_only", err: makeMySQLErr(1290, "HY000", "The MySQL server is running with --read-only")},
		{name: "1792 read-only tx", err: makeMySQLErr(1792, "25006", "Cannot execute statement in a READ ONLY transaction")},
		{name: "3819 check constraint", err: makeMySQLErr(3819, "HY000", "Check constraint violated")},
		{name: "4025 MariaDB constraint failed", err: makeMySQLErr(4025, "23000", "CONSTRAINT failed")},
		{name: "SQLSTATE 40001 rollback", err: makeMySQLErr(9999, "40001", "serialization failure")},
		{name: "SQLSTATE 40002 constraint rollback", err: makeMySQLErr(9999, "40002", "integrity rollback")},
		{name: "SQLSTATE 22001 data too long", err: makeMySQLErr(1406, "22001", "Data too long")},
	}
	for _, tc := range notCommittedCases {
		t.Run(tc.name+" is NotCommitted", func(t *testing.T) {
			res := ClassifyMySQLCommitError(tc.err, true)
			if res.Outcome != driver.CommitOutcomeNotCommitted {
				t.Fatalf("%s: expected NotCommitted, got %+v", tc.name, res)
			}
		})
	}

	unknownCases := []struct {
		name string
		err  error
	}{
		{name: "SQLSTATE 40003 statement_completion_unknown", err: makeMySQLErr(9999, "40003", "statement completion unknown")},
		{name: "1053 server shutdown", err: makeMySQLErr(1053, "08S01", "Server shutdown in progress")},
		{name: "1317 query interrupted", err: makeMySQLErr(1317, "70100", "Query execution was interrupted")},
		{name: "1927 connection killed", err: makeMySQLErr(1927, "70100", "Connection was killed")},
		{name: "2006 server gone", err: makeMySQLErr(2006, "HY000", "MySQL server has gone away")},
		{name: "2013 server lost", err: makeMySQLErr(2013, "HY000", "Lost connection to MySQL server during query")},
		{name: "3100 GR hook error", err: makeMySQLErr(3100, "HY000", "Error on observer while running replication hook")},
		{name: "3101 plugin commit uncertain", err: makeMySQLErr(3101, "HY000", "Plugin instructed the server to rollback")},
		{name: "SQLSTATE 08S01 comm link failure", err: makeMySQLErr(0, "08S01", "communication link failure")},
		{name: "ErrInvalidConn after write", err: mysqldriver.ErrInvalidConn},
		{name: "partial response EOF", err: io.ErrUnexpectedEOF},
		{name: "context canceled after write", err: context.Canceled},
	}
	for _, tc := range unknownCases {
		t.Run(tc.name+" is Unknown", func(t *testing.T) {
			res := ClassifyMySQLCommitError(tc.err, true)
			if res.Outcome != driver.CommitOutcomeUnknown {
				t.Fatalf("%s: expected Unknown, got %+v", tc.name, res)
			}
			if res.Err == nil || res.Err.Code != dto.ErrCommitOutcomeUnknown || res.Err.RetrySafe {
				t.Fatalf("%s: expected ERR_COMMIT_OUTCOME_UNKNOWN with RetrySafe=false, got %+v", tc.name, res.Err)
			}
		})
	}
}

type stubSQLConnector struct {
	tx *stubSQLTx
}

func (c *stubSQLConnector) Connect(context.Context) (sqldriver.Conn, error) {
	return &stubSQLConn{tx: c.tx}, nil
}
func (c *stubSQLConnector) Driver() sqldriver.Driver { return nil }

type stubSQLConn struct{ tx *stubSQLTx }

func (c *stubSQLConn) Prepare(string) (sqldriver.Stmt, error) { return nil, errors.New("unused") }
func (c *stubSQLConn) Close() error                           { return nil }
func (c *stubSQLConn) Begin() (sqldriver.Tx, error)           { return c.tx, nil }

type stubSQLTx struct {
	commitFn   func() error
	rollbackFn func() error
}

func (tx *stubSQLTx) Commit() error {
	if tx.commitFn != nil {
		return tx.commitFn()
	}
	return nil
}

func (tx *stubSQLTx) Rollback() error {
	if tx.rollbackFn != nil {
		return tx.rollbackFn()
	}
	return nil
}

func newStubMySQLWriteTx(t *testing.T, stx *stubSQLTx) (*mysqlTableWriteTx, *driver.TrackedConn) {
	t.Helper()
	clientPipe, serverPipe := net.Pipe()
	t.Cleanup(func() {
		_ = clientPipe.Close()
		_ = serverPipe.Close()
	})
	go func() {
		_, _ = io.Copy(io.Discard, serverPipe)
	}()
	tracked := driver.NewTrackedConn(clientPipe)

	db := sql.OpenDB(&stubSQLConnector{tx: stx})
	t.Cleanup(func() { _ = db.Close() })
	sqlTx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin stub tx: %v", err)
	}
	return &mysqlTableWriteTx{
		conn: &Conn{tracked: tracked},
		tx:   sqlTx,
	}, tracked
}

func TestMySQLTableWriteTxCommitWireSemanticsAndCleanup(t *testing.T) {
	t.Run("Commit returning context.Canceled AFTER writing wire bytes MUST return Unknown", func(t *testing.T) {
		var tracked *driver.TrackedConn
		stx := &stubSQLTx{
			commitFn: func() error {
				_, _ = tracked.Write([]byte("COMMIT"))
				return context.Canceled
			},
		}
		wtx, tc := newStubMySQLWriteTx(t, stx)
		tracked = tc

		res := wtx.Commit(context.Background())
		if res.Outcome != driver.CommitOutcomeUnknown {
			t.Fatalf("expected CommitOutcomeUnknown when bytes were written before context.Canceled, got %+v", res)
		}
		if res.Err == nil || res.Err.Code != dto.ErrCommitOutcomeUnknown || res.Err.RetrySafe {
			t.Fatalf("expected ERR_COMMIT_OUTCOME_UNKNOWN with RetrySafe=false, got %+v", res.Err)
		}
	})

	t.Run("Commit returning context.DeadlineExceeded with ZERO wire bytes returns NotCommitted", func(t *testing.T) {
		stx := &stubSQLTx{
			commitFn: func() error {
				return context.DeadlineExceeded
			},
		}
		wtx, _ := newStubMySQLWriteTx(t, stx)

		res := wtx.Commit(context.Background())
		if res.Outcome != driver.CommitOutcomeNotCommitted {
			t.Fatalf("expected CommitOutcomeNotCommitted when zero bytes written, got %+v", res)
		}
	})

	t.Run("Commit returning sql.ErrTxDone even after auto-ROLLBACK bytes returns NotCommitted", func(t *testing.T) {
		var tracked *driver.TrackedConn
		stx := &stubSQLTx{
			commitFn: func() error {
				_, _ = tracked.Write([]byte("ROLLBACK"))
				return sql.ErrTxDone
			},
		}
		wtx, tc := newStubMySQLWriteTx(t, stx)
		tracked = tc

		res := wtx.Commit(context.Background())
		if res.Outcome != driver.CommitOutcomeNotCommitted {
			t.Fatalf("expected CommitOutcomeNotCommitted on sql.ErrTxDone, got %+v", res)
		}
	})

	t.Run("pre-canceled Commit preserves failed cleanup rollback detail, keeps Rollback idempotent, and blocks second Commit", func(t *testing.T) {
		stx := &stubSQLTx{
			rollbackFn: func() error {
				return errors.New("temporary rollback failure")
			},
		}
		wtx, _ := newStubMySQLWriteTx(t, stx)

		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		res := wtx.Commit(canceledCtx)
		if res.Outcome != driver.CommitOutcomeNotCommitted {
			t.Fatalf("expected NotCommitted on pre-canceled Commit, got %+v", res)
		}
		if res.Err == nil || !strings.Contains(res.Err.Detail, "temporary rollback failure") {
			t.Fatalf("expected cleanup rollback failure preserved in Detail, got %+v", res.Err)
		}
		if wtx.done {
			t.Fatalf("expected t.done == false when pre-cancel cleanup rollback failed")
		}
		// Duplicate Commit MUST still be rejected.
		dupRes := wtx.Commit(context.Background())
		if dupRes.Outcome != driver.CommitOutcomeNotCommitted || dupRes.Err == nil || dupRes.Err.Code != dto.ErrTxAborted {
			t.Fatalf("expected duplicate Commit blocked even when cleanup failed, got %+v", dupRes)
		}
		// database/sql already flipped sql.Tx.done on the first rollback attempt, so a follow-up
		// wtx.Rollback receives sql.ErrTxDone from database/sql (without sending a second ROLLBACK to
		// the server), normalizes it to nil, and marks wtx.done = true so repeat cleanup stays error-free.
		if err := wtx.Rollback(context.Background()); err != nil {
			t.Fatalf("expected follow-up Rollback to normalize sql.ErrTxDone to nil, got %v", err)
		}
		if !wtx.done {
			t.Fatalf("expected t.done == true after follow-up Rollback normalized sql.ErrTxDone")
		}
	})
}

func TestIsMySQLDataLossWarning(t *testing.T) {
	cases := []struct {
		level string
		code  int
		want  bool
	}{
		{"Error", 1265, true},
		{"Error", 1062, true},
		{"Note", 1265, true},    // DECIMAL(10,0) fractional rounding in strict mode (e.g. 2.5 -> 3)
		{"note", 1265, true},    // case-insensitive level match
		{"Note", 1050, false},   // benign table-already-exists note
		{"Note", 1051, false},   // benign unknown-table note
		{"Note", 1008, false},   // benign drop-db note
		{"Warning", 1265, true}, // WARN_DATA_TRUNCATED
		{"Warning", 1264, true}, // ER_WARN_DATA_OUT_OF_RANGE
		{"Warning", 1406, true}, // ER_DATA_TOO_LONG
		{"Warning", 1292, true}, // ER_TRUNCATED_WRONG_VALUE
		{"Warning", 1366, true}, // ER_TRUNCATED_WRONG_VALUE_FOR_FIELD
		{"Warning", 1062, false},
	}
	for _, tc := range cases {
		if got := isMySQLDataLossWarning(tc.level, tc.code); got != tc.want {
			t.Errorf("isMySQLDataLossWarning(%q, %d) = %v, want %v", tc.level, tc.code, got, tc.want)
		}
	}
}
