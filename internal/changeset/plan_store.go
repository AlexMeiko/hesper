package changeset

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"hesper/internal/dto"
)

const (
	defaultPlanReadyTTL        = 15 * time.Minute
	defaultMaxReadyPlans       = 256
	defaultMaxFinishedRetained = 512
)

// PlanStoreOptions configures TTL and capacity caps for PlanStore.
type PlanStoreOptions struct {
	ReadyTTL            time.Duration
	MaxReadyPlans       int
	MaxFinishedRetained int
	Now                 func() time.Time
}

type planEntry struct {
	id        string
	connID    string
	state     PlanState
	plan      *ChangePlan // cleared to nil immediately upon entering PlanFinished
	createdAt time.Time
	updatedAt time.Time

	// Lightweight terminal state retained after PlanFinished (when plan == nil).
	outcome dto.GridApplyOutcome
	summary ExecutionSummary
	err     *dto.AppError
}

// PlanStore is the single owner of grid ChangePlan execution state transitions:
//
//	Ready -> Preparing -> Executing -> Finished(Committed | NotCommitted | Unknown)
//	  ^          |
//	  +----------+ (ReleaseToReady: ONLY for whitelisted pre-tx recoverable errors)
//
// Invariants:
//   - A planID can transition into PlanExecuting AT MOST ONCE (whether within retention or after eviction).
//   - PlanPreparing and PlanExecuting entries are NEVER evicted.
//   - Entering PlanFinished immediately clears entry.plan = nil to release Steps/SQL/Args memory.
//   - Once in PlanFinished, subsequent MarkFinished / ReleaseToReady calls are no-ops and NEVER overwrite the terminal outcome.
type PlanStore struct {
	mu                  sync.Mutex
	entries             map[string]*planEntry
	readyTTL            time.Duration
	maxReadyPlans       int
	maxFinishedRetained int
	now                 func() time.Time
}

func NewPlanStore(opts PlanStoreOptions) *PlanStore {
	readyTTL := opts.ReadyTTL
	if readyTTL <= 0 {
		readyTTL = defaultPlanReadyTTL
	}
	maxReady := opts.MaxReadyPlans
	if maxReady <= 0 {
		maxReady = defaultMaxReadyPlans
	}
	maxFin := opts.MaxFinishedRetained
	if maxFin <= 0 {
		maxFin = defaultMaxFinishedRetained
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &PlanStore{
		entries:             make(map[string]*planEntry),
		readyTTL:            readyTTL,
		maxReadyPlans:       maxReady,
		maxFinishedRetained: maxFin,
		now:                 nowFn,
	}
}

// Create stores a newly previewed immutable ChangePlan in PlanReady state and returns a 32-byte random planID.
func (s *PlanStore) Create(plan *ChangePlan) string {
	id := generatePlanID()
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictExpiredReadyLocked(now)
	s.entries[id] = &planEntry{
		id:        id,
		connID:    plan.Source.ConnectionID,
		state:     PlanReady,
		plan:      plan,
		createdAt: now,
		updatedAt: now,
	}
	s.evictOverflowReadyLocked()
	return id
}

// ReserveForApply atomically claims planID for execution (CAS: Ready -> Preparing).
//   - If already Preparing or Executing: returns ErrInvalidArgument rejecting concurrent re-entry.
//   - If Finished (within retention): returns cached *TerminalSnapshot (never re-executes).
//   - If unknown, mismatched connection, or expired Ready: returns ErrPlanInvalidated.
func (s *PlanStore) ReserveForApply(planID, connID string) (*ChangePlan, *TerminalSnapshot, error) {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[planID]
	if !ok {
		return nil, nil, dto.NewAppError(dto.ErrPlanInvalidated, "变更计划不存在或已过期，请重新预览", planID)
	}
	if connID != "" && entry.connID != connID {
		return nil, nil, dto.NewAppError(dto.ErrPlanInvalidated, "变更计划与当前连接不匹配，请重新预览", planID)
	}

	switch entry.state {
	case PlanReady:
		if s.readyTTL > 0 && now.Sub(entry.createdAt) > s.readyTTL {
			delete(s.entries, planID)
			return nil, nil, dto.NewAppError(dto.ErrPlanInvalidated, "变更计划已过期，请重新预览", planID)
		}
		entry.state = PlanPreparing
		entry.updatedAt = now
		return entry.plan, nil, nil

	case PlanPreparing, PlanExecuting:
		return nil, nil, dto.NewAppError(dto.ErrInvalidArgument, "该变更计划正在提交中，请勿重复提交", planID)

	case PlanFinished:
		snap := &TerminalSnapshot{
			PlanID:  entry.id,
			Outcome: entry.outcome,
			Summary: entry.summary,
			Err:     cloneAppError(entry.err),
		}
		return nil, snap, nil

	default:
		return nil, nil, dto.NewAppError(dto.ErrPlanInvalidated, "变更计划状态异常，请重新预览", planID)
	}
}

// ReleaseToReady transitions Preparing -> Ready ONLY for whitelisted pre-tx recoverable failures
// (SSH host-key / auth challenge, or transient pre-tx dial errors while SourceRef remains valid).
// Calling ReleaseToReady in any other state (including Executing or Finished) is a safe no-op.
func (s *PlanStore) ReleaseToReady(planID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[planID]
	if !ok || entry.state != PlanPreparing {
		return false
	}
	entry.state = PlanReady
	entry.updatedAt = s.now()
	return true
}

// MarkExecuting crosses the point of no return (Preparing -> Executing).
// Once called, planID can NEVER return to Ready or enter Executing again.
func (s *PlanStore) MarkExecuting(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[planID]
	if !ok {
		return dto.NewAppError(dto.ErrPlanInvalidated, "变更计划不存在", planID)
	}
	if entry.state != PlanPreparing {
		return dto.NewAppError(dto.ErrPlanInvalidated, "变更计划状态不允许进入执行阶段", string(entry.state))
	}
	entry.state = PlanExecuting
	entry.updatedAt = s.now()
	return nil
}

// MarkFinished records a terminal outcome (Preparing | Executing -> Finished) and immediately
// drops entry.plan to release SQL/Args memory. If entry is already Finished, this is a no-op
// so defer cleanup guards can NEVER overwrite an authoritative Commit outcome.
func (s *PlanStore) MarkFinished(planID string, outcome dto.GridApplyOutcome, summary ExecutionSummary, appErr *dto.AppError) bool {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[planID]
	if !ok {
		return false
	}
	if entry.state == PlanFinished {
		return false
	}
	entry.state = PlanFinished
	entry.plan = nil // Release heavy Steps / SQL / Args immediately
	entry.outcome = outcome
	entry.summary = summary
	entry.err = cloneAppError(appErr)
	entry.updatedAt = now

	s.evictOverflowFinishedLocked()
	return true
}

// StateOf inspects the current state and whether the heavy plan payload is still resident (for tests/diagnostics).
func (s *PlanStore) StateOf(planID string) (state PlanState, hasPayload bool, exists bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[planID]
	if !ok {
		return "", false, false
	}
	return entry.state, entry.plan != nil, true
}

func (s *PlanStore) evictExpiredReadyLocked(now time.Time) {
	if s.readyTTL <= 0 {
		return
	}
	for id, e := range s.entries {
		if e.state == PlanReady && now.Sub(e.createdAt) > s.readyTTL {
			delete(s.entries, id)
		}
	}
}

func (s *PlanStore) evictOverflowReadyLocked() {
	var ready []*planEntry
	for _, e := range s.entries {
		if e.state == PlanReady {
			ready = append(ready, e)
		}
	}
	if len(ready) <= s.maxReadyPlans {
		return
	}
	sort.Slice(ready, func(i, j int) bool {
		return ready[i].updatedAt.Before(ready[j].updatedAt)
	})
	excess := len(ready) - s.maxReadyPlans
	for i := 0; i < excess; i++ {
		delete(s.entries, ready[i].id)
	}
}

// evictOverflowFinishedLocked enforces maxFinishedRetained by evicting non-Unknown finished
// records oldest-first, preserving Unknown outcomes as long as possible before evicting oldest Unknown.
// PlanPreparing and PlanExecuting entries are NEVER touched.
func (s *PlanStore) evictOverflowFinishedLocked() {
	var nonUnknown []*planEntry
	var unknown []*planEntry
	for _, e := range s.entries {
		if e.state != PlanFinished {
			continue
		}
		if e.outcome == dto.GridApplyUnknown {
			unknown = append(unknown, e)
		} else {
			nonUnknown = append(nonUnknown, e)
		}
	}
	total := len(nonUnknown) + len(unknown)
	if total <= s.maxFinishedRetained {
		return
	}
	excess := total - s.maxFinishedRetained

	sort.Slice(nonUnknown, func(i, j int) bool {
		return nonUnknown[i].updatedAt.Before(nonUnknown[j].updatedAt)
	})
	for i := 0; i < len(nonUnknown) && excess > 0; i++ {
		delete(s.entries, nonUnknown[i].id)
		excess--
	}
	if excess <= 0 {
		return
	}
	sort.Slice(unknown, func(i, j int) bool {
		return unknown[i].updatedAt.Before(unknown[j].updatedAt)
	})
	for i := 0; i < len(unknown) && excess > 0; i++ {
		delete(s.entries, unknown[i].id)
		excess--
	}
}

func generatePlanID() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failure should never happen on Linux/Windows/macOS; panic loudly instead of weak IDs.
		panic("crypto/rand unavailable for ChangePlan ID generation: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}

func cloneAppError(src *dto.AppError) *dto.AppError {
	if src == nil {
		return nil
	}
	cp := *src
	if src.HostKeyChallenge != nil {
		hk := *src.HostKeyChallenge
		cp.HostKeyChallenge = &hk
	}
	if src.SSHAuthPrompt != nil {
		ap := *src.SSHAuthPrompt
		cp.SSHAuthPrompt = &ap
	}
	return &cp
}
