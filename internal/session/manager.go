package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hesper/internal/driver"
	"hesper/internal/dto"
	"hesper/internal/sqlsplit"
)

const (
	defaultMetaPoolConcurrency = 3
	defaultMetaPoolMaxIdle     = 1
	defaultMetaPoolIdleTTL     = 30 * time.Second
)

// ConfigResolver resolves decrypted ResolvedConfig for a given connectionID (and optional override database).
type ConfigResolver func(ctx context.Context, connID string, overrideDB string) (driver.ResolvedConfig, error)

// Manager owns both tracks of external database connections:
//   - Track A: Metadata & bypass-control pools per (connID, dbName)
//   - Track B: Dedicated physical TabSessions per open query tab
type Manager struct {
	registry *driver.Registry
	resolve  ConfigResolver

	epochSeq atomic.Int64
	opSeq    atomic.Uint64

	mu                   sync.Mutex
	metaMaxIdle          int
	metaIdleTTL          time.Duration
	metaPools            map[string]*metaPool   // key: connID + "\x00" + db
	tabs                 map[string]*TabSession // key: tabID
	invalidatedTunnelGen map[string]uint64      // highest invalidated SSH tunnel generation per connID
	connLifecycle        map[string]uint64      // monotonically increasing lifecycle epoch per connID
	inFlightOps          map[string]*managedOp  // active short-lived managed operations (incl. dialing)
	onStateChanged       func(dto.SessionStateEvent)
}

func NewManager(registry *driver.Registry, resolve ConfigResolver) *Manager {
	return &Manager{
		registry:             registry,
		resolve:              resolve,
		metaMaxIdle:          defaultMetaPoolMaxIdle,
		metaIdleTTL:          defaultMetaPoolIdleTTL,
		metaPools:            make(map[string]*metaPool),
		tabs:                 make(map[string]*TabSession),
		invalidatedTunnelGen: make(map[string]uint64),
		connLifecycle:        make(map[string]uint64),
		inFlightOps:          make(map[string]*managedOp),
	}
}

// SetMetaPoolPolicy adjusts Track A idle connection retention (mainly for tests).
func (m *Manager) SetMetaPoolPolicy(maxIdle int, idleTTL time.Duration) {
	if maxIdle < 0 {
		maxIdle = 0
	}
	if idleTTL < 0 {
		idleTTL = 0
	}
	m.mu.Lock()
	m.metaMaxIdle = maxIdle
	m.metaIdleTTL = idleTTL
	m.mu.Unlock()
}

// SetOnStateChanged registers a callback invoked outside session locks whenever a tab's
// authoritative SessionState changes asynchronously (e.g. tunnel disconnect).
func (m *Manager) SetOnStateChanged(fn func(dto.SessionStateEvent)) {
	m.mu.Lock()
	m.onStateChanged = fn
	m.mu.Unlock()
}

func (m *Manager) nextSessionEpoch() int64 {
	return m.epochSeq.Add(1)
}

func (m *Manager) emitStateChanged(evt dto.SessionStateEvent) {
	m.mu.Lock()
	fn := m.onStateChanged
	m.mu.Unlock()
	if fn != nil {
		fn(evt)
	}
}

func (m *Manager) isTunnelGenLost(connID string, gen uint64) bool {
	if gen == 0 {
		return false
	}
	m.mu.Lock()
	lost := m.invalidatedTunnelGen[connID]
	m.mu.Unlock()
	return gen <= lost
}

// OpenTransient opens a one-shot physical connection (used for TestConnection).
func (m *Manager) OpenTransient(ctx context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	opener, ok := m.registry.Get(cfg.Engine)
	if !ok {
		return nil, dto.NewAppError(dto.ErrUnsupported, fmt.Sprintf("暂不支持的数据库类型: %s", cfg.Engine), "")
	}
	return opener.Open(ctx, cfg)
}

// WithMetaConn borrows a connection from Track A (metadata & control pool) for (connID, db).
// Never executes user transactions, so slow queries on Track B never block tree browsing or KILL QUERY.
func (m *Manager) WithMetaConn(ctx context.Context, connID, db string, fn func(conn driver.Conn) error) error {
	pool, err := m.getOrCreateMetaPool(ctx, connID, db)
	if err != nil {
		return err
	}
	conn, gen, err := pool.acquire(ctx)
	if err != nil {
		return err
	}
	broken := false
	err = fn(conn)
	if err != nil {
		var ae *dto.AppError
		if errors.As(err, &ae) && ae.Code == dto.ErrSessionLost {
			broken = true
		} else if pingErr := conn.Ping(ctx); pingErr != nil {
			broken = true
		}
	}
	pool.release(conn, gen, broken)
	return err
}

// AcquireBypassControlConn borrows an independent connection for sending server-side cancel commands (e.g. MySQL KILL QUERY).
func (m *Manager) AcquireBypassControlConn(ctx context.Context, connID, db string) (driver.Conn, func(broken bool), error) {
	pool, err := m.getOrCreateMetaPool(ctx, connID, db)
	if err != nil {
		return nil, nil, err
	}
	conn, gen, err := pool.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn, func(broken bool) { pool.release(conn, gen, broken) }, nil
}

// GetOrCreateTabSession returns the dedicated Track B session for tabID, opening a physical connection if needed.
func (m *Manager) GetOrCreateTabSession(ctx context.Context, tabID, connID, db, schema string) (*TabSession, error) {
	m.mu.Lock()
	ts, exists := m.tabs[tabID]
	if exists {
		ownerConnID := ts.connID
		m.mu.Unlock()
		if connID != "" && ownerConnID != connID {
			return nil, dto.NewAppError(
				dto.ErrInvalidArgument,
				"标签页会话归属连接不匹配，已拒绝执行",
				fmt.Sprintf("tab %s belongs to connection %s, got %s", tabID, ownerConnID, connID),
			)
		}
		if err := ts.EnsureConnected(ctx, db, schema); err != nil {
			return nil, err
		}
		return ts, nil
	}
	ts = &TabSession{
		mgr:           m,
		tabID:         tabID,
		connID:        connID,
		currentDB:     db,
		currentSchema: schema,
		state:         dto.SessionLost,
		sessionEpoch:  m.nextSessionEpoch(),
	}
	m.tabs[tabID] = ts
	m.mu.Unlock()

	if err := ts.connectFresh(ctx, db, schema); err != nil {
		m.mu.Lock()
		if m.tabs[tabID] == ts {
			delete(m.tabs, tabID)
		}
		m.mu.Unlock()
		return nil, err
	}
	return ts, nil
}

// GetTabSession returns an existing TabSession without creating one.
func (m *Manager) GetTabSession(tabID string) (*TabSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ts, ok := m.tabs[tabID]
	return ts, ok
}

// CloseTab closes the dedicated physical connection owned by tabID and removes it from tracking.
func (m *Manager) CloseTab(tabID string) error {
	m.mu.Lock()
	ts, ok := m.tabs[tabID]
	if ok {
		delete(m.tabs, tabID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return ts.Close()
}

// MarkConnectionLost handles an unexpected transport / SSH tunnel disconnect for (connID, lostGen).
// Unlike CloseConnection, every Track B TabSession is KEPT in m.tabs so the UI reflects SessionLost
// (or SessionCommitUnknown if a commit/write was in flight without response) and waits for explicit Reconnect.
func (m *Manager) MarkConnectionLost(connID string, lostGen uint64) {
	m.mu.Lock()
	if lostGen > m.invalidatedTunnelGen[connID] {
		m.invalidatedTunnelGen[connID] = lostGen
	}
	var pools []*metaPool
	for k, p := range m.metaPools {
		if p.connID == connID {
			pools = append(pools, p)
			if lostGen == 0 {
				delete(m.metaPools, k)
			}
		}
	}
	var tabs []*TabSession
	for _, ts := range m.tabs {
		if ts.connID == connID {
			tabs = append(tabs, ts)
		}
	}
	opHandles := m.detachOpsForTunnelLocked(connID, lostGen)
	m.mu.Unlock()

	abortOpHandles(opHandles)

	for _, p := range pools {
		if lostGen == 0 {
			p.closeAll()
		} else {
			p.evictGeneration(lostGen)
		}
	}

	for _, ts := range tabs {
		ts.markLostByTunnel(lostGen)
	}
}

// CloseConnection closes every Track A metadata pool, Track B tab session, and active short-lived
// managed operation belonging to connID, while incrementing connLifecycle so older plans invalidate naturally.
func (m *Manager) CloseConnection(connID string) {
	m.mu.Lock()
	m.connLifecycle[connID]++
	var pools []*metaPool
	for k, p := range m.metaPools {
		if p.connID == connID {
			pools = append(pools, p)
			delete(m.metaPools, k)
		}
	}
	var tabs []*TabSession
	for id, ts := range m.tabs {
		if ts.connID == connID {
			tabs = append(tabs, ts)
			delete(m.tabs, id)
		}
	}
	opHandles := m.detachOpsForConnectionLocked(connID)
	m.mu.Unlock()

	abortOpHandles(opHandles)

	for _, ts := range tabs {
		_ = ts.Close()
	}
	for _, p := range pools {
		p.closeAll()
	}
}

// CloseAll closes all connections across all pools, tabs, and managed operations (called on app shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	seenConnIDs := make(map[string]struct{}, len(m.connLifecycle)+len(m.metaPools)+len(m.tabs)+len(m.inFlightOps))
	for cid := range m.connLifecycle {
		seenConnIDs[cid] = struct{}{}
	}
	pools := make([]*metaPool, 0, len(m.metaPools))
	for k, p := range m.metaPools {
		seenConnIDs[p.connID] = struct{}{}
		pools = append(pools, p)
		delete(m.metaPools, k)
	}
	tabs := make([]*TabSession, 0, len(m.tabs))
	for id, ts := range m.tabs {
		seenConnIDs[ts.connID] = struct{}{}
		tabs = append(tabs, ts)
		delete(m.tabs, id)
	}
	opHandles := make([]abortedOpHandle, 0, len(m.inFlightOps))
	for id, op := range m.inFlightOps {
		seenConnIDs[op.connID] = struct{}{}
		op.aborted = true
		op.abortedByLifecycle = true
		opHandles = append(opHandles, abortedOpHandle{cancel: op.cancel})
		delete(m.inFlightOps, id)
	}
	for cid := range seenConnIDs {
		m.connLifecycle[cid]++
	}
	m.mu.Unlock()

	abortOpHandles(opHandles)

	for _, ts := range tabs {
		_ = ts.Close()
	}
	for _, p := range pools {
		p.closeAll()
	}
}

// ActiveTabCount returns how many dedicated query tab sessions are currently open.
func (m *Manager) ActiveTabCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tabs)
}

// CloseDatabaseMetaPool closes and removes all Track A metadata pools for connID that target targetDB
// (including the default-DB pool if its resolved database equals targetDB).
func (m *Manager) CloseDatabaseMetaPool(connID, targetDB string) int {
	if connID == "" || targetDB == "" {
		return 0
	}
	type poolEntry struct {
		key  string
		pool *metaPool
	}
	m.mu.Lock()
	candidates := make([]poolEntry, 0, len(m.metaPools))
	for k, p := range m.metaPools {
		if p.connID == connID {
			candidates = append(candidates, poolEntry{key: k, pool: p})
		}
	}
	m.mu.Unlock()

	var matched []poolEntry
	for _, entry := range candidates {
		if entry.pool.matchesDatabase(targetDB) {
			matched = append(matched, entry)
		}
	}
	if len(matched) == 0 {
		return 0
	}

	var toClose []*metaPool
	m.mu.Lock()
	for _, entry := range matched {
		if m.metaPools[entry.key] == entry.pool {
			delete(m.metaPools, entry.key)
			toClose = append(toClose, entry.pool)
		}
	}
	m.mu.Unlock()

	for _, p := range toClose {
		p.closeAll()
	}
	return len(toClose)
}

func matchDBName(engine dto.EngineType, a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if engine == dto.EnginePostgres {
		return a == b
	}
	return strings.EqualFold(a, b)
}

func (m *Manager) getOrCreateMetaPool(_ context.Context, connID, db string) (*metaPool, error) {
	key := connID + "\x00" + db
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.metaPools[key]; ok {
		return p, nil
	}
	maxIdle := m.metaMaxIdle
	if maxIdle < 0 {
		maxIdle = 0
	}
	p := &metaPool{
		mgr:     m,
		connID:  connID,
		db:      db,
		sem:     make(chan struct{}, defaultMetaPoolConcurrency),
		maxIdle: maxIdle,
		idleTTL: m.metaIdleTTL,
		idle:    make([]pooledMetaConn, 0, maxIdle),
		inUse:   make(map[driver.Conn]uint64),
	}
	m.metaPools[key] = p
	return p, nil
}

// --- Track A: Small Metadata / Control Pool ---

type pooledMetaConn struct {
	conn       driver.Conn
	tunnelGen  uint64
	returnedAt time.Time
}

type metaPool struct {
	mgr     *Manager
	connID  string
	db      string
	sem     chan struct{}
	maxIdle int
	idleTTL time.Duration

	mu        sync.Mutex
	engine    dto.EngineType
	actualDB  string
	idle      []pooledMetaConn
	inUse     map[driver.Conn]uint64
	reapTimer *time.Timer
	closed    bool
}

func (p *metaPool) matchesDatabase(targetDB string) bool {
	p.mu.Lock()
	eng := p.engine
	actual := p.actualDB
	p.mu.Unlock()

	if p.db != "" && matchDBName(eng, p.db, targetDB) {
		return true
	}
	if actual != "" && matchDBName(eng, actual, targetDB) {
		return true
	}
	return false
}

func (p *metaPool) acquire(ctx context.Context) (driver.Conn, uint64, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.sem
		return nil, 0, dto.NewAppError(dto.ErrSessionLost, "连接池已关闭", "")
	}
	for len(p.idle) > 0 {
		pc := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		expired := p.idleTTL > 0 && !pc.returnedAt.IsZero() && time.Since(pc.returnedAt) >= p.idleTTL
		p.mu.Unlock()

		if expired || p.mgr.isTunnelGenLost(p.connID, pc.tunnelGen) {
			_ = pc.conn.Close()
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				<-p.sem
				return nil, 0, dto.NewAppError(dto.ErrSessionLost, "连接池已关闭", "")
			}
			continue
		}

		if err := pc.conn.Ping(ctx); err == nil {
			// Re-verify pool & tunnel generation state before handing out to caller.
			p.mu.Lock()
			if p.closed || p.mgr.isTunnelGenLost(p.connID, pc.tunnelGen) {
				p.mu.Unlock()
				_ = pc.conn.Close()
				<-p.sem
				return nil, 0, dto.NewAppError(dto.ErrSessionLost, "连接或隧道已失效", "")
			}
			p.inUse[pc.conn] = pc.tunnelGen
			p.mu.Unlock()
			return pc.conn, pc.tunnelGen, nil
		}
		_ = pc.conn.Close()
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			<-p.sem
			return nil, 0, dto.NewAppError(dto.ErrSessionLost, "连接池已关闭", "")
		}
	}
	p.mu.Unlock()

	cfg, err := p.mgr.resolve(ctx, p.connID, p.db)
	if err != nil {
		<-p.sem
		return nil, 0, err
	}
	c, err := p.mgr.OpenTransient(ctx, cfg)
	if err != nil {
		<-p.sem
		return nil, 0, err
	}

	resolvedDB := cfg.Database
	if resolvedDB == "" && cfg.Engine == dto.EnginePostgres {
		resolvedDB = "postgres"
	}

	// Re-verify pool & tunnel generation state after OpenTransient before handing out.
	p.mu.Lock()
	if p.closed || p.mgr.isTunnelGenLost(p.connID, cfg.TunnelGen) {
		p.mu.Unlock()
		_ = c.Close()
		<-p.sem
		return nil, 0, dto.NewAppError(dto.ErrSessionLost, "连接建立期间隧道已失效", "")
	}
	p.engine = cfg.Engine
	if resolvedDB != "" {
		p.actualDB = resolvedDB
	}
	p.inUse[c] = cfg.TunnelGen
	p.mu.Unlock()
	return c, cfg.TunnelGen, nil
}

func (p *metaPool) release(c driver.Conn, gen uint64, broken bool) {
	defer func() { <-p.sem }()
	if c == nil {
		return
	}
	p.mu.Lock()
	delete(p.inUse, c)
	if p.closed || broken || p.mgr.isTunnelGenLost(p.connID, gen) || len(p.idle) >= p.maxIdle {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.idle = append(p.idle, pooledMetaConn{conn: c, tunnelGen: gen, returnedAt: time.Now()})
	p.scheduleReapLocked()
	p.mu.Unlock()
}

// scheduleReapLocked arms an idle-eviction timer for the oldest idle connection. Caller holds p.mu.
func (p *metaPool) scheduleReapLocked() {
	if p.closed || p.idleTTL <= 0 || len(p.idle) == 0 {
		return
	}
	oldest := p.idle[0].returnedAt
	for _, pc := range p.idle[1:] {
		if pc.returnedAt.Before(oldest) {
			oldest = pc.returnedAt
		}
	}
	wait := p.idleTTL - time.Since(oldest)
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	if p.reapTimer != nil {
		p.reapTimer.Stop()
	}
	p.reapTimer = time.AfterFunc(wait, p.reapIdle)
}

func (p *metaPool) reapIdle() {
	p.mu.Lock()
	if p.closed || p.idleTTL <= 0 || len(p.idle) == 0 {
		p.reapTimer = nil
		p.mu.Unlock()
		return
	}
	now := time.Now()
	var toClose []driver.Conn
	kept := p.idle[:0]
	for _, pc := range p.idle {
		if now.Sub(pc.returnedAt) >= p.idleTTL {
			toClose = append(toClose, pc.conn)
		} else {
			kept = append(kept, pc)
		}
	}
	p.idle = kept
	p.reapTimer = nil
	if len(p.idle) > 0 {
		p.scheduleReapLocked()
	}
	p.mu.Unlock()

	for _, c := range toClose {
		_ = c.Close()
	}
}

func (p *metaPool) evictGeneration(lostGen uint64) {
	p.mu.Lock()
	var toClose []driver.Conn
	kept := p.idle[:0]
	for _, pc := range p.idle {
		if pc.tunnelGen > 0 && pc.tunnelGen <= lostGen {
			toClose = append(toClose, pc.conn)
		} else {
			kept = append(kept, pc)
		}
	}
	p.idle = kept
	for c, gen := range p.inUse {
		if gen > 0 && gen <= lostGen {
			toClose = append(toClose, c)
			delete(p.inUse, c)
		}
	}
	p.mu.Unlock()

	for _, c := range toClose {
		_ = c.Close()
	}
}

func (p *metaPool) closeAll() {
	p.mu.Lock()
	p.closed = true
	if p.reapTimer != nil {
		p.reapTimer.Stop()
		p.reapTimer = nil
	}
	toClose := make([]driver.Conn, 0, len(p.idle)+len(p.inUse))
	for _, pc := range p.idle {
		toClose = append(toClose, pc.conn)
	}
	p.idle = nil
	for c := range p.inUse {
		toClose = append(toClose, c)
		delete(p.inUse, c)
	}
	p.mu.Unlock()

	for _, c := range toClose {
		_ = c.Close()
	}
}

// --- Track B: Dedicated Tab Session with Cancel Fence & Authoritative SessionState ---

type TabSession struct {
	mgr    *Manager
	tabID  string
	connID string

	// execMu strictly serializes SQL executions on this tab's single physical connection.
	// Held across the entire statement execution, stream drain/close, AND in-flight cancel fence wait!
	execMu sync.Mutex

	// stateMu guards live state inspected concurrently by CancelExecution / Info / ForceAbort / MarkConnectionLost.
	stateMu           sync.Mutex
	conn              driver.Conn
	tunnelGen         uint64
	state             dto.SessionState
	sessionEpoch      int64
	inFlightMayCommit bool
	readOnly          bool
	currentDB         string
	currentSchema     string
	serverConnID      string
	caps              dto.Capabilities
	dialect           dto.EngineType
	pendingTxDDL      []sqlsplit.DDLAction
	activeExecID      string
	inFlightCancels   int
	cancelDoneCh      chan struct{}
	cancelSent        bool
	cancelConfirmed   bool
	cancelFailed      bool
	closed            bool
}

func (ts *TabSession) infoLocked() dto.TabSessionInfo {
	return dto.TabSessionInfo{
		TabID:         ts.tabID,
		ConnectionID:  ts.connID,
		CurrentDB:     ts.currentDB,
		CurrentSchema: ts.currentSchema,
		SessionState:  ts.state,
		SessionEpoch:  ts.sessionEpoch,
		ServerConnID:  ts.serverConnID,
		ReadOnly:      ts.readOnly,
	}
}

func (ts *TabSession) Info() dto.TabSessionInfo {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	return ts.infoLocked()
}

// RecordPendingTxDDL buffers a structural DDL action executed inside an open transaction
// so Explorer refreshes once COMMIT succeeds (never prematurely before commit).
func (ts *TabSession) RecordPendingTxDDL(act sqlsplit.DDLAction) {
	ts.stateMu.Lock()
	ts.pendingTxDDL = append(ts.pendingTxDDL, act)
	ts.stateMu.Unlock()
}

// DrainPendingTxDDL returns and clears buffered transactional DDL actions (called after COMMIT).
func (ts *TabSession) DrainPendingTxDDL() []sqlsplit.DDLAction {
	ts.stateMu.Lock()
	out := ts.pendingTxDDL
	ts.pendingTxDDL = nil
	ts.stateMu.Unlock()
	return out
}

// ClearPendingTxDDL discards buffered transactional DDL actions (called after ROLLBACK).
func (ts *TabSession) ClearPendingTxDDL() {
	ts.stateMu.Lock()
	ts.pendingTxDDL = nil
	ts.stateMu.Unlock()
}

func (ts *TabSession) snapshotState() (dto.SessionState, int64) {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	return ts.state, ts.sessionEpoch
}

// transitionToLostLocked enforces the CommitOutcomeUnknown protection invariant across ALL
// teardown/abort/error paths: once SessionCommitUnknown is entered (or an in-flight commit/write
// sent wire bytes without a response), NEVER downgrade it to SessionLost.
// Caller MUST hold ts.stateMu.
func (ts *TabSession) transitionToLostLocked(inFlightCommit bool) (driver.Conn, dto.SessionState, int64) {
	old := ts.conn
	nextState := dto.SessionLost
	if ts.state == dto.SessionCommitUnknown ||
		(inFlightCommit && old != nil && old.WriteSentWithoutResponse()) {
		nextState = dto.SessionCommitUnknown
	}
	ts.conn = nil
	ts.tunnelGen = 0
	ts.activeExecID = ""
	ts.pendingTxDDL = nil
	ts.state = nextState
	ts.sessionEpoch = ts.mgr.nextSessionEpoch()
	return old, ts.state, ts.sessionEpoch
}

func (ts *TabSession) markLostByTunnel(lostGen uint64) {
	ts.stateMu.Lock()
	if ts.closed {
		ts.stateMu.Unlock()
		return
	}
	if lostGen > 0 {
		if ts.tunnelGen == 0 || ts.tunnelGen > lostGen {
			ts.stateMu.Unlock()
			return
		}
	}
	// If already lost with no live conn and not in CommitUnknown, avoid duplicate noisy events.
	if ts.conn == nil && (ts.state == dto.SessionLost || ts.state == dto.SessionCommitUnknown) {
		ts.stateMu.Unlock()
		return
	}
	old, st, ep := ts.transitionToLostLocked(ts.inFlightMayCommit)
	ts.stateMu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	ts.mgr.emitStateChanged(dto.SessionStateEvent{
		TabID:        ts.tabID,
		ConnectionID: ts.connID,
		SessionState: st,
		SessionEpoch: ep,
	})
}

func (ts *TabSession) ConnectionID() string {
	return ts.connID
}

func (ts *TabSession) State() dto.SessionState {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	return ts.state
}

func (ts *TabSession) Dialect() dto.EngineType {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	return ts.dialect
}

func (ts *TabSession) ReadOnly() bool {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	return ts.readOnly
}

func (ts *TabSession) Capabilities() dto.Capabilities {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	return ts.caps
}

// EnsureConnected verifies the session is usable.
// CRITICAL: Never silently reconnects if SessionLost or CommitOutcomeUnknown — caller/user must explicitly reconnect!
func (ts *TabSession) EnsureConnected(ctx context.Context, targetDB, targetSchema string) error {
	ts.stateMu.Lock()
	st := ts.state
	ep := ts.sessionEpoch
	conn := ts.conn
	curDB := ts.currentDB
	ts.stateMu.Unlock()

	if st == dto.SessionCommitUnknown {
		return &dto.AppError{
			Code:         dto.ErrCommitOutcomeUnknown,
			Message:      "上次写入或提交结果未知，请先核对数据库数据并显式重新连接后再执行",
			RetrySafe:    false,
			Session:      dto.SessionCommitUnknown,
			SessionEpoch: ep,
		}
	}
	if st == dto.SessionLost || conn == nil {
		return &dto.AppError{
			Code:         dto.ErrSessionLost,
			Message:      "原会话已结束 (临时表与未提交事务已失效)，请点击重新连接建立新会话",
			Session:      dto.SessionLost,
			SessionEpoch: ep,
		}
	}
	if targetDB != "" && targetDB != curDB {
		if err := ts.SwitchDatabase(ctx, targetDB); err != nil {
			return err
		}
	}
	if targetSchema != "" {
		ts.stateMu.Lock()
		curSchema := ts.currentSchema
		curConn := ts.conn
		ts.stateMu.Unlock()
		if targetSchema != curSchema {
			if ss, ok := curConn.(driver.SchemaSwitcher); ok {
				if err := ss.SwitchSchema(ctx, targetSchema); err != nil {
					return err
				}
			}
			ts.stateMu.Lock()
			if ts.conn == curConn {
				ts.currentSchema = targetSchema
			}
			ts.stateMu.Unlock()
		}
	}
	return nil
}

// Reconnect explicitly replaces a lost/unknown session with a brand-new physical connection.
func (ts *TabSession) Reconnect(ctx context.Context, db, schema string) error {
	ts.execMu.Lock()
	defer ts.execMu.Unlock()
	if db == "" {
		ts.stateMu.Lock()
		db = ts.currentDB
		if schema == "" {
			schema = ts.currentSchema
		}
		ts.stateMu.Unlock()
	}
	return ts.connectFresh(ctx, db, schema)
}

func (ts *TabSession) connectFresh(ctx context.Context, db, schema string) error {
	cfg, err := ts.mgr.resolve(ctx, ts.connID, db)
	if err != nil {
		return err
	}
	c, err := ts.mgr.OpenTransient(ctx, cfg)
	if err != nil {
		return err
	}
	if schema != "" {
		if ss, ok := c.(driver.SchemaSwitcher); ok {
			if err := ss.SwitchSchema(ctx, schema); err != nil {
				_ = c.Close()
				return err
			}
		}
	}
	fact, err := c.ServerFact(ctx)
	if err != nil {
		_ = c.Close()
		return err
	}
	caps := c.Capabilities(fact.ServerVersion)

	ts.stateMu.Lock()
	if ts.closed {
		ep := ts.sessionEpoch
		ts.stateMu.Unlock()
		_ = c.Close()
		return &dto.AppError{
			Code:         dto.ErrSessionLost,
			Message:      "标签页会话已关闭",
			Session:      dto.SessionLost,
			SessionEpoch: ep,
		}
	}
	if cfg.TunnelGen > 0 && ts.mgr.isTunnelGenLost(ts.connID, cfg.TunnelGen) {
		old, st, ep := ts.transitionToLostLocked(false)
		ts.stateMu.Unlock()
		_ = c.Close()
		if old != nil {
			_ = old.Close()
		}
		ts.mgr.emitStateChanged(dto.SessionStateEvent{
			TabID:        ts.tabID,
			ConnectionID: ts.connID,
			SessionState: st,
			SessionEpoch: ep,
		})
		return &dto.AppError{
			Code:         dto.ErrSessionLost,
			Message:      "建连期间 SSH 隧道已断开，请重新连接",
			Session:      st,
			SessionEpoch: ep,
		}
	}

	old := ts.conn
	ts.conn = c
	ts.tunnelGen = cfg.TunnelGen
	ts.state = dto.SessionAliveIdle
	ts.sessionEpoch = ts.mgr.nextSessionEpoch()
	ts.inFlightMayCommit = false
	ts.readOnly = cfg.ReadOnly
	ts.currentDB = fact.CurrentDB
	if ts.currentDB == "" {
		ts.currentDB = db
	}
	ts.currentSchema = schema
	if ts.currentSchema == "" {
		ts.currentSchema = fact.CurrentSchema
	}
	ts.serverConnID = fact.ServerConnID
	ts.caps = caps
	ts.dialect = fact.DetectedType
	ts.pendingTxDDL = nil
	ts.activeExecID = ""
	ts.cancelSent = false
	ts.cancelConfirmed = false
	ts.cancelFailed = false
	ts.stateMu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	return nil
}

// SwitchDatabase switches the tab's active database.
// For MySQL/MariaDB (implements DatabaseSwitcher), runs USE in-place.
// For PostgreSQL (SwitchDBReconnects == true), opens a new connection targeting targetDB (blocked if in tx).
func (ts *TabSession) SwitchDatabase(ctx context.Context, targetDB string) error {
	if targetDB == "" {
		return nil
	}
	ts.execMu.Lock()
	defer ts.execMu.Unlock()

	ts.stateMu.Lock()
	conn := ts.conn
	st := ts.state
	ep := ts.sessionEpoch
	curDB := ts.currentDB
	schema := ts.currentSchema
	ts.stateMu.Unlock()

	if targetDB == curDB && conn != nil && st != dto.SessionLost && st != dto.SessionCommitUnknown {
		return nil
	}
	if conn == nil || st == dto.SessionLost {
		return ts.connectFresh(ctx, targetDB, schema)
	}
	if st == dto.SessionCommitUnknown {
		return &dto.AppError{
			Code:         dto.ErrCommitOutcomeUnknown,
			Message:      "上次写入或提交结果未知，请先核对数据并显式重新连接后再切换数据库",
			RetrySafe:    false,
			Session:      dto.SessionCommitUnknown,
			SessionEpoch: ep,
		}
	}
	if switcher, ok := conn.(driver.DatabaseSwitcher); ok {
		if err := switcher.SwitchDatabase(ctx, targetDB); err != nil {
			return err
		}
		ts.stateMu.Lock()
		if ts.conn == conn {
			ts.currentDB = targetDB
		}
		ts.stateMu.Unlock()
		return nil
	}

	// Engine requires reconnecting to target database (e.g., PostgreSQL).
	if st == dto.SessionAliveInTx || st == dto.SessionAliveFailedTx {
		return dto.NewAppError(
			dto.ErrInvalidArgument,
			"当前标签页处于未结束事务中，切换该数据库需要重建连接，请先 COMMIT 或 ROLLBACK",
			"",
		)
	}
	return ts.connectFresh(ctx, targetDB, "")
}

// SwitchSchema changes the active schema / search_path on engines implementing driver.SchemaSwitcher (e.g., PostgreSQL).
func (ts *TabSession) SwitchSchema(ctx context.Context, targetSchema string) error {
	if targetSchema == "" {
		return nil
	}
	ts.execMu.Lock()
	defer ts.execMu.Unlock()

	ts.stateMu.Lock()
	conn := ts.conn
	st := ts.state
	ep := ts.sessionEpoch
	ts.stateMu.Unlock()

	if conn == nil || st == dto.SessionLost {
		return &dto.AppError{
			Code:         dto.ErrSessionLost,
			Message:      "当前会话已断开，请重新连接后再切换 Schema",
			Session:      dto.SessionLost,
			SessionEpoch: ep,
		}
	}
	if st == dto.SessionCommitUnknown {
		return &dto.AppError{
			Code:         dto.ErrCommitOutcomeUnknown,
			Message:      "上次写入或提交结果未知，请先核对数据并显式重新连接",
			RetrySafe:    false,
			Session:      dto.SessionCommitUnknown,
			SessionEpoch: ep,
		}
	}
	ss, ok := conn.(driver.SchemaSwitcher)
	if !ok {
		return nil
	}
	if err := ss.SwitchSchema(ctx, targetSchema); err != nil {
		return err
	}
	ts.stateMu.Lock()
	if ts.conn == conn {
		ts.currentSchema = targetSchema
	}
	ts.stateMu.Unlock()
	return nil
}

// StatementOutcome holds the result of executing one statement inside TabSession.
type StatementOutcome struct {
	IsResultSet   bool
	AffectedRows  int64
	DurationMs    int64
	SessionState  dto.SessionState
	SessionEpoch  int64
	CurrentDB     string
	CurrentSchema string
	Error         *dto.AppError
}

// ExecuteStatement runs one classified SQL statement on this tab's dedicated physical connection.
// For row-returning statements, onStream is called while execMu is held and MUST drain/close the stream
// before returning — guaranteeing stream closure and cancel-fence completion BEFORE any queued query B runs.
func (ts *TabSession) ExecuteStatement(
	ctx context.Context,
	execID string,
	stmt sqlsplit.Statement,
	onStream func(stream driver.RowStream) error,
) (outcome StatementOutcome) {
	ts.execMu.Lock()
	defer ts.execMu.Unlock()

	// Check current session state BEFORE touching the connection (may have become SessionLost
	// if a previous query's cancel could not be confirmed!).
	ts.stateMu.Lock()
	if ts.closed || ts.conn == nil || ts.state == dto.SessionLost {
		ep := ts.sessionEpoch
		ts.stateMu.Unlock()
		return StatementOutcome{
			SessionState: dto.SessionLost,
			SessionEpoch: ep,
			Error: &dto.AppError{
				Code:         dto.ErrSessionLost,
				Message:      "原会话已结束 (临时表与未提交事务已失效)，请重新连接后再执行",
				Session:      dto.SessionLost,
				SessionEpoch: ep,
			},
		}
	}
	if ts.state == dto.SessionCommitUnknown {
		ep := ts.sessionEpoch
		ts.stateMu.Unlock()
		return StatementOutcome{
			SessionState: dto.SessionCommitUnknown,
			SessionEpoch: ep,
			Error: &dto.AppError{
				Code:         dto.ErrCommitOutcomeUnknown,
				Message:      "上次写入或提交结果未知，请先核对数据库数据并显式重新连接后再执行",
				RetrySafe:    false,
				Session:      dto.SessionCommitUnknown,
				SessionEpoch: ep,
			},
		}
	}

	conn := ts.conn
	wasInTx := ts.state == dto.SessionAliveInTx
	readOnly := ts.readOnly
	dialect := ts.dialect
	curDB := ts.currentDB
	curSchema := ts.currentSchema
	ts.activeExecID = execID
	ts.cancelSent = false
	ts.cancelConfirmed = false
	ts.cancelFailed = false
	ts.inFlightMayCommit = false
	ts.stateMu.Unlock()

	interruptedByCancel := false
	// Guarantee activeExecID is cleared AND any in-flight cancel fence has fully drained BEFORE execMu unlocks!
	defer func() {
		ts.finishExecutionAndDrainCancelFence(execID, interruptedByCancel)
		outcome.SessionState, outcome.SessionEpoch = ts.snapshotState()
	}()

	if stmt.SplitError != "" {
		st, ep := ts.snapshotState()
		return StatementOutcome{
			SessionState:  st,
			SessionEpoch:  ep,
			CurrentDB:     curDB,
			CurrentSchema: curSchema,
			Error: &dto.AppError{
				Code:         dto.ErrInvalidArgument,
				Message:      stmt.SplitError,
				Detail:       stmt.Text,
				Session:      st,
				SessionEpoch: ep,
			},
		}
	}

	// Client read-only guardrail (in addition to real DB privileges & session read-only mode)
	if readOnly {
		splitDialect := toSplitDialect(dialect)
		if stmt.MutatesData || stmt.Kind == sqlsplit.KindWrite || stmt.Kind == sqlsplit.KindDDL || sqlsplit.IsReadOnlyEscapeSet(stmt.Text, splitDialect) {
			st, ep := ts.snapshotState()
			return StatementOutcome{
				SessionState:  st,
				SessionEpoch:  ep,
				CurrentDB:     curDB,
				CurrentSchema: curSchema,
				Error: &dto.AppError{
					Code:         dto.ErrReadOnlyDenied,
					Message:      "当前连接处于只读保护模式，已拦截写入或修改操作",
					Detail:       "client read-only guardrail blocked statement: " + stmt.FirstWord,
					Session:      st,
					SessionEpoch: ep,
				},
			}
		}
	}

	executor, ok := conn.(driver.QueryExecutor)
	if !ok {
		st, ep := ts.snapshotState()
		return StatementOutcome{
			SessionState: st,
			SessionEpoch: ep,
			Error:        dto.NewAppError(dto.ErrInternal, "驱动未实现查询执行接口", ""),
		}
	}

	mayCommitOnServer := (stmt.Kind == sqlsplit.KindCommit) ||
		(!wasInTx && (stmt.MutatesData || stmt.Kind == sqlsplit.KindWrite || stmt.Kind == sqlsplit.KindDDL))

	conn.ResetWireTracker()
	ts.stateMu.Lock()
	if ts.conn == conn {
		ts.inFlightMayCommit = mayCommitOnServer
	}
	ts.stateMu.Unlock()

	start := time.Now()
	bound := driver.BoundStatement{SQL: stmt.Text}

	if stmt.ReturnsRows {
		stream, err := executor.Query(ctx, bound)
		if err != nil {
			appErr := ts.handleExecError(conn, err, wasInTx, mayCommitOnServer)
			interruptedByCancel = appErr.Code == dto.ErrStatementCanceled
			info := ts.Info()
			return StatementOutcome{
				DurationMs:    time.Since(start).Milliseconds(),
				SessionState:  info.SessionState,
				SessionEpoch:  info.SessionEpoch,
				CurrentDB:     info.CurrentDB,
				CurrentSchema: info.CurrentSchema,
				Error:         appErr,
			}
		}
		var streamErr error
		if onStream != nil {
			streamErr = onStream(stream)
		}
		closeErr := stream.Close()
		if streamErr == nil {
			streamErr = closeErr
		}
		if streamErr != nil {
			appErr := ts.handleExecError(conn, streamErr, wasInTx, mayCommitOnServer)
			interruptedByCancel = appErr.Code == dto.ErrStatementCanceled
			info := ts.Info()
			return StatementOutcome{
				IsResultSet:   true,
				DurationMs:    time.Since(start).Milliseconds(),
				SessionState:  info.SessionState,
				SessionEpoch:  info.SessionEpoch,
				CurrentDB:     info.CurrentDB,
				CurrentSchema: info.CurrentSchema,
				Error:         appErr,
			}
		}
		ts.updateStateAfterSuccess(stmt, conn)
		info := ts.Info()
		return StatementOutcome{
			IsResultSet:   true,
			DurationMs:    time.Since(start).Milliseconds(),
			SessionState:  info.SessionState,
			SessionEpoch:  info.SessionEpoch,
			CurrentDB:     info.CurrentDB,
			CurrentSchema: info.CurrentSchema,
		}
	}

	fact, err := executor.Exec(ctx, bound)
	if err != nil {
		appErr := ts.handleExecError(conn, err, wasInTx, mayCommitOnServer)
		interruptedByCancel = appErr.Code == dto.ErrStatementCanceled
		info := ts.Info()
		return StatementOutcome{
			DurationMs:    time.Since(start).Milliseconds(),
			SessionState:  info.SessionState,
			SessionEpoch:  info.SessionEpoch,
			CurrentDB:     info.CurrentDB,
			CurrentSchema: info.CurrentSchema,
			Error:         appErr,
		}
	}

	ts.updateStateAfterSuccess(stmt, conn)
	info := ts.Info()
	return StatementOutcome{
		IsResultSet:   false,
		AffectedRows:  fact.AffectedRows,
		DurationMs:    time.Since(start).Milliseconds(),
		SessionState:  info.SessionState,
		SessionEpoch:  info.SessionEpoch,
		CurrentDB:     info.CurrentDB,
		CurrentSchema: info.CurrentSchema,
	}
}

// finishExecutionAndDrainCancelFence runs while execMu is STILL held!
//  1. Clears activeExecID & inFlightMayCommit so no future late cancel for execID can pass validation.
//  2. If a cancel for execID ALREADY passed validation and is still in flight on the server,
//     blocks here until that cancel call completes.
//  3. Stops reusing ts.conn (closes it & sets SessionLost without ever downgrading SessionCommitUnknown)
//     whenever cancel failed OR an unconfirmed async cancel was sent while the query exited naturally.
func (ts *TabSession) finishExecutionAndDrainCancelFence(execID string, interruptedByCancel bool) {
	ts.stateMu.Lock()
	if ts.activeExecID == execID {
		ts.activeExecID = ""
	}
	ts.inFlightMayCommit = false
	waitCh := ts.cancelDoneCh
	inFlight := ts.inFlightCancels
	ts.stateMu.Unlock()

	if inFlight > 0 && waitCh != nil {
		<-waitCh
	}

	ts.stateMu.Lock()
	unsafeReuse := ts.cancelFailed || (ts.cancelSent && !ts.cancelConfirmed && !interruptedByCancel)
	ts.cancelSent = false
	ts.cancelConfirmed = false
	ts.cancelFailed = false
	var dropConn driver.Conn
	var emitEvt *dto.SessionStateEvent
	if unsafeReuse && ts.conn != nil {
		old, st, ep := ts.transitionToLostLocked(false)
		dropConn = old
		emitEvt = &dto.SessionStateEvent{
			TabID:        ts.tabID,
			ConnectionID: ts.connID,
			SessionState: st,
			SessionEpoch: ep,
		}
	}
	ts.stateMu.Unlock()

	if dropConn != nil {
		_ = dropConn.Close()
	}
	if emitEvt != nil {
		ts.mgr.emitStateChanged(*emitEvt)
	}
}

func (ts *TabSession) handleExecError(conn driver.Conn, err error, wasInTx bool, mayCommitOnServer bool) *dto.AppError {
	appErr := conn.ClassifyError(err, wasInTx)

	// CRITICAL: If an autocommit write/DDL or explicit COMMIT sent bytes on the wire
	// and lost the connection BEFORE receiving any response byte from the server,
	// we MUST report CommitOutcomeUnknown — NEVER assert rolled back, NEVER auto-retry!
	if mayCommitOnServer && conn.WriteSentWithoutResponse() && (appErr.Code == dto.ErrSessionLost || appErr.Code == dto.ErrConnectionFailed) {
		ts.stateMu.Lock()
		var old driver.Conn
		if ts.conn == conn {
			old = ts.conn
			ts.conn = nil
			ts.tunnelGen = 0
		}
		ts.state = dto.SessionCommitUnknown
		ts.sessionEpoch = ts.mgr.nextSessionEpoch()
		ep := ts.sessionEpoch
		ts.stateMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		ts.mgr.emitStateChanged(dto.SessionStateEvent{
			TabID:        ts.tabID,
			ConnectionID: ts.connID,
			SessionState: dto.SessionCommitUnknown,
			SessionEpoch: ep,
		})
		return &dto.AppError{
			Code:         dto.ErrCommitOutcomeUnknown,
			Message:      "写入或提交已发出，但在收到数据库确认前连接中断，结果未知。请先核对数据库实际数据，切勿直接重试。",
			Detail:       err.Error(),
			RetrySafe:    false,
			Session:      dto.SessionCommitUnknown,
			SessionEpoch: ep,
		}
	}

	if appErr.Code == dto.ErrSessionLost {
		ts.stateMu.Lock()
		old, st, ep := ts.transitionToLostLocked(false)
		ts.stateMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		ts.mgr.emitStateChanged(dto.SessionStateEvent{
			TabID:        ts.tabID,
			ConnectionID: ts.connID,
			SessionState: st,
			SessionEpoch: ep,
		})
		if st == dto.SessionCommitUnknown {
			return &dto.AppError{
				Code:         dto.ErrCommitOutcomeUnknown,
				Message:      "写入或提交已发出，但在收到数据库确认前连接中断，结果未知。请先核对数据库实际数据，切勿直接重试。",
				Detail:       err.Error(),
				RetrySafe:    false,
				Session:      dto.SessionCommitUnknown,
				SessionEpoch: ep,
			}
		}
		appErr.Session = st
		appErr.SessionEpoch = ep
		return appErr
	}

	if appErr.Code == dto.ErrTxAborted || (wasInTx && ts.Dialect() == dto.EnginePostgres) {
		// In PostgreSQL, any error (including canceled statement) inside a transaction aborts the tx block.
		// Keep pendingTxDDL intact here because ROLLBACK TO SAVEPOINT can recover the transaction and
		// later COMMIT pre-savepoint DDL; full ROLLBACK / SessionLost clears pendingTxDDL.
		ts.stateMu.Lock()
		if ts.conn == conn && ts.state == dto.SessionAliveInTx {
			ts.state = dto.SessionAliveFailedTx
			ts.sessionEpoch = ts.mgr.nextSessionEpoch()
		}
		appErr.Session = ts.state
		appErr.SessionEpoch = ts.sessionEpoch
		ts.stateMu.Unlock()
		return appErr
	}

	st, ep := ts.snapshotState()
	appErr.Session = st
	appErr.SessionEpoch = ep
	return appErr
}

func (ts *TabSession) updateStateAfterSuccess(stmt sqlsplit.Statement, execConn driver.Conn) {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	// Never resurrect SessionLost or SessionCommitUnknown if the connection died concurrently!
	if ts.conn != execConn || ts.state == dto.SessionLost || ts.state == dto.SessionCommitUnknown {
		return
	}
	prevState := ts.state
	switch stmt.Kind {
	case sqlsplit.KindBegin:
		ts.state = dto.SessionAliveInTx
	case sqlsplit.KindCommit, sqlsplit.KindRollback:
		if stmt.StartsNextTx && (ts.dialect != dto.EnginePostgres || prevState == dto.SessionAliveInTx || prevState == dto.SessionAliveFailedTx) {
			ts.state = dto.SessionAliveInTx
		} else {
			ts.state = dto.SessionAliveIdle
		}
	case sqlsplit.KindRollbackToSavepoint:
		// Savepoint rollback keeps the outer transaction open and recovers PG FailedTx back to InTx.
		if ts.state == dto.SessionAliveFailedTx {
			ts.state = dto.SessionAliveInTx
		}
	case sqlsplit.KindDDL:
		if !ts.caps.DDLTransactional && ts.state == dto.SessionAliveInTx {
			ts.state = dto.SessionAliveIdle
		}
	case sqlsplit.KindUse:
		if db := sqlsplit.ExtractUseDatabase(stmt.Text); db != "" {
			ts.currentDB = db
		}
	}
	if ts.state != prevState {
		ts.sessionEpoch = ts.mgr.nextSessionEpoch()
	}
}

// CancelExecution attempts graceful server-side cancellation for one specific execID.
func (ts *TabSession) CancelExecution(ctx context.Context, execID string) dto.CancelExecutionResponse {
	ts.stateMu.Lock()
	if ts.activeExecID == "" || ts.activeExecID != execID || ts.conn == nil {
		st := ts.state
		ep := ts.sessionEpoch
		ts.stateMu.Unlock()
		return dto.CancelExecutionResponse{
			TabID:        ts.tabID,
			ExecID:       execID,
			Status:       "noop",
			SessionState: st,
			SessionEpoch: ep,
			Message:      "目标语句已结束，忽略迟到的取消请求",
		}
	}

	targetConn := ts.conn
	db := ts.currentDB
	ts.inFlightCancels++
	ts.cancelSent = true
	if ts.cancelDoneCh == nil {
		ts.cancelDoneCh = make(chan struct{})
	}
	ts.stateMu.Unlock()

	defer func() {
		ts.stateMu.Lock()
		ts.inFlightCancels--
		if ts.inFlightCancels == 0 && ts.cancelDoneCh != nil {
			close(ts.cancelDoneCh)
			ts.cancelDoneCh = nil
		}
		ts.stateMu.Unlock()
	}()

	markCancelFailed := func(msg string) dto.CancelExecutionResponse {
		ts.stateMu.Lock()
		ts.cancelFailed = true
		st := dto.SessionLost
		if ts.state == dto.SessionCommitUnknown {
			st = dto.SessionCommitUnknown
		}
		ep := ts.sessionEpoch
		ts.stateMu.Unlock()
		return dto.CancelExecutionResponse{
			TabID:        ts.tabID,
			ExecID:       execID,
			Status:       "session_lost",
			SessionState: st,
			SessionEpoch: ep,
			Message:      msg,
		}
	}

	canceler, ok := targetConn.(driver.ExecutionCanceler)
	if !ok {
		return markCancelFailed("当前驱动不支持服务端优雅取消，已停止复用原会话")
	}

	cancelCtx, cancelTimeout := context.WithTimeout(ctx, 4*time.Second)
	defer cancelTimeout()

	var ctrlConn driver.Conn
	releaseCtrl := func(bool) {}
	if targetConn.Dialect() != dto.EngineSQLite {
		var err error
		ctrlConn, releaseCtrl, err = ts.mgr.AcquireBypassControlConn(cancelCtx, ts.connID, db)
		if err != nil {
			return markCancelFailed("无法建立旁路取消连接，原会话已停止复用")
		}
	}

	confirmed, cancelErr := canceler.CancelInFlight(cancelCtx, ctrlConn)
	releaseCtrl(cancelErr != nil)

	if cancelErr != nil {
		return markCancelFailed("取消指令发送失败，无法确认原会话安全，已停止复用")
	}

	if confirmed {
		ts.stateMu.Lock()
		ts.cancelConfirmed = true
		st := ts.state
		ep := ts.sessionEpoch
		ts.stateMu.Unlock()
		return dto.CancelExecutionResponse{
			TabID:        ts.tabID,
			ExecID:       execID,
			Status:       "canceled",
			SessionState: st,
			SessionEpoch: ep,
			Message:      "已通过旁路通道取消当前语句，原会话保持存活",
		}
	}

	// Async protocol cancel sent without server ACK (e.g., PostgreSQL CancelRequest).
	// If query A exits with ErrStatementCanceled, connection remains alive; if A finished naturally,
	// finishExecutionAndDrainCancelFence will close the connection and mark SessionLost.
	st, ep := ts.snapshotState()
	return dto.CancelExecutionResponse{
		TabID:        ts.tabID,
		ExecID:       execID,
		Status:       "canceled",
		SessionState: st,
		SessionEpoch: ep,
		Message:      "已发送服务端取消信号，正在等待语句状态确认",
	}
}

// ForceAbort explicitly tears down the physical connection when a query is unresponsive,
// transitioning the tab into SessionLost — NEVER downgrading SessionCommitUnknown.
func (ts *TabSession) ForceAbort() dto.TabSessionInfo {
	ts.stateMu.Lock()
	old, st, ep := ts.transitionToLostLocked(ts.inFlightMayCommit)
	info := ts.infoLocked()
	ts.stateMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	ts.mgr.emitStateChanged(dto.SessionStateEvent{
		TabID:        ts.tabID,
		ConnectionID: ts.connID,
		SessionState: st,
		SessionEpoch: ep,
	})
	return info
}

func (ts *TabSession) Close() error {
	ts.stateMu.Lock()
	ts.closed = true
	old, _, _ := ts.transitionToLostLocked(false)
	ts.stateMu.Unlock()
	if old != nil {
		return old.Close()
	}
	return nil
}

func toSplitDialect(engine dto.EngineType) sqlsplit.Dialect {
	switch engine {
	case dto.EnginePostgres:
		return sqlsplit.DialectPostgres
	case dto.EngineSQLite:
		return sqlsplit.DialectSQLite
	default:
		return sqlsplit.DialectMySQL
	}
}
