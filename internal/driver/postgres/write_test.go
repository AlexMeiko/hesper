package postgres

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

func TestClassifyPGCommitError(t *testing.T) {
	noopClassify := func(err error) *dto.AppError {
		if err == nil {
			return nil
		}
		return dto.NewAppError(dto.ErrInternal, err.Error(), err.Error())
	}

	t.Run("nil error is Committed", func(t *testing.T) {
		res := ClassifyPGCommitError(nil, true, noopClassify)
		if res.Outcome != driver.CommitOutcomeCommitted || res.Err != nil {
			t.Fatalf("expected Committed with nil err, got %+v", res)
		}
	})

	t.Run("pgx.ErrTxCommitRollback is NotCommitted even after writeSent", func(t *testing.T) {
		res := ClassifyPGCommitError(pgx.ErrTxCommitRollback, true, noopClassify)
		if res.Outcome != driver.CommitOutcomeNotCommitted {
			t.Fatalf("expected NotCommitted for pgx.ErrTxCommitRollback, got %+v", res)
		}
	})

	t.Run("zero bytes written on wire is NotCommitted", func(t *testing.T) {
		res := ClassifyPGCommitError(io.ErrUnexpectedEOF, false, noopClassify)
		if res.Outcome != driver.CommitOutcomeNotCommitted {
			t.Fatalf("expected NotCommitted when writeSent=false, got %+v", res)
		}
	})

	notCommittedCodes := []string{
		"22P02", // invalid_text_representation
		"23505", // unique_violation (e.g. DEFERRABLE INITIALLY DEFERRED on COMMIT)
		"23503", // foreign_key_violation
		"25P02", // in_failed_sql_transaction
		"40000", // transaction_rollback
		"40001", // serialization_failure
		"40002", // transaction_integrity_constraint_violation
		"40P01", // deadlock_detected
		"42501", // insufficient_privilege
		"44000", // with_check_option_violation
		"53100", // disk_full
		"55P03", // lock_not_available
	}
	for _, code := range notCommittedCodes {
		t.Run("whitelisted SQLSTATE "+code+" is NotCommitted", func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: code, Message: "server rejected commit"}
			res := ClassifyPGCommitError(pgErr, true, noopClassify)
			if res.Outcome != driver.CommitOutcomeNotCommitted {
				t.Fatalf("SQLSTATE %s: expected NotCommitted, got %+v", code, res)
			}
		})
	}

	unknownCases := []struct {
		name string
		err  error
	}{
		{name: "SQLSTATE 40003 statement_completion_unknown", err: &pgconn.PgError{Code: "40003", Message: "statement completion unknown"}},
		{name: "SQLSTATE 08006 connection_failure", err: &pgconn.PgError{Code: "08006", Message: "connection failure"}},
		{name: "SQLSTATE 08003 connection_does_not_exist", err: &pgconn.PgError{Code: "08003", Message: "connection does not exist"}},
		{name: "SQLSTATE 57P01 admin_shutdown", err: &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}},
		{name: "SQLSTATE 57P02 crash_shutdown", err: &pgconn.PgError{Code: "57P02", Message: "crash shutdown"}},
		{name: "SQLSTATE 57P03 cannot_connect_now", err: &pgconn.PgError{Code: "57P03", Message: "cannot connect now"}},
		{name: "SQLSTATE XX000 internal_error", err: &pgconn.PgError{Code: "XX000", Message: "internal error"}},
		{name: "partial response unexpected EOF", err: io.ErrUnexpectedEOF},
		{name: "context deadline exceeded after write", err: context.DeadlineExceeded},
		{name: "unclassified transport error", err: errors.New("tls: broken pipe after header")},
	}
	for _, tc := range unknownCases {
		t.Run(tc.name+" is Unknown", func(t *testing.T) {
			res := ClassifyPGCommitError(tc.err, true, noopClassify)
			if res.Outcome != driver.CommitOutcomeUnknown {
				t.Fatalf("%s: expected Unknown, got %+v", tc.name, res)
			}
			if res.Err == nil || res.Err.Code != dto.ErrCommitOutcomeUnknown || res.Err.RetrySafe {
				t.Fatalf("%s: expected ERR_COMMIT_OUTCOME_UNKNOWN with RetrySafe=false, got %+v", tc.name, res.Err)
			}
		})
	}
}
