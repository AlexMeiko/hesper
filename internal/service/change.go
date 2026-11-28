package service

import (
	"context"
	"errors"
	"net"
	"strings"

	"hesper/internal/changeset"
	"hesper/internal/driver"
	"hesper/internal/dto"
	"hesper/internal/repository"
	"hesper/internal/session"
)

// ChangeService orchestrates transactional grid write preview & apply using:
//   - session.Manager.AcquireOperation (unified SourceRef, ConfigVersion, LifecycleEpoch)
//   - driver.TableWriteInspector / TableWriteStarter / TableWriteTx (atomic lock + in-tx schema + CommitResult)
//   - changeset.PlanStore (single-owner state machine: Ready -> Preparing -> Executing -> Finished)
type ChangeService struct {
	sessMgr          *session.Manager
	repo             *repository.LocalDB
	planStore        *changeset.PlanStore
	prepareSSHSecret func(ctx context.Context, connID string, carrier *runtimeSSHSecret)
}

func NewChangeService(sessMgr *session.Manager, repo *repository.LocalDB, planStore *changeset.PlanStore) *ChangeService {
	if planStore == nil {
		planStore = changeset.NewPlanStore(changeset.PlanStoreOptions{})
	}
	return &ChangeService{
		sessMgr:   sessMgr,
		repo:      repo,
		planStore: planStore,
	}
}

// SetSSHSecretPreparer registers ConnectionService.PrepareRuntimeSSHCredential so explicit saveSecret=true
// submissions persist independently without bumping ConfigVersion or tearing down live SSH tunnels.
func (s *ChangeService) SetSSHSecretPreparer(fn func(ctx context.Context, connID string, carrier *runtimeSSHSecret)) {
	s.prepareSSHSecret = fn
}

// PlanStore returns the underlying PlanStore (mainly for diagnostics and tests).
func (s *ChangeService) PlanStore() *changeset.PlanStore {
	return s.planStore
}

// PreviewGridChanges acquires a lifecycle-managed connection, reads the authoritative WriteSchema,
// builds an immutable parameterized ChangePlan bound to the lease's SourceRef, and stores it in PlanStore.
func (s *ChangeService) PreviewGridChanges(ctx context.Context, req dto.PreviewGridChangesRequest) (dto.PreviewGridChangesResponse, error) {
	return s.previewGridChangesInternal(ctx, req, nil)
}

// PreviewGridChangesWithSSHSecret previews grid changes supplying a runtime SSH password or passphrase.
func (s *ChangeService) PreviewGridChangesWithSSHSecret(
	ctx context.Context,
	req dto.PreviewGridChangesRequest,
	sshSecret string,
	saveSecret bool,
) (dto.PreviewGridChangesResponse, error) {
	reqCtx, carrier := withRuntimeSSHSecret(ctx, sshSecret, saveSecret)
	return s.previewGridChangesInternal(reqCtx, req, carrier)
}

func (s *ChangeService) previewGridChangesInternal(
	ctx context.Context,
	req dto.PreviewGridChangesRequest,
	carrier *runtimeSSHSecret,
) (dto.PreviewGridChangesResponse, error) {
	if req.ConnectionID == "" || strings.TrimSpace(req.Table) == "" {
		appErr := dto.NewAppError(dto.ErrInvalidArgument, "连接与表名不能为空", "")
		attachSaveOutcomeToError(appErr, carrier)
		return dto.PreviewGridChangesResponse{}, appErr
	}

	if s.prepareSSHSecret != nil {
		s.prepareSSHSecret(ctx, req.ConnectionID, carrier)
	}

	lease, err := s.sessMgr.AcquireOperation(ctx, req.ConnectionID, req.Database, nil)
	if err != nil {
		appErr := asAppError(err, dto.ErrConnectionFailed, "建立预览连接失败")
		attachSaveOutcomeToError(appErr, carrier)
		return dto.PreviewGridChangesResponse{}, appErr
	}
	defer lease.Release()

	inspector, ok := lease.Conn().(driver.TableWriteInspector)
	if !ok {
		appErr := dto.NewAppError(dto.ErrUnsupported, "当前数据库驱动暂不支持网格写入结构探测", string(lease.Conn().Dialect()))
		attachSaveOutcomeToError(appErr, carrier)
		return dto.PreviewGridChangesResponse{}, appErr
	}

	ws, err := inspector.InspectWriteSchema(lease.Context(), driver.WriteTarget{
		Database: req.Database,
		Schema:   req.Schema,
		Table:    req.Table,
	}, lease.ReadOnly())
	if err != nil {
		appErr := asAppError(err, dto.ErrInternal, "读取目标表写入结构失败")
		attachSaveOutcomeToError(appErr, carrier)
		return dto.PreviewGridChangesResponse{}, appErr
	}

	// Derive authoritative SourceRef directly from the Preview lease (never stitched separately).
	source := lease.SourceRef()
	if ws.Database != "" {
		source.Target.Database = ws.Database
	}
	source.Target.Schema = ws.Schema
	source.Target.Table = ws.Table

	plan, stepPreviews, err := changeset.BuildChangePlan(source, ws, req)
	if err != nil {
		appErr := asAppError(err, dto.ErrInvalidArgument, "生成变更计划失败")
		attachSaveOutcomeToError(appErr, carrier)
		return dto.PreviewGridChangesResponse{}, appErr
	}

	planID := s.planStore.Create(plan)
	isProd := false
	if s.repo != nil {
		if ent, rerr := s.repo.GetConnection(ctx, req.ConnectionID); rerr == nil && ent != nil && ent.EnvColor == "red" {
			isProd = true
		}
	}

	resp := dto.PreviewGridChangesResponse{
		PlanID:       planID,
		Steps:        stepPreviews,
		DeleteCount:  plan.DeleteCount,
		UpdateCount:  plan.UpdateCount,
		InsertCount:  plan.InsertCount,
		IsProduction: isProd,
	}
	if carrier != nil {
		resp.CredentialSaved = carrier.credentialSaved.Load()
		resp.SaveWarning = carrier.getSaveWarning()
	}
	return resp, nil
}

// ApplyGridChanges executes a previously previewed immutable ChangePlan at most once inside
// a dedicated, locked, short-lived transaction.
func (s *ChangeService) ApplyGridChanges(ctx context.Context, req dto.ApplyGridChangesRequest) (dto.ApplyGridChangesResponse, error) {
	return s.applyGridChangesInternal(ctx, req, nil)
}

// ApplyGridChangesWithSSHSecret executes a previously previewed ChangePlan supplying a runtime SSH secret.
// Reusing the same planID after an SSH host-key or auth challenge executes the plan at most once.
func (s *ChangeService) ApplyGridChangesWithSSHSecret(
	ctx context.Context,
	req dto.ApplyGridChangesRequest,
	sshSecret string,
	saveSecret bool,
) (dto.ApplyGridChangesResponse, error) {
	reqCtx, carrier := withRuntimeSSHSecret(ctx, sshSecret, saveSecret)
	return s.applyGridChangesInternal(reqCtx, req, carrier)
}

func (s *ChangeService) applyGridChangesInternal(
	ctx context.Context,
	req dto.ApplyGridChangesRequest,
	carrier *runtimeSSHSecret,
) (dto.ApplyGridChangesResponse, error) {
	if req.ConnectionID == "" || req.PlanID == "" {
		appErr := dto.NewAppError(dto.ErrInvalidArgument, "连接 ID 与变更计划 ID 不能为空", "")
		return dto.ApplyGridChangesResponse{}, appErr
	}

	// Atomically claim plan (CAS: Ready -> Preparing) or receive cached terminal snapshot BEFORE
	// persisting any runtime SSH credential so invalid, expired, or replayed plans never write secrets.
	plan, snap, err := s.planStore.ReserveForApply(req.PlanID, req.ConnectionID)
	if err != nil {
		appErr := asAppError(err, dto.ErrPlanInvalidated, "无法提交变更计划")
		return dto.ApplyGridChangesResponse{}, appErr
	}
	if snap != nil {
		return s.responseFromSnapshot(snap, nil)
	}

	var (
		lease      *session.OperationLease
		tx         driver.TableWriteTx
		released   bool
		finalized  bool
		commitSent bool
	)

	// Unified defer guard:
	//   1. Always roll back any uncommitted transaction before Commit is attempted.
	//   2. Always release the managed OperationLease (canceling & closing physical conn).
	//   3. Always finalize any lingering Preparing state as Finished(NotCommitted) so plans never stick,
	//      while strictly NEVER overwriting a terminal Finished outcome already written after Commit.
	defer func() {
		if tx != nil && !commitSent {
			_ = tx.Rollback(context.Background())
		}
		if lease != nil {
			lease.Release()
		}
		if !released && !finalized {
			s.planStore.MarkFinished(
				req.PlanID,
				dto.GridApplyNotCommitted,
				changeset.ExecutionSummary{},
				dto.NewAppError(dto.ErrTxAborted, "变更计划在提交前中止，未提交", ""),
			)
		}
	}()

	// Bind explicit SSH credential persistence strictly to the plan's original ConfigVersion BEFORE
	// acquiring the connection so edits to the connection/host after Preview never inherit the secret.
	if carrier != nil {
		carrier.expectedCfgVer = plan.Source.ConfigVersion
	}
	if s.prepareSSHSecret != nil {
		s.prepareSSHSecret(ctx, req.ConnectionID, carrier)
	}

	expected := plan.Source
	lease, err = s.sessMgr.AcquireOperation(ctx, req.ConnectionID, plan.Source.Target.Database, &expected)
	if err != nil {
		appErr := asAppError(err, dto.ErrConnectionFailed, "建立提交连接失败")
		attachSaveOutcomeToError(appErr, carrier)

		// ONLY whitelisted pre-tx recoverable failures release Preparing -> Ready and return err
		// so frontend runWithSSHChallenges can intercept SSH challenges / dial timeouts and retry the same planID.
		if isRecoverablePreTxError(err, appErr) {
			if s.planStore.ReleaseToReady(req.PlanID) {
				appErr.RetrySafe = true
				released = true
				return dto.ApplyGridChangesResponse{}, appErr
			}
			// Plan expired (ReadyTTL) or was evicted under MaxReadyPlans while preparing:
			// do NOT claim RetrySafe=true when the plan can no longer be retried.
			finalized = true
			invErr := dto.NewAppError(dto.ErrPlanInvalidated, "变更计划已过期或已失效，请重新预览", appErr.Detail)
			invErr.RetrySafe = false
			attachSaveOutcomeToError(invErr, carrier)
			return dto.ApplyGridChangesResponse{
				PlanID:          req.PlanID,
				Outcome:         dto.GridApplyNotCommitted,
				Error:           invErr,
				CredentialSaved: invErr.CredentialSaved,
				SaveWarning:     invErr.SaveWarning,
			}, nil
		}

		// Non-recoverable pre-tx failure (e.g. ConfigVersion/LifecycleEpoch invalidated, missing SQLite file,
		// auth/config error, context canceled): finalize as Finished(NotCommitted) and return structured response.
		appErr.RetrySafe = false
		s.planStore.MarkFinished(req.PlanID, dto.GridApplyNotCommitted, changeset.ExecutionSummary{}, appErr)
		finalized = true
		return dto.ApplyGridChangesResponse{
			PlanID:          req.PlanID,
			Outcome:         dto.GridApplyNotCommitted,
			Error:           appErr,
			CredentialSaved: appErr.CredentialSaved,
			SaveWarning:     appErr.SaveWarning,
		}, nil
	}

	starter, ok := lease.Conn().(driver.TableWriteStarter)
	if !ok {
		appErr := dto.NewAppError(dto.ErrUnsupported, "当前数据库驱动暂不支持事务网格写入", string(lease.Conn().Dialect()))
		attachSaveOutcomeToError(appErr, carrier)
		s.planStore.MarkFinished(req.PlanID, dto.GridApplyNotCommitted, changeset.ExecutionSummary{}, appErr)
		finalized = true
		return dto.ApplyGridChangesResponse{
			PlanID:          req.PlanID,
			Outcome:         dto.GridApplyNotCommitted,
			Error:           appErr,
			CredentialSaved: appErr.CredentialSaved,
			SaveWarning:     appErr.SaveWarning,
		}, nil
	}

	// BeginTableWrite atomically opens the transaction, locks the target table, and reads WriteSchema in-tx.
	// Any failure in BeginTableWrite is cleaned up inside the driver and finalized as Finished(NotCommitted).
	tx, err = starter.BeginTableWrite(lease.Context(), driver.WriteTarget{
		Database: plan.Source.Target.Database,
		Schema:   plan.Source.Target.Schema,
		Table:    plan.Source.Target.Table,
	}, lease.ReadOnly())
	if err != nil {
		appErr := asAppError(err, dto.ErrTxAborted, "开启写入事务并校验表结构失败")
		attachSaveOutcomeToError(appErr, carrier)
		s.planStore.MarkFinished(req.PlanID, dto.GridApplyNotCommitted, changeset.ExecutionSummary{}, appErr)
		finalized = true
		return dto.ApplyGridChangesResponse{
			PlanID:          req.PlanID,
			Outcome:         dto.GridApplyNotCommitted,
			Error:           appErr,
			CredentialSaved: appErr.CredentialSaved,
			SaveWarning:     appErr.SaveWarning,
		}, nil
	}

	// ExecutePlan checks schema fingerprint, crosses the irreversible Executing boundary, runs Deletes->Updates->Inserts,
	// and calls tx.Commit(ctx) returning the driver's authoritative CommitResult.
	execRes := changeset.ExecutePlan(lease.Context(), tx, plan, func() error {
		return s.planStore.MarkExecuting(req.PlanID)
	})
	commitSent = true // ExecutePlan either already rolled back before Commit or already called tx.Commit.

	if execRes.Err != nil {
		attachSaveOutcomeToError(execRes.Err, carrier)
	}
	s.planStore.MarkFinished(req.PlanID, execRes.Outcome, execRes.Summary, execRes.Err)
	finalized = true

	resp := dto.ApplyGridChangesResponse{
		PlanID:         req.PlanID,
		Outcome:        execRes.Outcome,
		AppliedCount:   execRes.Summary.AppliedCount,
		DeleteCount:    execRes.Summary.DeleteCount,
		UpdateCount:    execRes.Summary.UpdateCount,
		InsertCount:    execRes.Summary.InsertCount,
		ConflictRowKey: execRes.Summary.ConflictRowKey,
		Error:          execRes.Err,
	}
	if carrier != nil {
		resp.CredentialSaved = carrier.credentialSaved.Load()
		resp.SaveWarning = carrier.getSaveWarning()
	}

	// Return (resp, nil) for every definitive execution outcome (committed | not_committed | unknown)
	// so Wails v3 resolves the frontend Promise with full Outcome, ConflictRowKey, and Error fields.
	return resp, nil
}

func (s *ChangeService) responseFromSnapshot(
	snap *changeset.TerminalSnapshot,
	carrier *runtimeSSHSecret,
) (dto.ApplyGridChangesResponse, error) {
	switch snap.Outcome {
	case dto.GridApplyCommitted:
		resp := dto.ApplyGridChangesResponse{
			PlanID:       snap.PlanID,
			Outcome:      dto.GridApplyCommitted,
			AppliedCount: snap.Summary.AppliedCount,
			DeleteCount:  snap.Summary.DeleteCount,
			UpdateCount:  snap.Summary.UpdateCount,
			InsertCount:  snap.Summary.InsertCount,
		}
		if carrier != nil {
			resp.CredentialSaved = carrier.credentialSaved.Load()
			resp.SaveWarning = carrier.getSaveWarning()
		}
		return resp, nil

	case dto.GridApplyUnknown:
		appErr := snap.Err
		if appErr == nil {
			appErr = &dto.AppError{
				Code:      dto.ErrCommitOutcomeUnknown,
				Message:   "该变更计划上次提交结果未知，禁止重复执行，请先核对目标表数据状态",
				RetrySafe: false,
			}
		}
		appErr.RetrySafe = false
		appErr.CredentialSaved = false
		appErr.SaveWarning = ""
		attachSaveOutcomeToError(appErr, carrier)
		resp := dto.ApplyGridChangesResponse{
			PlanID:          snap.PlanID,
			Outcome:         dto.GridApplyUnknown,
			Error:           appErr,
			CredentialSaved: appErr.CredentialSaved,
			SaveWarning:     appErr.SaveWarning,
		}
		return resp, nil

	default: // dto.GridApplyNotCommitted
		appErr := dto.NewAppError(dto.ErrPlanInvalidated, "该变更计划已结束，请重新预览", snap.PlanID)
		attachSaveOutcomeToError(appErr, carrier)
		resp := dto.ApplyGridChangesResponse{
			PlanID:          snap.PlanID,
			Outcome:         dto.GridApplyNotCommitted,
			Error:           appErr,
			CredentialSaved: appErr.CredentialSaved,
			SaveWarning:     appErr.SaveWarning,
		}
		return resp, nil
	}
}

// isRecoverablePreTxError strictly whitelists pre-transaction AcquireOperation failures that may
// release Preparing -> Ready so the caller can retry the same planID:
//   1. SSH host-key confirmation challenge (ErrTunnelFailed + HostKeyChallenge)
//   2. SSH credential challenge (ErrAuthFailed + SSHAuthPrompt)
//   3. Explicit transient dial timeout (context.DeadlineExceeded or net.Error.Timeout())
//
// All other failures (missing SQLite file, non-timeout ErrConnectionFailed, SourceRef invalidation,
// read-only denial, context.Canceled, session loss) finalize immediately to Finished(NotCommitted).
func isRecoverablePreTxError(err error, appErr *dto.AppError) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if appErr != nil {
		if appErr.Code == dto.ErrTunnelFailed && appErr.HostKeyChallenge != nil {
			return true
		}
		if appErr.Code == dto.ErrAuthFailed && appErr.SSHAuthPrompt != nil {
			return true
		}
		switch appErr.Code {
		case dto.ErrPlanInvalidated, dto.ErrSessionLost, dto.ErrReadOnlyDenied,
			dto.ErrInvalidArgument, dto.ErrNotFound, dto.ErrUnsupported:
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}
