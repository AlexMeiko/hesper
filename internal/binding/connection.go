package binding

import (
	"context"

	"sql-manager/internal/dto"
	"sql-manager/internal/service"
)

// ConnectionBinding is the Wails v3 boundary adapter for connection management.
type ConnectionBinding struct {
	svc *service.ConnectionService
}

func NewConnectionBinding(svc *service.ConnectionService) *ConnectionBinding {
	return &ConnectionBinding{svc: svc}
}

func (b *ConnectionBinding) ListConnections(ctx context.Context) ([]dto.ConnectionDTO, error) {
	return b.svc.ListConnections(ctx)
}

func (b *ConnectionBinding) SaveConnection(ctx context.Context, req dto.SaveConnectionRequest) (dto.ConnectionDTO, error) {
	return b.svc.SaveConnection(ctx, req)
}

func (b *ConnectionBinding) DeleteConnection(ctx context.Context, id string) error {
	return b.svc.DeleteConnection(ctx, id)
}

func (b *ConnectionBinding) TestConnection(ctx context.Context, req dto.SaveConnectionRequest) dto.TestConnectionResult {
	return b.svc.TestConnection(ctx, req)
}

func (b *ConnectionBinding) Connect(ctx context.Context, connID string) (dto.ConnectResult, error) {
	return b.svc.Connect(ctx, connID)
}

func (b *ConnectionBinding) Disconnect(connID string) {
	b.svc.Disconnect(connID)
}
