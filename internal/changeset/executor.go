package changeset

import (
	"context"
	"errors"
	"fmt"
	"time"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

const rollbackTimeout = 3 * time.Second

func rollbackWithTimeout(tx driver.TableWriteTx) {
	if tx == nil {
		return
	}
	rbCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(rbCtx)
}

// ExecuteResult holds the definitive outcome of executing a ChangePlan inside a protected TableWriteTx.
type ExecuteResult struct {
	Outcome dto.GridApplyOutcome
	Summary ExecutionSummary
	Err     *dto.AppError
}

// ExecutePlan verifies transactional capability and schema fingerprint inside the locked TableWriteTx,
// crosses the irreversible Executing boundary via markExecuting, executes every immutable BoundStatement
// in order (Deletes -> Updates -> Inserts), checks AffectedRows == 1 on row-targeting statements,
// and commits using the driver's authoritative tri-state CommitResult.
//
// Invariants:
//   - Failures BEFORE tx.Commit(ctx) roll back and return Outcome = GridApplyNotCommitted.
//   - After tx.Commit(ctx) is called, ExecutePlan returns the driver's CommitResult verbatim
//     (Committed | NotCommitted | Unknown) without overwriting it.
func ExecutePlan(
	ctx context.Context,
	tx driver.TableWriteTx,
	plan *ChangePlan,
	markExecuting func() error,
) ExecuteResult {
	liveSchema := tx.Schema()

	if !liveSchema.Editability.CanWrite {
		rollbackWithTimeout(tx)
		return ExecuteResult{
			Outcome: dto.GridApplyNotCommitted,
			Err: dto.NewAppError(
				dto.ErrReadOnlyDenied,
				"目标表或连接当前不可写入，已中止并回滚事务",
				"",
			),
		}
	}

	if !liveSchema.Editability.BatchTransactional {
		rollbackWithTimeout(tx)
		return ExecuteResult{
			Outcome: dto.GridApplyNotCommitted,
			Err: dto.NewAppError(
				dto.ErrBatchNonTx,
				fmt.Sprintf("目标表存储引擎 (%s) 已不支持整批原子事务回滚，已拒绝执行", liveSchema.StorageEngine),
				"",
			),
		}
	}

	liveFP := ComputeSchemaFingerprint(liveSchema)
	if liveFP != plan.SchemaFingerprint {
		rollbackWithTimeout(tx)
		return ExecuteResult{
			Outcome: dto.GridApplyNotCommitted,
			Err: dto.NewAppError(
				dto.ErrSchemaChanged,
				"目标表结构或列属性在预览后已发生变化，为防止数据截断或语义漂移已拒绝提交，请重新刷新并预览",
				fmt.Sprintf("expected=%s live=%s", plan.SchemaFingerprint, liveFP),
			),
		}
	}

	if markExecuting != nil {
		if err := markExecuting(); err != nil {
			rollbackWithTimeout(tx)
			return ExecuteResult{
				Outcome: dto.GridApplyNotCommitted,
				Err:     asChangesetAppError(err, dto.ErrPlanInvalidated, "变更计划状态切换失败"),
			}
		}
	}

	applied := 0
	for _, step := range plan.Steps {
		fact, err := tx.Exec(ctx, step.Stmt)
		if err != nil {
			rollbackWithTimeout(tx)
			appErr := asChangesetAppError(err, dto.ErrInternal, fmt.Sprintf("执行第 %d 步 (%s) 失败，事务已回滚", step.Index+1, step.Op))
			appErr.ConflictRowKey = step.RowKey
			return ExecuteResult{
				Outcome: dto.GridApplyNotCommitted,
				Summary: ExecutionSummary{ConflictRowKey: step.RowKey},
				Err:     appErr,
			}
		}

		if step.Op == OpUpdate || step.Op == OpDelete {
			if fact.AffectedRows == 0 {
				rollbackWithTimeout(tx)
				appErr := dto.NewAppError(
					dto.ErrConcurrencyConflict,
					"检测到目标行已被其他会话修改或删除，整批变更已回滚，请刷新后核对冲突行",
					fmt.Sprintf("step=%d op=%s rowKey=%s affected=0", step.Index, step.Op, step.RowKey),
				)
				appErr.ConflictRowKey = step.RowKey
				return ExecuteResult{
					Outcome: dto.GridApplyNotCommitted,
					Summary: ExecutionSummary{ConflictRowKey: step.RowKey},
					Err:     appErr,
				}
			}
			if fact.AffectedRows > 1 {
				rollbackWithTimeout(tx)
				appErr := dto.NewAppError(
					dto.ErrRowNotIdentifiable,
					fmt.Sprintf("定位条件命中了 %d 行而非唯一 1 行，为防止误改多行已回滚整批变更", fact.AffectedRows),
					fmt.Sprintf("step=%d op=%s rowKey=%s affected=%d", step.Index, step.Op, step.RowKey, fact.AffectedRows),
				)
				appErr.ConflictRowKey = step.RowKey
				return ExecuteResult{
					Outcome: dto.GridApplyNotCommitted,
					Summary: ExecutionSummary{ConflictRowKey: step.RowKey},
					Err:     appErr,
				}
			}
		} else if step.Op == OpInsert && fact.AffectedRows != 1 {
			rollbackWithTimeout(tx)
			appErr := dto.NewAppError(
				dto.ErrTxAborted,
				fmt.Sprintf("单行新增实际影响 %d 行 (预期 1 行，可能被数据库触发器跳过或改写)，为保证结果一致已回滚整批变更", fact.AffectedRows),
				fmt.Sprintf("step=%d op=%s rowKey=%s affected=%d", step.Index, step.Op, step.RowKey, fact.AffectedRows),
			)
			appErr.ConflictRowKey = step.RowKey
			return ExecuteResult{
				Outcome: dto.GridApplyNotCommitted,
				Summary: ExecutionSummary{ConflictRowKey: step.RowKey},
				Err:     appErr,
			}
		}
		applied++
	}

	// Commit and trust the driver's authoritative tri-state CommitResult directly.
	commitRes := tx.Commit(ctx)
	switch commitRes.Outcome {
	case driver.CommitOutcomeCommitted:
		return ExecuteResult{
			Outcome: dto.GridApplyCommitted,
			Summary: ExecutionSummary{
				AppliedCount: applied,
				DeleteCount:  plan.DeleteCount,
				UpdateCount:  plan.UpdateCount,
				InsertCount:  plan.InsertCount,
			},
		}
	case driver.CommitOutcomeNotCommitted:
		appErr := commitRes.Err
		if appErr == nil {
			appErr = dto.NewAppError(dto.ErrTxAborted, "事务提交被数据库拒绝，未提交", "")
		}
		return ExecuteResult{
			Outcome: dto.GridApplyNotCommitted,
			Err:     appErr,
		}
	default: // driver.CommitOutcomeUnknown
		appErr := commitRes.Err
		if appErr == nil {
			appErr = &dto.AppError{
				Code:      dto.ErrCommitOutcomeUnknown,
				Message:   "提交结果未知，请先核对目标表数据状态，切勿盲目重试",
				RetrySafe: false,
			}
		}
		appErr.RetrySafe = false
		return ExecuteResult{
			Outcome: dto.GridApplyUnknown,
			Err:     appErr,
		}
	}
}

func asChangesetAppError(err error, fallbackCode dto.ErrorCode, fallbackMsg string) *dto.AppError {
	if err == nil {
		return nil
	}
	var ae *dto.AppError
	if errors.As(err, &ae) && ae != nil {
		return cloneAppError(ae)
	}
	return dto.NewAppError(fallbackCode, fmt.Sprintf("%s: %s", fallbackMsg, err.Error()), err.Error())
}
