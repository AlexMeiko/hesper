package binding

import (
	"context"

	"sql-manager/internal/dto"
	"sql-manager/internal/service"
)

// ConnectionBinding is the Wails v3 boundary adapter for connection management.
type ConnectionBinding struct {
	svc            *service.ConnectionService
	pickSQLiteFile func() (string, error)
	pickLocalFile  func(kind string) (string, error)
}

func NewConnectionBinding(svc *service.ConnectionService) *ConnectionBinding {
	return &ConnectionBinding{svc: svc}
}

// AttachSQLiteFilePicker injects the Wails native OpenFile dialog callback from bootstrap
// without exposing a callback setter method over Wails RPC.
func AttachSQLiteFilePicker(b *ConnectionBinding, fn func() (string, error)) {
	b.pickSQLiteFile = fn
}

// AttachLocalFilePicker injects a general native OpenFile dialog callback supporting
// TLS certificates/keys and SSH private keys without exposing a callback setter over RPC.
func AttachLocalFilePicker(b *ConnectionBinding, fn func(kind string) (string, error)) {
	b.pickLocalFile = fn
}

func (b *ConnectionBinding) PickSQLiteFile() (string, error) {
	if b.pickSQLiteFile == nil {
		return "", dto.NewAppError(dto.ErrUnsupported, "当前运行模式不支持本地文件选择器，请手动输入文件路径", "")
	}
	return b.pickSQLiteFile()
}

func (b *ConnectionBinding) PickLocalFile(kind string) (string, error) {
	if b.pickLocalFile == nil {
		return "", dto.NewAppError(dto.ErrUnsupported, "当前运行模式不支持本地文件选择器，请手动输入文件路径", "")
	}
	return b.pickLocalFile(kind)
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

func (b *ConnectionBinding) TrustSSHHostKey(ctx context.Context, hostPort, algorithm, fingerprint string) error {
	return b.svc.TrustSSHHostKey(ctx, hostPort, algorithm, fingerprint)
}

func (b *ConnectionBinding) Connect(ctx context.Context, connID string) (dto.ConnectResult, error) {
	return b.svc.Connect(ctx, connID)
}

func (b *ConnectionBinding) ConnectWithSSHSecret(ctx context.Context, connID, sshSecret string, saveSecret bool) (dto.ConnectResult, error) {
	return b.svc.ConnectWithSSHSecret(ctx, connID, sshSecret, saveSecret)
}

func (b *ConnectionBinding) Disconnect(connID string) {
	b.svc.Disconnect(connID)
}
