package binding

import (
	"context"

	"sql-manager/internal/dto"
	"sql-manager/internal/service"
)

// QueryBinding is the Wails v3 boundary adapter for live table browsing, Execute-Once SQL,
// snapshot pagination, bypass cancel, session lifecycle, and query history.
type QueryBinding struct {
	svc *service.QueryService
}

func NewQueryBinding(svc *service.QueryService) *QueryBinding {
	return &QueryBinding{svc: svc}
}

func (b *QueryBinding) OpenTabSession(ctx context.Context, tabID, connID, db, schema string) (dto.TabSessionInfo, error) {
	return b.svc.OpenTabSession(ctx, tabID, connID, db, schema)
}

func (b *QueryBinding) SwitchTabDatabase(ctx context.Context, tabID, targetDB string) (dto.TabSessionInfo, error) {
	return b.svc.SwitchTabDatabase(ctx, tabID, targetDB)
}

func (b *QueryBinding) SwitchTabSchema(ctx context.Context, tabID, targetSchema string) (dto.TabSessionInfo, error) {
	return b.svc.SwitchTabSchema(ctx, tabID, targetSchema)
}

func (b *QueryBinding) ReconnectTabSession(ctx context.Context, tabID, db, schema string) (dto.TabSessionInfo, error) {
	return b.svc.ReconnectTabSession(ctx, tabID, db, schema)
}

func (b *QueryBinding) CloseTab(tabID string) error {
	return b.svc.CloseTab(tabID)
}

func (b *QueryBinding) GetTabSessionInfo(tabID string) dto.TabSessionInfo {
	info, _ := b.svc.GetTabSessionInfo(tabID)
	return info
}

func (b *QueryBinding) BrowseTable(ctx context.Context, req dto.BrowseTableRequest) (*dto.BrowseTableResponse, error) {
	return b.svc.BrowseTable(ctx, req)
}

func (b *QueryBinding) ExecuteSQL(ctx context.Context, req dto.ExecuteSQLRequest) (*dto.ExecuteSQLResponse, error) {
	return b.svc.ExecuteSQL(ctx, req)
}

func (b *QueryBinding) FetchResultPage(req dto.FetchResultPageRequest) (*dto.FetchResultPageResponse, error) {
	return b.svc.FetchResultPage(req)
}

func (b *QueryBinding) CancelExecution(ctx context.Context, tabID, execID string) dto.CancelExecutionResponse {
	return b.svc.CancelExecution(ctx, tabID, execID)
}

func (b *QueryBinding) ForceAbortSession(tabID string) dto.TabSessionInfo {
	return b.svc.ForceAbortSession(tabID)
}

func (b *QueryBinding) ListHistory(ctx context.Context, connID string, limit int) ([]dto.QueryHistoryItem, error) {
	return b.svc.ListHistory(ctx, connID, limit)
}
