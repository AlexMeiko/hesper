package dto

import "fmt"

// ErrorCode defines stable machine-readable error codes shared across Go services,
// Wails bindings, and the Vue frontend.
type ErrorCode string

const (
	ErrAuthFailed          ErrorCode = "ERR_AUTH_FAILED"
	ErrTunnelFailed        ErrorCode = "ERR_TUNNEL_FAILED"
	ErrConnectionFailed    ErrorCode = "ERR_CONNECTION_FAILED"
	ErrSQLSyntax           ErrorCode = "ERR_SQL_SYNTAX"
	ErrStatementCanceled   ErrorCode = "ERR_STATEMENT_CANCELED"
	ErrSessionLost         ErrorCode = "ERR_SESSION_LOST"
	ErrCommitOutcomeUnknown ErrorCode = "ERR_COMMIT_OUTCOME_UNKNOWN"
	ErrTxAborted           ErrorCode = "ERR_TX_ABORTED"
	ErrReadOnlyDenied      ErrorCode = "ERR_READ_ONLY_DENIED"
	ErrResultExpired       ErrorCode = "ERR_RESULT_EXPIRED"
	ErrConcurrencyConflict ErrorCode = "ERR_CONCURRENCY_CONFLICT"
	ErrRowNotIdentifiable  ErrorCode = "ERR_ROW_NOT_IDENTIFIABLE"
	ErrBatchNonTx          ErrorCode = "ERR_BATCH_NON_TRANSACTIONAL"
	ErrInvalidArgument     ErrorCode = "ERR_INVALID_ARGUMENT"
	ErrNotFound            ErrorCode = "ERR_NOT_FOUND"
	ErrUnsupported         ErrorCode = "ERR_UNSUPPORTED"
	ErrInternal            ErrorCode = "ERR_INTERNAL"
)

// AppError is a structured domain error carrying machine-readable code, user-facing message,
// raw database error details, and optional SQL position metadata.
type AppError struct {
	Code       ErrorCode    `json:"code"`
	Message    string       `json:"message"`
	Detail     string       `json:"detail,omitempty"`
	SQLState   string       `json:"sqlState,omitempty"`
	Line       int          `json:"line,omitempty"`
	Column     int          `json:"column,omitempty"`
	Position   int          `json:"position,omitempty"`
	RetrySafe  bool         `json:"retrySafe"`
	Session    SessionState `json:"sessionState,omitempty"`
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail != "" {
		return fmt.Sprintf("[%s] %s (%s)", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

func NewAppError(code ErrorCode, message string, detail string) *AppError {
	return &AppError{
		Code:    code,
		Message: message,
		Detail:  detail,
	}
}

// SessionState is the backend-authoritative physical session & transaction state.
// The frontend displays this directly and never guesses transaction state locally.
type SessionState string

const (
	SessionAliveIdle      SessionState = "alive_idle"
	SessionAliveInTx      SessionState = "alive_in_tx"
	SessionAliveFailedTx  SessionState = "alive_failed_tx"
	SessionLost           SessionState = "session_lost"
	SessionCommitUnknown  SessionState = "commit_outcome_unknown"
)
