package dto

// DatabaseInfo represents a database entry in the explorer tree.
type DatabaseInfo struct {
	Name      string `json:"name"`
	Charset   string `json:"charset,omitempty"`
	Collation string `json:"collation,omitempty"`
	IsSystem  bool   `json:"isSystem"`
}

// TableSummary represents a table entry in the explorer tree.
type TableSummary struct {
	Database      string `json:"database"`
	Schema        string `json:"schema,omitempty"`
	Name          string `json:"name"`
	Engine        string `json:"engine,omitempty"`
	EstimatedRows int64  `json:"estimatedRows"`
	Comment       string `json:"comment,omitempty"`
}

// ObjectSummary represents a view, sequence, or routine entry in the explorer tree.
type ObjectSummary struct {
	Database string `json:"database"`
	Schema   string `json:"schema,omitempty"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // view | matview | sequence | function | procedure
	Comment  string `json:"comment,omitempty"`
}

// ColumnDetail describes a table column's structural metadata.
type ColumnDetail struct {
	Name           string `json:"name"`
	DataType       string `json:"dataType"`
	ColumnType     string `json:"columnType"`
	Collation      string `json:"collation,omitempty"`
	Nullable       bool   `json:"nullable"`
	IsPrimaryKey   bool   `json:"isPrimaryKey"`
	DefaultValue   string `json:"defaultValue,omitempty"`
	HasDefault     bool   `json:"hasDefault"`
	AutoIncrement  bool   `json:"autoIncrement"`
	GeneratedKind  string `json:"generatedKind,omitempty"`  // "" | "virtual" | "stored"
	IdentityKind   string `json:"identityKind,omitempty"`   // "" | "always" | "by_default"
	Insertable     bool   `json:"insertable"`
	Updatable      bool   `json:"updatable"`
	ReadOnlyReason string `json:"readOnlyReason,omitempty"`
	Comment        string `json:"comment,omitempty"`
	OrdinalPos     int    `json:"ordinalPos"`
}

// IndexDetail describes a table index.
type IndexDetail struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Unique    bool     `json:"unique"`
	Primary   bool     `json:"primary"`
	IndexType string   `json:"indexType,omitempty"`
}

// TableDetail combines columns, indexes, storage engine, and computed editability.
type TableDetail struct {
	Database    string           `json:"database"`
	Schema      string           `json:"schema,omitempty"`
	Name        string           `json:"name"`
	Engine      string           `json:"engine,omitempty"`
	Comment     string           `json:"comment,omitempty"`
	Columns     []ColumnDetail   `json:"columns"`
	Indexes     []IndexDetail    `json:"indexes"`
	Editability TableEditability `json:"editability"`
}

// RowIdentityKind classifies how a row can be uniquely identified for future grid writes.
type RowIdentityKind string

const (
	RowIdentityNone          RowIdentityKind = "none"
	RowIdentityPrimaryKey    RowIdentityKind = "primary_key"
	RowIdentityUniqueNotNull RowIdentityKind = "unique_not_null"
)

// TableEditability captures the backend's 3-dimension editability assessment.
type TableEditability struct {
	CanWrite           bool            `json:"canWrite"`
	RowIdentity        RowIdentityKind `json:"rowIdentity"`
	KeyColumns         []string        `json:"keyColumns"`
	BatchTransactional bool            `json:"batchTransactional"`
	StorageEngine      string          `json:"storageEngine,omitempty"`
	Reasons            []string        `json:"reasons,omitempty"`
}

// CompletionCatalog provides lightweight schema metadata for CodeMirror 6 SQL autocomplete.
type CompletionCatalog struct {
	Database string              `json:"database"`
	Schema   string              `json:"schema,omitempty"`
	Tables   map[string][]string `json:"tables"` // tableName -> column names
}
