package dto

// ValueKind represents normalized database column value categories for fidelity rendering.
type ValueKind string

const (
	KindNull     ValueKind = "null"
	KindBool     ValueKind = "bool"
	KindInt      ValueKind = "int"
	KindBigInt   ValueKind = "bigint"   // Always transmitted as exact decimal string
	KindDecimal  ValueKind = "decimal"  // Always transmitted as exact decimal string
	KindFloat    ValueKind = "float"
	KindString   ValueKind = "string"
	KindDateTime ValueKind = "datetime" // Raw database datetime text, no implicit TZ shift
	KindJSON     ValueKind = "json"
	KindBinary   ValueKind = "binary"   // Hex preview + ByteLength + StreamRef
	KindArray    ValueKind = "array"
	KindEnum     ValueKind = "enum"
)

// CellValue preserves exact string representation for precision-sensitive types and
// strictly distinguishes NULL (Kind == KindNull) from empty string (Kind == KindString, Text == "").
type CellValue struct {
	Kind       ValueKind `json:"kind"`
	Text       string    `json:"text,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	ByteLength int64     `json:"byteLength,omitempty"`
	StreamRef  string    `json:"streamRef,omitempty"`
}

// ColumnMeta describes one column in a query or browse result set.
type ColumnMeta struct {
	Name         string    `json:"name"`
	DatabaseType string    `json:"databaseType"`
	Kind         ValueKind `json:"kind"`
	Nullable     bool      `json:"nullable"`
	IsPrimaryKey bool      `json:"isPrimaryKey,omitempty"`
}

// BrowseTableRequest requests a live page from a table (separate from Execute-Once SQL snapshots).
type BrowseTableRequest struct {
	ConnectionID string `json:"connectionId"`
	Database     string `json:"database"`
	Schema       string `json:"schema,omitempty"`
	Table        string `json:"table"`
	Page         int    `json:"page"`     // 1-based
	PageSize     int    `json:"pageSize"` // default 200, max 5000
	SortColumn   string `json:"sortColumn,omitempty"`
	SortDir      string `json:"sortDir,omitempty"` // asc | desc
	FilterWhere  string `json:"filterWhere,omitempty"`
}

// BrowseTableResponse returns a live-queried table page with explicit editability & sort metadata.
type BrowseTableResponse struct {
	Columns         []ColumnMeta     `json:"columns"`
	Rows            [][]CellValue    `json:"rows"`
	Page            int              `json:"page"`
	PageSize        int              `json:"pageSize"`
	HasMore         bool             `json:"hasMore"`
	TotalKnown      bool             `json:"totalKnown"`
	TotalRows       int64            `json:"totalRows"`
	DurationMs      int64            `json:"durationMs"`
	ExecutedSQL     string           `json:"executedSql"`
	StableOrderUsed bool             `json:"stableOrderUsed"`
	OrderColumns    []string         `json:"orderColumns,omitempty"`
	Editability     TableEditability `json:"editability"`
}

// ExecuteSQLRequest runs SQL inside a dedicated query tab session with caller-generated ExecID.
type ExecuteSQLRequest struct {
	TabID        string `json:"tabId"`
	ExecID       string `json:"execId"`
	ConnectionID string `json:"connectionId"`
	Database     string `json:"database,omitempty"`
	Schema       string `json:"schema,omitempty"`
	SQL          string `json:"sql"`
	CursorOffset int    `json:"cursorOffset"` // >=0 when RunMode == "current"
	RunMode      string `json:"runMode"`      // "current" | "all" | "selection"
	PageSize     int    `json:"pageSize"`     // initial page size returned from snapshot
}

// StatementResult represents the outcome of one executed statement in a query batch.
type StatementResult struct {
	Index            int          `json:"index"`
	SQL              string       `json:"sql"`
	StartLine        int          `json:"startLine"`
	IsResultSet      bool         `json:"isResultSet"`
	ResultID         string       `json:"resultId,omitempty"`
	Columns          []ColumnMeta `json:"columns,omitempty"`
	Rows             [][]CellValue `json:"rows,omitempty"`
	Page             int          `json:"page,omitempty"`
	PageSize         int          `json:"pageSize,omitempty"`
	TotalBuffered    int64        `json:"totalBuffered,omitempty"`
	HasMorePages     bool         `json:"hasMorePages,omitempty"`
	TruncatedByLimit bool         `json:"truncatedByLimit,omitempty"`
	LimitReason      string       `json:"limitReason,omitempty"`
	AffectedRows     int64        `json:"affectedRows"`
	DurationMs       int64        `json:"durationMs"`
	WarningMessage   string       `json:"warningMessage,omitempty"`
	Error            *AppError    `json:"error,omitempty"`
}

// ExecuteSQLResponse carries all statement results and the authoritative post-execution SessionState.
type ExecuteSQLResponse struct {
	TabID        string            `json:"tabId"`
	ExecID       string            `json:"execId"`
	CurrentDB    string            `json:"currentDb"`
	CurrentSchema string           `json:"currentSchema,omitempty"`
	SessionState SessionState      `json:"sessionState"`
	Statements   []StatementResult `json:"statements"`
	TotalMs      int64             `json:"totalMs"`
	Error        *AppError         `json:"error,omitempty"`
}

// FetchResultPageRequest reads a page from an Execute-Once snapshot WITHOUT re-running SQL.
type FetchResultPageRequest struct {
	ResultID string `json:"resultId"`
	Page     int    `json:"page"`     // 1-based
	PageSize int    `json:"pageSize"` // default 200
}

// FetchResultPageResponse returns a slice from an existing ResultBuffer snapshot.
type FetchResultPageResponse struct {
	ResultID         string        `json:"resultId"`
	Columns          []ColumnMeta  `json:"columns"`
	Rows             [][]CellValue `json:"rows"`
	Page             int           `json:"page"`
	PageSize         int           `json:"pageSize"`
	TotalBuffered    int64         `json:"totalBuffered"`
	HasMorePages     bool          `json:"hasMorePages"`
	TruncatedByLimit bool          `json:"truncatedByLimit"`
	LimitReason      string        `json:"limitReason,omitempty"`
}

// CancelExecutionResponse reports the exact outcome of a CancelExecution call.
type CancelExecutionResponse struct {
	TabID        string       `json:"tabId"`
	ExecID       string       `json:"execId"`
	Status       string       `json:"status"` // "canceled" | "noop" | "session_lost" | "pending_force"
	SessionState SessionState `json:"sessionState"`
	Message      string       `json:"message"`
}

// TabSessionInfo reports current dedicated session metadata for UI display.
type TabSessionInfo struct {
	TabID         string       `json:"tabId"`
	ConnectionID  string       `json:"connectionId"`
	CurrentDB     string       `json:"currentDb"`
	CurrentSchema string       `json:"currentSchema,omitempty"`
	SessionState  SessionState `json:"sessionState"`
	ServerConnID  string       `json:"serverConnId,omitempty"`
	ReadOnly      bool         `json:"readOnly"`
}

// QueryHistoryItem represents one entry in local query history.
type QueryHistoryItem struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	DatabaseName string `json:"databaseName"`
	SQLText      string `json:"sqlText"`
	Status       string `json:"status"`
	DurationMs   int64  `json:"durationMs"`
	AffectedRows int64  `json:"affectedRows"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ExecutedAt   int64  `json:"executedAt"`
}
