package binding

import (
	"context"

	"hesper/internal/dto"
	"hesper/internal/service"
)

// ChangeBinding is the thin Wails v3 boundary adapter for transactional grid write preview & apply.
type ChangeBinding struct {
	svc *service.ChangeService
}

func NewChangeBinding(svc *service.ChangeService) *ChangeBinding {
	return &ChangeBinding{svc: svc}
}

func (b *ChangeBinding) PreviewGridChanges(ctx context.Context, req dto.PreviewGridChangesRequest) (dto.PreviewGridChangesResponse, error) {
	return b.svc.PreviewGridChanges(ctx, req)
}

func (b *ChangeBinding) PreviewGridChangesWithSSHSecret(
	ctx context.Context,
	req dto.PreviewGridChangesRequest,
	sshSecret string,
	saveSecret bool,
) (dto.PreviewGridChangesResponse, error) {
	return b.svc.PreviewGridChangesWithSSHSecret(ctx, req, sshSecret, saveSecret)
}

func (b *ChangeBinding) ApplyGridChanges(ctx context.Context, req dto.ApplyGridChangesRequest) (dto.ApplyGridChangesResponse, error) {
	return b.svc.ApplyGridChanges(ctx, req)
}

func (b *ChangeBinding) ApplyGridChangesWithSSHSecret(
	ctx context.Context,
	req dto.ApplyGridChangesRequest,
	sshSecret string,
	saveSecret bool,
) (dto.ApplyGridChangesResponse, error) {
	return b.svc.ApplyGridChangesWithSSHSecret(ctx, req, sshSecret, saveSecret)
}
