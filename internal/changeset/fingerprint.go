package changeset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

type schemaFingerprintPayload struct {
	Dialect            dto.EngineType            `json:"dialect"`
	Database           string                    `json:"database"`
	Schema             string                    `json:"schema"`
	Table              string                    `json:"table"`
	StorageEngine      string                    `json:"storageEngine"`
	WithoutRowID       bool                      `json:"withoutRowId"`
	Strict             bool                      `json:"strict"`
	TableDefinition    string                    `json:"tableDefinition"`
	CanWrite           bool                      `json:"canWrite"`
	BatchTransactional bool                      `json:"batchTransactional"`
	RowIdentity        dto.RowIdentityKind       `json:"rowIdentity"`
	KeyColumns         []string                  `json:"keyColumns"`
	Columns            []columnFingerprintRecord `json:"columns"`
}

type columnFingerprintRecord struct {
	Name                 string                 `json:"name"`
	NativeType           string                 `json:"nativeType"`
	ColumnType           string                 `json:"columnType"`
	Collation            string                 `json:"collation"`
	Deterministic        bool                   `json:"deterministic"`
	SQLiteAffinity       string                 `json:"sqliteAffinity"`
	Nullable             bool                   `json:"nullable"`
	HasDefault           bool                   `json:"hasDefault"`
	DefaultValue         string                 `json:"defaultValue"`
	AutoIncrement        bool                   `json:"autoIncrement"`
	GeneratedKind        string                 `json:"generatedKind"`
	GenerationExpression string                 `json:"generationExpression"`
	IdentityKind         string                 `json:"identityKind"`
	Insertable           bool                   `json:"insertable"`
	Updatable            bool                   `json:"updatable"`
	CompareStrategy      driver.CompareStrategy `json:"compareStrategy"`
	KeyComparable        bool                   `json:"keyComparable"`
}

// ComputeSchemaFingerprint computes a deterministic SHA-256 hex digest over every table and
// column write-semantic attribute, including full Detail.ColumnType (e.g. MySQL DECIMAL(38,10)
// vs DECIMAL(38,2), VARCHAR length, UNSIGNED), HasDefault, DefaultValue, AutoIncrement,
// GeneratedKind, GenerationExpression, IdentityKind, Collation, Deterministic, SQLiteAffinity,
// WithoutRowID, Strict, TableDefinition, CompareStrategy, and KeyComparable.
func ComputeSchemaFingerprint(ws driver.WriteSchema) string {
	cols := make([]driver.WriteColumn, len(ws.Columns))
	copy(cols, ws.Columns)
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].Detail.OrdinalPos != cols[j].Detail.OrdinalPos {
			return cols[i].Detail.OrdinalPos < cols[j].Detail.OrdinalPos
		}
		return cols[i].Detail.Name < cols[j].Detail.Name
	})

	colRecords := make([]columnFingerprintRecord, len(cols))
	for i, c := range cols {
		colRecords[i] = columnFingerprintRecord{
			Name:                 c.Detail.Name,
			NativeType:           c.NativeType,
			ColumnType:           c.Detail.ColumnType,
			Collation:            c.Collation,
			Deterministic:        c.Deterministic,
			SQLiteAffinity:       c.SQLiteAffinity,
			Nullable:             c.Detail.Nullable,
			HasDefault:           c.Detail.HasDefault,
			DefaultValue:         c.Detail.DefaultValue,
			AutoIncrement:        c.Detail.AutoIncrement,
			GeneratedKind:        c.Detail.GeneratedKind,
			GenerationExpression: c.GenerationExpression,
			IdentityKind:         c.Detail.IdentityKind,
			Insertable:           c.Detail.Insertable,
			Updatable:            c.Detail.Updatable,
			CompareStrategy:      c.CompareStrategy,
			KeyComparable:        c.KeyComparable,
		}
	}

	keyCols := make([]string, len(ws.Editability.KeyColumns))
	copy(keyCols, ws.Editability.KeyColumns)

	payload := schemaFingerprintPayload{
		Dialect:            ws.Dialect,
		Database:           ws.Database,
		Schema:             ws.Schema,
		Table:              ws.Table,
		StorageEngine:      ws.StorageEngine,
		WithoutRowID:       ws.WithoutRowID,
		Strict:             ws.Strict,
		TableDefinition:    ws.TableDefinition,
		CanWrite:           ws.Editability.CanWrite,
		BatchTransactional: ws.Editability.BatchTransactional,
		RowIdentity:        ws.Editability.RowIdentity,
		KeyColumns:         keyCols,
		Columns:            colRecords,
	}

	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
