package binding

import (
	"context"

	"hesper/internal/dto"
	"hesper/internal/service"
)

// SchemaBinding is the Wails v3 boundary adapter for object explorer and metadata lookup.
type SchemaBinding struct {
	svc *service.SchemaService
}

func NewSchemaBinding(svc *service.SchemaService) *SchemaBinding {
	return &SchemaBinding{svc: svc}
}

func (b *SchemaBinding) ListDatabases(ctx context.Context, connID string) ([]dto.DatabaseInfo, error) {
	return b.svc.ListDatabases(ctx, connID)
}

func (b *SchemaBinding) ListSchemas(ctx context.Context, connID, db string) ([]string, error) {
	return b.svc.ListSchemas(ctx, connID, db)
}

func (b *SchemaBinding) ListTables(ctx context.Context, connID, db, schema string) ([]dto.TableSummary, error) {
	return b.svc.ListTables(ctx, connID, db, schema)
}

func (b *SchemaBinding) ListViews(ctx context.Context, connID, db, schema string) ([]dto.ObjectSummary, error) {
	return b.svc.ListViews(ctx, connID, db, schema)
}

func (b *SchemaBinding) ListSequences(ctx context.Context, connID, db, schema string) ([]dto.ObjectSummary, error) {
	return b.svc.ListSequences(ctx, connID, db, schema)
}

func (b *SchemaBinding) GetTableDetail(ctx context.Context, connID, db, schema, table string) (*dto.TableDetail, error) {
	return b.svc.GetTableDetail(ctx, connID, db, schema, table)
}

func (b *SchemaBinding) GetTableDDL(ctx context.Context, connID, db, schema, table string) (string, error) {
	return b.svc.GetTableDDL(ctx, connID, db, schema, table)
}

func (b *SchemaBinding) FetchCompletionCatalog(ctx context.Context, connID, db, schema string) (*dto.CompletionCatalog, error) {
	return b.svc.FetchCompletionCatalog(ctx, connID, db, schema)
}
