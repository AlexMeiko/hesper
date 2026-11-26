package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/text/encoding/simplifiedchinese"

	"sql-manager/internal/dto"
)

type stubErrRows struct {
	pgx.Rows
	err error
}

func (s stubErrRows) Err() error { return s.err }

func TestRowsErrorClassifiesAndPreventsDoubleDecoding(t *testing.T) {
	c := &Conn{errorCharset: "GBK"}
	if got := c.rowsError(stubErrRows{}); got != nil {
		t.Fatalf("rowsError(nil) = %#v, want untyped nil", got)
	}

	// Non-PG errors (such as timeouts) must keep their original error chain for errors.Is.
	if got := c.rowsError(stubErrRows{err: context.DeadlineExceeded}); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("rowsError(DeadlineExceeded) = %v, want errors.Is(_, context.DeadlineExceeded)", got)
	}

	permUTF8 := `对表 users 权限不够`
	permGBK, _ := simplifiedchinese.GBK.NewEncoder().String(permUTF8)
	wrapped := fmt.Errorf("rows: %w", &pgconn.PgError{Code: "42501", Message: permGBK})

	rowErr := c.rowsError(stubErrRows{err: wrapped})
	ae, ok := rowErr.(*dto.AppError)
	if !ok || ae.Code != dto.ErrReadOnlyDenied || ae.Detail != permUTF8 {
		t.Fatalf("rowsError got %#v, want ErrReadOnlyDenied(%q)", rowErr, permUTF8)
	}
	if re := c.classifyError(rowErr); re.Detail != permUTF8 {
		t.Fatalf("re-classified Detail = %q, want %q", re.Detail, permUTF8)
	}
}
