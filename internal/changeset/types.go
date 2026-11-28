package changeset

import (
	"hesper/internal/driver"
	"hesper/internal/dto"
)

// Re-export driver source identity types for readability inside changeset callers.
type (
	ResolvedTarget = driver.ResolvedTarget
	SourceRef      = driver.SourceRef
)

// ChangeOp identifies the DML operation kind of one planned step.
type ChangeOp string

const (
	OpDelete ChangeOp = "delete"
	OpUpdate ChangeOp = "update"
	OpInsert ChangeOp = "insert"
)

// ChangeStep is one immutable, parameterized DML statement produced at Preview time
// and executed verbatim at Apply time.
type ChangeStep struct {
	Index      int
	Op         ChangeOp
	RowKey     string
	Stmt       driver.BoundStatement
	PreviewSQL string
	Params     []string
}

// ChangePlan holds the immutable preview output bound to one SourceRef and SchemaFingerprint.
// Execution state is owned exclusively by PlanStore, never stored on ChangePlan.
type ChangePlan struct {
	Source            SourceRef
	SchemaFingerprint string
	Steps             []ChangeStep
	DeleteCount       int
	UpdateCount       int
	InsertCount       int
}

// PlanState represents the single-owner lifecycle state machine in PlanStore:
//
//	Ready -> Preparing -> Executing -> Finished(Committed | NotCommitted | Unknown)
//	  ^          |
//	  +----------+ (whitelisted pre-tx recoverable failure only)
type PlanState string

const (
	PlanReady     PlanState = "ready"
	PlanPreparing PlanState = "preparing"
	PlanExecuting PlanState = "executing"
	PlanFinished  PlanState = "finished"
)

// ExecutionSummary retains lightweight outcome metadata after Finished,
// allowing PlanStore to drop heavy Steps/SQL/Args slices immediately.
type ExecutionSummary struct {
	AppliedCount   int
	DeleteCount    int
	UpdateCount    int
	InsertCount    int
	ConflictRowKey string
}

// TerminalSnapshot captures a cached Finished result returned on repeat Apply calls
// within retention (guaranteeing the same planID never enters Executing twice).
type TerminalSnapshot struct {
	PlanID  string
	Outcome dto.GridApplyOutcome
	Summary ExecutionSummary
	Err     *dto.AppError
}
