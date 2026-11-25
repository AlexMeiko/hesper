package entity

// ConnectionGroupEntity represents a folder in the local app.db connection tree.
type ConnectionGroupEntity struct {
	ID        string
	ParentID  *string
	Name      string
	SortOrder int
	CreatedAt int64
}

// ConnectionEntity represents a persisted connection profile in local app.db.
// Secrets are never stored in plaintext here; SecretRef points to OS keyring / encrypted store.
type ConnectionEntity struct {
	ID         string
	GroupID    *string
	Name       string
	Driver     string
	EnvColor   string
	ReadOnly   bool
	ConfigJSON string
	SecretRef  string
	SortOrder  int
	CreatedAt  int64
	UpdatedAt  int64
}

// QueryHistoryEntity stores executed SQL audit history in local app.db.
type QueryHistoryEntity struct {
	ID           string
	ConnectionID string
	DatabaseName string
	SQLText      string
	Status       string
	DurationMs   int64
	AffectedRows int64
	ErrorCode    string
	ExecutedAt   int64
}

// SavedQueryEntity stores user-saved SQL snippets in local app.db.
type SavedQueryEntity struct {
	ID           string
	ConnectionID string
	Title        string
	Folder       string
	SQLText      string
	UpdatedAt    int64
}
