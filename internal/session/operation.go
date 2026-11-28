package session

import (
	"context"
	"fmt"
	"sync"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

type managedOp struct {
	id                 string
	connID             string
	lifecycleEpoch     uint64
	tunnelGen          uint64
	cancel             context.CancelFunc
	aborted            bool
	abortedByLifecycle bool
	abortedByTunnel    bool
}

type abortedOpHandle struct {
	cancel context.CancelFunc
}

// OperationLease owns one dedicated short-lived physical connection registered with session.Manager
// (used by transactional grid writes and schema previews). Callers MUST invoke Release() via defer.
//
// Connection ownership rule: OperationLease is the SOLE owner of l.conn.Close(). Disconnect / tunnel-loss
// handlers cancel l.ctx via op.cancel() so the driver's thread-safe context watcher interrupts active I/O
// (e.g. pgx contextWatcher, database/sql awaitDone, sqlite3_interrupt), and the lease-owning goroutine
// closes l.conn serially in Release() once the driver call returns — preventing concurrent Close() data races.
type OperationLease struct {
	mgr       *Manager
	op        *managedOp
	ctx       context.Context
	conn      driver.Conn
	cfg       driver.ResolvedConfig
	sourceRef driver.SourceRef
	once      sync.Once
}

func (l *OperationLease) Context() context.Context       { return l.ctx }
func (l *OperationLease) Conn() driver.Conn              { return l.conn }
func (l *OperationLease) ReadOnly() bool                 { return l.cfg.ReadOnly }
func (l *OperationLease) ResolvedConfig() driver.ResolvedConfig { return l.cfg }
func (l *OperationLease) SourceRef() driver.SourceRef    { return l.sourceRef }

// Release unregisters the managed operation from session.Manager, cancels its context,
// and closes the dedicated physical connection. Safe to call multiple times.
func (l *OperationLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		var cancelFn context.CancelFunc

		l.mgr.mu.Lock()
		if cur, ok := l.mgr.inFlightOps[l.op.id]; ok && cur == l.op {
			delete(l.mgr.inFlightOps, l.op.id)
		}
		cancelFn = l.op.cancel
		l.mgr.mu.Unlock()

		if cancelFn != nil {
			cancelFn()
		}
		if l.conn != nil {
			_ = l.conn.Close()
		}
	})
}

// CurrentLifecycleEpoch returns the current connection lifecycle epoch for connID.
func (m *Manager) CurrentLifecycleEpoch(connID string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connLifecycle[connID]
}

// AcquireOperation opens a dedicated, lifecycle-managed short connection for (connID, db).
//   - Preview passes expected == nil and obtains the authoritative SourceRef directly from lease.SourceRef().
//   - Apply passes expected == &plan.Source: AcquireOperation pre-registers a cancelable operation under m.mu
//     BEFORE dialing, verifies actual ResolvedConfig.ConfigVersion & ResolvedTarget outside m.mu, opens the
//     physical connection outside m.mu, and re-checks LifecycleEpoch & TunnelGen under m.mu before returning.
func (m *Manager) AcquireOperation(
	ctx context.Context,
	connID string,
	db string,
	expected *driver.SourceRef,
) (*OperationLease, error) {
	if connID == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "连接 ID 不能为空", "")
	}
	if expected != nil && expected.ConnectionID != "" && expected.ConnectionID != connID {
		return nil, dto.NewAppError(dto.ErrPlanInvalidated, "变更计划归属连接不匹配，请重新预览", "")
	}

	// Step 1: Check lifecycle epoch and pre-register cancelable operation BEFORE any network I/O.
	opCtx, cancel := context.WithCancel(ctx)
	opID := fmt.Sprintf("op-%d", m.opSeq.Add(1))

	m.mu.Lock()
	epoch := m.connLifecycle[connID]
	if expected != nil && epoch != expected.LifecycleEpoch {
		m.mu.Unlock()
		cancel()
		return nil, dto.NewAppError(
			dto.ErrPlanInvalidated,
			"连接已断开或重建，当前变更计划已失效，请重新预览",
			fmt.Sprintf("expected_epoch=%d current_epoch=%d", expected.LifecycleEpoch, epoch),
		)
	}
	op := &managedOp{
		id:             opID,
		connID:         connID,
		lifecycleEpoch: epoch,
		cancel:         cancel,
	}
	m.inFlightOps[opID] = op
	m.mu.Unlock()

	cleanupPreBind := func() (abortedByLifecycle, abortedByTunnel bool) {
		m.mu.Lock()
		if cur, ok := m.inFlightOps[opID]; ok && cur == op {
			delete(m.inFlightOps, opID)
		}
		abortedByLifecycle = op.abortedByLifecycle
		abortedByTunnel = op.abortedByTunnel
		m.mu.Unlock()
		cancel()
		return abortedByLifecycle, abortedByTunnel
	}

	// Step 2: Resolve decrypted config & ConfigVersion OUTSIDE m.mu.
	cfg, err := m.resolve(opCtx, connID, db)
	if err != nil {
		abortedLife, abortedTun := cleanupPreBind()
		if abortedLife {
			if expected != nil {
				return nil, dto.NewAppError(dto.ErrPlanInvalidated, "连接在获取配置期间已断开，请重新预览", err.Error())
			}
			return nil, dto.NewAppError(dto.ErrSessionLost, "连接已断开", err.Error())
		}
		if abortedTun {
			return nil, dto.NewAppError(dto.ErrSessionLost, "SSH 隧道已断开", err.Error())
		}
		return nil, err
	}

	actualDB := normalizeResolvedDB(cfg.Engine, cfg.Database)

	// Step 3: Verify ConfigVersion and physical target match expected SourceRef BEFORE dialing.
	if expected != nil {
		if cfg.ConfigVersion != expected.ConfigVersion {
			cleanupPreBind()
			return nil, dto.NewAppError(
				dto.ErrPlanInvalidated,
				"连接配置在预览后已被修改，为确保目标安全已拒绝提交，请重新预览",
				fmt.Sprintf("expected_config_ver=%d current_config_ver=%d", expected.ConfigVersion, cfg.ConfigVersion),
			)
		}
		if reason := mismatchResolvedTarget(expected.Target, cfg, actualDB); reason != "" {
			cleanupPreBind()
			return nil, dto.NewAppError(
				dto.ErrPlanInvalidated,
				"连接实际目标与预览快照不一致，请重新预览",
				reason,
			)
		}
	}

	// Step 3b: Register non-zero cfg.TunnelGen under m.mu BEFORE dialing (and fail fast if lifecycle
	// or tunnel generation already invalidated) so MarkConnectionLost(lostGen) cancels an in-flight dial promptly.
	m.mu.Lock()
	preLifeChanged := op.abortedByLifecycle || m.connLifecycle[connID] != epoch
	preTunLost := op.abortedByTunnel || (cfg.TunnelGen > 0 && cfg.TunnelGen <= m.invalidatedTunnelGen[connID])
	if preLifeChanged || preTunLost || op.aborted {
		if cur, ok := m.inFlightOps[opID]; ok && cur == op {
			delete(m.inFlightOps, opID)
		}
		m.mu.Unlock()
		cancel()
		if preLifeChanged {
			if expected != nil {
				return nil, dto.NewAppError(dto.ErrPlanInvalidated, "连接已断开或重建，当前变更计划已失效，请重新预览", "")
			}
			return nil, dto.NewAppError(dto.ErrSessionLost, "连接已断开", "")
		}
		return nil, dto.NewAppError(dto.ErrSessionLost, "SSH 隧道已失效", "")
	}
	if cfg.TunnelGen > 0 {
		op.tunnelGen = cfg.TunnelGen
	}
	m.mu.Unlock()

	// Step 4: Dial dedicated physical connection OUTSIDE m.mu.
	conn, err := m.OpenTransient(opCtx, cfg)
	if err != nil {
		abortedLife, abortedTun := cleanupPreBind()
		if abortedLife {
			if expected != nil {
				return nil, dto.NewAppError(dto.ErrPlanInvalidated, "建连期间连接已断开，变更计划已失效，请重新预览", err.Error())
			}
			return nil, dto.NewAppError(dto.ErrSessionLost, "建连期间连接已断开", err.Error())
		}
		if abortedTun {
			return nil, dto.NewAppError(dto.ErrSessionLost, "建连期间 SSH 隧道已断开", err.Error())
		}
		return nil, err
	}

	// Step 5: Re-acquire m.mu and verify lifecycle epoch & tunnel generation did not invalidate during dial.
	m.mu.Lock()
	lifeChanged := op.abortedByLifecycle || m.connLifecycle[connID] != epoch
	tunLost := op.abortedByTunnel || (cfg.TunnelGen > 0 && cfg.TunnelGen <= m.invalidatedTunnelGen[connID])
	if lifeChanged || tunLost || op.aborted {
		if cur, ok := m.inFlightOps[opID]; ok && cur == op {
			delete(m.inFlightOps, opID)
		}
		m.mu.Unlock()
		cancel()
		_ = conn.Close()
		if lifeChanged {
			if expected != nil {
				return nil, dto.NewAppError(dto.ErrPlanInvalidated, "建连期间连接已断开或重建，变更计划已失效，请重新预览", "")
			}
			return nil, dto.NewAppError(dto.ErrSessionLost, "建连期间连接已断开", "")
		}
		return nil, dto.NewAppError(dto.ErrSessionLost, "建连期间 SSH 隧道已失效", "")
	}

	m.mu.Unlock()

	src := driver.SourceRef{
		ConnectionID:   connID,
		ConfigVersion:  cfg.ConfigVersion,
		LifecycleEpoch: epoch,
		Target: driver.ResolvedTarget{
			Engine:   cfg.Engine,
			Host:     cfg.Host,
			Port:     cfg.Port,
			FilePath: cfg.FilePath,
			Database: actualDB,
			ReadOnly: cfg.ReadOnly,
		},
	}

	return &OperationLease{
		mgr:       m,
		op:        op,
		ctx:       opCtx,
		conn:      conn,
		cfg:       cfg,
		sourceRef: src,
	}, nil
}

func normalizeResolvedDB(engine dto.EngineType, db string) string {
	if db != "" {
		return db
	}
	switch engine {
	case dto.EnginePostgres:
		return "postgres"
	case dto.EngineSQLite:
		return "main"
	default:
		return ""
	}
}

func mismatchResolvedTarget(expected driver.ResolvedTarget, actual driver.ResolvedConfig, actualDB string) string {
	if expected.Engine != "" && actual.Engine != expected.Engine {
		return fmt.Sprintf("engine expected=%s actual=%s", expected.Engine, actual.Engine)
	}
	if actual.Host != expected.Host {
		return fmt.Sprintf("host expected=%s actual=%s", expected.Host, actual.Host)
	}
	if actual.Port != expected.Port {
		return fmt.Sprintf("port expected=%d actual=%d", expected.Port, actual.Port)
	}
	if actual.FilePath != expected.FilePath {
		return fmt.Sprintf("filePath expected=%s actual=%s", expected.FilePath, actual.FilePath)
	}
	if actual.ReadOnly != expected.ReadOnly {
		return fmt.Sprintf("readOnly expected=%v actual=%v", expected.ReadOnly, actual.ReadOnly)
	}
	if expected.Database != "" && actualDB != "" && !matchDBName(actual.Engine, actualDB, expected.Database) {
		return fmt.Sprintf("database expected=%s actual=%s", expected.Database, actualDB)
	}
	return ""
}

// detachOpsForConnectionLocked collects and detaches all inFlightOps for connID (caller holds m.mu).
func (m *Manager) detachOpsForConnectionLocked(connID string) []abortedOpHandle {
	var handles []abortedOpHandle
	for id, op := range m.inFlightOps {
		if op.connID != connID {
			continue
		}
		op.aborted = true
		op.abortedByLifecycle = true
		handles = append(handles, abortedOpHandle{cancel: op.cancel})
		delete(m.inFlightOps, id)
	}
	return handles
}

// detachOpsForTunnelLocked collects and detaches inFlightOps bound to lostGen (or all for connID when lostGen == 0).
// Caller holds m.mu.
func (m *Manager) detachOpsForTunnelLocked(connID string, lostGen uint64) []abortedOpHandle {
	var handles []abortedOpHandle
	for id, op := range m.inFlightOps {
		if op.connID != connID {
			continue
		}
		if lostGen > 0 && (op.tunnelGen == 0 || op.tunnelGen > lostGen) {
			continue
		}
		op.aborted = true
		op.abortedByTunnel = true
		handles = append(handles, abortedOpHandle{cancel: op.cancel})
		delete(m.inFlightOps, id)
	}
	return handles
}

func abortOpHandles(handles []abortedOpHandle) {
	for _, h := range handles {
		if h.cancel != nil {
			h.cancel()
		}
	}
}
