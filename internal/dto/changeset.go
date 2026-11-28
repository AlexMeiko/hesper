package dto

// CellInputMode represents mutually exclusive user input modes for a written cell,
// eliminating ambiguous combinations such as IsNull && IsDefault.
type CellInputMode string

const (
	CellInputLiteral CellInputMode = "literal"
	CellInputNull    CellInputMode = "null"
	CellInputDefault CellInputMode = "default" // Allowed in INSERT to omit column / use DB default
)

// NewCellValue carries one user-supplied target cell value.
type NewCellValue struct {
	Mode CellInputMode `json:"mode"`
	Text string        `json:"text,omitempty"`
}

// OriginalCellValue carries the exact read-time database fact returned by BrowseTable,
// used strictly for stable-key targeting and modified-column concurrency verification.
type OriginalCellValue struct {
	Kind         ValueKind `json:"kind"`
	Text         string    `json:"text,omitempty"`
	Truncated    bool      `json:"truncated,omitempty"`
	StorageClass string    `json:"storageClass,omitempty"` // SQLite typeof: null|integer|real|text|blob
}

// GridRowDelete requests deleting one existing row identified by its full original stable key.
type GridRowDelete struct {
	RowKey    string                       `json:"rowKey"`
	KeyValues map[string]OriginalCellValue `json:"keyValues"`
}

// GridRowUpdate requests updating modified columns on one existing row.
// KeyValues holds the original stable key values; OriginalValues holds read-time facts for
// modified columns; NewValues holds the user-supplied target values.
type GridRowUpdate struct {
	RowKey         string                       `json:"rowKey"`
	KeyValues      map[string]OriginalCellValue `json:"keyValues"`
	OriginalValues map[string]OriginalCellValue `json:"originalValues"`
	NewValues      map[string]NewCellValue      `json:"newValues"`
}

// GridRowInsert requests inserting one new row. Columns omitted or marked CellInputDefault
// are skipped so the database fills defaults / auto-increment / generated values.
type GridRowInsert struct {
	RowKey string                  `json:"rowKey"`
	Values map[string]NewCellValue `json:"values"`
}

// PreviewGridChangesRequest requests building and storing an immutable parameterized ChangePlan.
type PreviewGridChangesRequest struct {
	ConnectionID string          `json:"connectionId"`
	Database     string          `json:"database"`
	Schema       string          `json:"schema,omitempty"`
	Table        string          `json:"table"`
	Deletes      []GridRowDelete `json:"deletes,omitempty"`
	Updates      []GridRowUpdate `json:"updates,omitempty"`
	Inserts      []GridRowInsert `json:"inserts,omitempty"`
}

// ChangeStepPreview displays one parameterized statement in the exact execution order
// (Deletes -> Updates -> Inserts) rendered from the immutable ChangePlan.
type ChangeStepPreview struct {
	Index      int      `json:"index"`
	Op         string   `json:"op"` // delete | update | insert
	RowKey     string   `json:"rowKey"`
	SQL        string   `json:"sql"`
	PreviewSQL string   `json:"previewSql"`
	Params     []string `json:"params,omitempty"`
}

// PreviewGridChangesResponse returns the server-issued one-time planID and exact preview SQL.
type PreviewGridChangesResponse struct {
	PlanID          string              `json:"planId"`
	Steps           []ChangeStepPreview `json:"steps"`
	DeleteCount     int                 `json:"deleteCount"`
	UpdateCount     int                 `json:"updateCount"`
	InsertCount     int                 `json:"insertCount"`
	IsProduction    bool                `json:"isProduction"`
	CredentialSaved bool                `json:"credentialSaved,omitempty"`
	SaveWarning     string              `json:"saveWarning,omitempty"`
}

// ApplyGridChangesRequest executes a previously previewed immutable plan by its planID.
type ApplyGridChangesRequest struct {
	ConnectionID string `json:"connectionId"`
	PlanID       string `json:"planId"`
}

// GridApplyOutcome reports the definitive backend outcome of applying a grid change plan.
type GridApplyOutcome string

const (
	GridApplyCommitted    GridApplyOutcome = "committed"
	GridApplyNotCommitted GridApplyOutcome = "not_committed"
	GridApplyUnknown      GridApplyOutcome = "unknown"
)

// ApplyGridChangesResponse reports the final result of ApplyGridChanges.
type ApplyGridChangesResponse struct {
	PlanID          string           `json:"planId"`
	Outcome         GridApplyOutcome `json:"outcome"`
	AppliedCount    int              `json:"appliedCount"`
	DeleteCount     int              `json:"deleteCount"`
	UpdateCount     int              `json:"updateCount"`
	InsertCount     int              `json:"insertCount"`
	ConflictRowKey  string           `json:"conflictRowKey,omitempty"`
	Error           *AppError        `json:"error,omitempty"`
	CredentialSaved bool             `json:"credentialSaved,omitempty"`
	SaveWarning     string           `json:"saveWarning,omitempty"`
}
