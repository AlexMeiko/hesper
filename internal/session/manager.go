package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
	"sql-manager/internal/sqlsplit"
)

// ConfigResolver resolves decrypted ResolvedConfig for a given connectionID (and optional override database).
type ConfigResolver func(ctx context.Context, connID string, overrideDB string) (driver.ResolvedConfig, error)

// Manager owns both tracks of external database connections:
//   - Track A: Metadata & bypass-control pools per (connID, dbName)
//   - Track B: Dedicated physical TabSessions per open query tab
type Manager struct {
	registry *driver.Registry
	resolve  ConfigResolver

	mu        sync.Mutex
	metaPools map[string]*metaPool   // key: connID + "\x00" + db
	tabs      map[string]*TabSession // key: tabID
}

func NewManager(registry *driver.Registry, resolve ConfigResolver) *Manager {
	return &Manager{
		registry:  registry,
		resolve:   resolve,
		metaPools: make(map[string]*metaPool),
		tabs:      make(map[string]*TabSession),
	}
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
	conn, err := pool.acquire(ctx)
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
	pool.release(conn, broken)
	return err
}

// AcquireBypassControlConn borrows an independent connection for sending server-side cancel commands (e.g. MySQL KILL QUERY).
func (m *Manager) AcquireBypassControlConn(ctx context.Context, connID, db string) (driver.Conn, func(broken bool), error) {
	pool, err := m.getOrCreateMetaPool(ctx, connID, db)
	if err != nil {
		return nil, nil, err
	}
	conn, err := pool.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn, func(broken bool) { pool.release(conn, broken) }, nil
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
	}
	m.tabs[tabID] = ts
	m.mu.Unlock()

	if err := ts.connectFresh(ctx, db, schema); err != nil {
		m.mu.Lock()
		delete(m.tabs, tabID)
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

// CloseConnection closes every Track A metadata pool and Track B tab session belonging to connID.
func (m *Manager) CloseConnection(connID string) {
	m.mu.Lock()
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
	m.mu.Unlock()

	for _, ts := range tabs {
		_ = ts.Close()
	}
	for _, p := range pools {
		p.closeAll()
	}
}

// CloseAll closes all connections across all pools and tabs (called on app shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	pools := make([]*metaPool, 0, len(m.metaPools))
	for k, p := range m.metaPools {
		pools = append(pools, p)
		delete(m.metaPools, k)
	}
	tabs := make([]*TabSession, 0, len(m.tabs))
	for id, ts := range m.tabs {
		tabs = append(tabs, ts)
		delete(m.tabs, id)
	}
	m.mu.Unlock()

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

func (m *Manager) getOrCreateMetaPool(_ context.Context, connID, db string) (*metaPool, error) {
	key := connID + "\x00" + db
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.metaPools[key]; ok {
		return p, nil
	}
	p := &metaPool{
		mgr:    m,
		connID: connID,
		db:     db,
		sem:    make(chan struct{}, 3),
		idle:   make([]driver.Conn, 0, 3),
	}
	m.metaPools[key] = p
	return p, nil
}

// --- Track A: Small Metadata / Control Pool ---

type metaPool struct {
	mgr    *Manager
	connID string
	db     string
	sem    chan struct{}

	mu     sync.Mutex
	idle   []driver.Conn
	closed bool
}

func (p *metaPool) acquire(ctx context.Context) (driver.Conn, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.sem
		return nil, dto.NewAppError(dto.ErrSessionLost, "连接池已关闭", "")
	}
	for len(p.idle) > 0 {
		c := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		p.mu.Unlock()
		if err := c.Ping(ctx); err == nil {
			return c, nil
		}
		_ = c.Close()
		p.mu.Lock()
	}
	p.mu.Unlock()

	cfg, err := p.mgr.resolve(ctx, p.connID, p.db)
	if err != nil {
		<-p.sem
		return nil, err
	}
	c, err := p.mgr.OpenTransient(ctx, cfg)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return c, nil
}

func (p *metaPool) release(c driver.Conn, broken bool) {
	defer func() { <-p.sem }()
	if c == nil {
		return
	}
	p.mu.Lock()
	if p.closed || broken || len(p.idle) >= 3 {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

func (p *metaPool) closeAll() {
	p.mu.Lock()
	p.closed = true
	conns := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, c := range conns {
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

	// stateMu guards live state inspected concurrently by CancelExecution / Info / ForceAbort.
	stateMu           sync.Mutex
	conn              driver.Conn
	state             dto.SessionState
	readOnly          bool
	currentDB         string
	currentSchema     string
	serverConnID      string
	caps              dto.Capabilities
	dialect           dto.EngineType
	activeExecID    string
	inFlightCancels int
	cancelDoneCh    chan struct{}
	cancelSent      bool
	cancelConfirmed bool
	cancelFailed    bool
	closed          bool
}

func (ts *TabSession) Info() dto.TabSessionInfo {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	return dto.TabSessionInfo{
		TabID:         ts.tabID,
		ConnectionID:  ts.connID,
		CurrentDB:     ts.currentDB,
		CurrentSchema: ts.currentSchema,
		SessionState:  ts.state,
		ServerConnID:  ts.serverConnID,
		ReadOnly:      ts.readOnly,
	}
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
	conn := ts.conn
	curDB := ts.currentDB
	ts.stateMu.Unlock()

	if st == dto.SessionCommitUnknown {
		return &dto.AppError{
			Code:      dto.ErrCommitOutcomeUnknown,
			Message:   "上次写入或提交结果未知，请先核对数据库数据并显式重新连接后再执行",
			RetrySafe: false,
			Session:   dto.SessionCommitUnknown,
		}
	}
	if st == dto.SessionLost || conn == nil {
		return &dto.AppError{
			Code:    dto.ErrSessionLost,
			Message: "原会话已结束（临时表与未提交事务已失效），请点击重新连接建立新会话",
			Session: dto.SessionLost,
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
			ts.currentSchema = targetSchema
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
	old := ts.conn
	ts.conn = c
	ts.state = dto.SessionAliveIdle
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
			Code:      dto.ErrCommitOutcomeUnknown,
			Message:   "上次写入或提交结果未知，请先核对数据并显式重新连接后再切换数据库",
			RetrySafe: false,
			Session:   dto.SessionCommitUnknown,
		}
	}
	if switcher, ok := conn.(driver.DatabaseSwitcher); ok {
		if err := switcher.SwitchDatabase(ctx, targetDB); err != nil {
			return err
		}
		ts.stateMu.Lock()
		ts.currentDB = targetDB
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
	ts.stateMu.Unlock()

	if conn == nil || st == dto.SessionLost {
		return &dto.AppError{
			Code:    dto.ErrSessionLost,
			Message: "当前会话已断开，请重新连接后再切换 Schema",
			Session: dto.SessionLost,
		}
	}
	if st == dto.SessionCommitUnknown {
		return &dto.AppError{
			Code:      dto.ErrCommitOutcomeUnknown,
			Message:   "上次写入或提交结果未知，请先核对数据并显式重新连接",
			RetrySafe: false,
			Session:   dto.SessionCommitUnknown,
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
	ts.currentSchema = targetSchema
	ts.stateMu.Unlock()
	return nil
}

// StatementOutcome holds the result of executing one statement inside TabSession.
type StatementOutcome struct {
	IsResultSet  bool
	AffectedRows int64
	DurationMs   int64
	SessionState dto.SessionState
	CurrentDB    string
	Error        *dto.AppError
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
		ts.stateMu.Unlock()
		return StatementOutcome{
			SessionState: dto.SessionLost,
			Error: &dto.AppError{
				Code:    dto.ErrSessionLost,
				Message: "原会话已结束（临时表与未提交事务已失效），请重新连接后再执行",
				Session: dto.SessionLost,
			},
		}
	}
	if ts.state == dto.SessionCommitUnknown {
		ts.stateMu.Unlock()
		return StatementOutcome{
			SessionState: dto.SessionCommitUnknown,
			Error: &dto.AppError{
				Code:      dto.ErrCommitOutcomeUnknown,
				Message:   "上次写入或提交结果未知，请先核对数据库数据并显式重新连接后再执行",
				RetrySafe: false,
				Session:   dto.SessionCommitUnknown,
			},
		}
	}

	conn := ts.conn
	wasInTx := ts.state == dto.SessionAliveInTx
	readOnly := ts.readOnly
	dialect := ts.dialect
	ts.activeExecID = execID
	ts.cancelSent = false
	ts.cancelConfirmed = false
	ts.cancelFailed = false
	ts.stateMu.Unlock()

	interruptedByCancel := false
	// Guarantee activeExecID is cleared AND any in-flight cancel fence has fully drained BEFORE execMu unlocks!
	defer func() {
		ts.finishExecutionAndDrainCancelFence(execID, interruptedByCancel)
		outcome.SessionState = ts.State()
	}()

	// Client read-only guardrail (in addition to real DB privileges & session read-only mode)
	if readOnly {
		splitDialect := toSplitDialect(dialect)
		if stmt.MutatesData || stmt.Kind == sqlsplit.KindWrite || stmt.Kind == sqlsplit.KindDDL || sqlsplit.IsReadOnlyEscapeSet(stmt.Text, splitDialect) {
			return StatementOutcome{
				SessionState: ts.State(),
				CurrentDB:    ts.Info().CurrentDB,
				Error: &dto.AppError{
					Code:    dto.ErrReadOnlyDenied,
					Message: "当前连接处于只读保护模式，已拦截写入或修改操作",
					Detail:  "client read-only guardrail blocked statement: " + stmt.FirstWord,
					Session: ts.State(),
				},
			}
		}
	}

	executor, ok := conn.(driver.QueryExecutor)
	if !ok {
		return StatementOutcome{
			SessionState: ts.State(),
			Error:        dto.NewAppError(dto.ErrInternal, "驱动未实现查询执行接口", ""),
		}
	}

	mayCommitOnServer := (stmt.Kind == sqlsplit.KindCommit) ||
		(!wasInTx && (stmt.MutatesData || stmt.Kind == sqlsplit.KindWrite || stmt.Kind == sqlsplit.KindDDL))

	start := time.Now()
	bound := driver.BoundStatement{SQL: stmt.Text}

	if stmt.ReturnsRows {
		stream, err := executor.Query(ctx, bound)
		if err != nil {
			appErr := ts.handleExecError(conn, err, wasInTx, mayCommitOnServer)
			interruptedByCancel = appErr.Code == dto.ErrStatementCanceled
			return StatementOutcome{
				DurationMs:   time.Since(start).Milliseconds(),
				SessionState: ts.State(),
				CurrentDB:    ts.Info().CurrentDB,
				Error:        appErr,
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
			return StatementOutcome{
				IsResultSet:  true,
				DurationMs:   time.Since(start).Milliseconds(),
				SessionState: ts.State(),
				CurrentDB:    ts.Info().CurrentDB,
				Error:        appErr,
			}
		}
		ts.updateStateAfterSuccess(stmt)
		return StatementOutcome{
			IsResultSet:  true,
			DurationMs:   time.Since(start).Milliseconds(),
			SessionState: ts.State(),
			CurrentDB:    ts.Info().CurrentDB,
		}
	}

	fact, err := executor.Exec(ctx, bound)
	if err != nil {
		appErr := ts.handleExecError(conn, err, wasInTx, mayCommitOnServer)
		interruptedByCancel = appErr.Code == dto.ErrStatementCanceled
		return StatementOutcome{
			DurationMs:   time.Since(start).Milliseconds(),
			SessionState: ts.State(),
			CurrentDB:    ts.Info().CurrentDB,
			Error:        appErr,
		}
	}

	ts.updateStateAfterSuccess(stmt)
	return StatementOutcome{
		IsResultSet:  false,
		AffectedRows: fact.AffectedRows,
		DurationMs:   time.Since(start).Milliseconds(),
		SessionState: ts.State(),
		CurrentDB:    ts.Info().CurrentDB,
	}
}

// finishExecutionAndDrainCancelFence runs while execMu is STILL held!
// 1. Clears activeExecID so no future late cancel for execID can pass validation.
// 2. If a cancel for execID ALREADY passed validation and is still in flight on the server,
//    blocks here until that cancel call completes.
// 3. Stops reusing ts.conn (closes it & sets SessionLost) whenever cancel failed OR an
//    unconfirmed async cancel (e.g., PG CancelRequest) was sent while the query exited naturally.
func (ts *TabSession) finishExecutionAndDrainCancelFence(execID string, interruptedByCancel bool) {
	ts.stateMu.Lock()
	if ts.activeExecID == execID {
		ts.activeExecID = ""
	}
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
	if unsafeReuse && ts.conn != nil {
		dropConn = ts.conn
		ts.conn = nil
		if ts.state != dto.SessionCommitUnknown {
			ts.state = dto.SessionLost
		}
	}
	ts.stateMu.Unlock()

	if dropConn != nil {
		_ = dropConn.Close()
	}
}

func (ts *TabSession) handleExecError(conn driver.Conn, err error, wasInTx bool, mayCommitOnServer bool) *dto.AppError {
	appErr := conn.ClassifyError(err, wasInTx)

	// CRITICAL: If an autocommit write/DDL or explicit COMMIT sent bytes on the wire
	// and lost the connection BEFORE receiving any response byte from the server,
	// we MUST report CommitOutcomeUnknown — NEVER assert rolled back, NEVER auto-retry!
	if mayCommitOnServer && conn.WriteSentWithoutResponse() && (appErr.Code == dto.ErrSessionLost || appErr.Code == dto.ErrConnectionFailed) {
		ts.stateMu.Lock()
		old := ts.conn
		ts.conn = nil
		ts.state = dto.SessionCommitUnknown
		ts.stateMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		return &dto.AppError{
			Code:      dto.ErrCommitOutcomeUnknown,
			Message:   "写入或提交已发出，但在收到数据库确认前连接中断，结果未知。请先核对数据库实际数据，切勿直接重试。",
			Detail:    err.Error(),
			RetrySafe: false,
			Session:   dto.SessionCommitUnknown,
		}
	}

	if appErr.Code == dto.ErrSessionLost {
		ts.stateMu.Lock()
		old := ts.conn
		ts.conn = nil
		ts.state = dto.SessionLost
		ts.stateMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		appErr.Session = dto.SessionLost
		return appErr
	}

	if appErr.Code == dto.ErrTxAborted || (wasInTx && ts.Dialect() == dto.EnginePostgres) {
		// In PostgreSQL, any error (including canceled statement) inside a transaction aborts the tx block.
		ts.stateMu.Lock()
		if ts.state == dto.SessionAliveInTx {
			ts.state = dto.SessionAliveFailedTx
		}
		appErr.Session = ts.state
		ts.stateMu.Unlock()
		return appErr
	}

	appErr.Session = ts.State()
	return appErr
}

func (ts *TabSession) updateStateAfterSuccess(stmt sqlsplit.Statement) {
	ts.stateMu.Lock()
	defer ts.stateMu.Unlock()
	switch stmt.Kind {
	case sqlsplit.KindBegin:
		ts.state = dto.SessionAliveInTx
	case sqlsplit.KindCommit, sqlsplit.KindRollback:
		ts.state = dto.SessionAliveIdle
	case sqlsplit.KindDDL:
		if !ts.caps.DDLTransactional && ts.state == dto.SessionAliveInTx {
			ts.state = dto.SessionAliveIdle
		}
	case sqlsplit.KindUse:
		if db := sqlsplit.ExtractUseDatabase(stmt.Text); db != "" {
			ts.currentDB = db
		}
	}
}

// CancelExecution attempts graceful server-side cancellation for one specific execID.
func (ts *TabSession) CancelExecution(ctx context.Context, execID string) dto.CancelExecutionResponse {
	ts.stateMu.Lock()
	if ts.activeExecID == "" || ts.activeExecID != execID || ts.conn == nil {
		st := ts.state
		ts.stateMu.Unlock()
		return dto.CancelExecutionResponse{
			TabID:        ts.tabID,
			ExecID:       execID,
			Status:       "noop",
			SessionState: st,
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

	canceler, ok := targetConn.(driver.ExecutionCanceler)
	if !ok {
		ts.stateMu.Lock()
		ts.cancelFailed = true
		ts.stateMu.Unlock()
		return dto.CancelExecutionResponse{
			TabID:        ts.tabID,
			ExecID:       execID,
			Status:       "session_lost",
			SessionState: dto.SessionLost,
			Message:      "当前驱动不支持服务端优雅取消，已停止复用原会话",
		}
	}

	cancelCtx, cancelTimeout := context.WithTimeout(ctx, 4*time.Second)
	defer cancelTimeout()

	var ctrlConn driver.Conn
	releaseCtrl := func(bool) {}
	if targetConn.Dialect() != dto.EngineSQLite {
		var err error
		ctrlConn, releaseCtrl, err = ts.mgr.AcquireBypassControlConn(cancelCtx, ts.connID, db)
		if err != nil {
			ts.stateMu.Lock()
			ts.cancelFailed = true
			ts.stateMu.Unlock()
			return dto.CancelExecutionResponse{
				TabID:        ts.tabID,
				ExecID:       execID,
				Status:       "session_lost",
				SessionState: dto.SessionLost,
				Message:      "无法建立旁路取消连接，原会话已停止复用",
			}
		}
	}

	confirmed, cancelErr := canceler.CancelInFlight(cancelCtx, ctrlConn)
	releaseCtrl(cancelErr != nil)

	if cancelErr != nil {
		ts.stateMu.Lock()
		ts.cancelFailed = true
		ts.stateMu.Unlock()
		return dto.CancelExecutionResponse{
			TabID:        ts.tabID,
			ExecID:       execID,
			Status:       "session_lost",
			SessionState: dto.SessionLost,
			Message:      "取消指令发送失败，无法确认原会话安全，已停止复用",
		}
	}

	if confirmed {
		ts.stateMu.Lock()
		ts.cancelConfirmed = true
		ts.stateMu.Unlock()
		return dto.CancelExecutionResponse{
			TabID:        ts.tabID,
			ExecID:       execID,
			Status:       "canceled",
			SessionState: ts.State(),
			Message:      "已通过旁路通道取消当前语句，原会话保持存活",
		}
	}

	// Async protocol cancel sent without server ACK (e.g., PostgreSQL CancelRequest).
	// If query A exits with ErrStatementCanceled, connection remains alive; if A finished naturally,
	// finishExecutionAndDrainCancelFence will close the connection and mark SessionLost.
	return dto.CancelExecutionResponse{
		TabID:        ts.tabID,
		ExecID:       execID,
		Status:       "canceled",
		SessionState: ts.State(),
		Message:      "已发送服务端取消信号，正在等待语句状态确认",
	}
}

// ForceAbort explicitly tears down the physical connection when a query is unresponsive,
// transitioning the tab into SessionLost.
func (ts *TabSession) ForceAbort() dto.TabSessionInfo {
	ts.stateMu.Lock()
	old := ts.conn
	ts.conn = nil
	ts.state = dto.SessionLost
	ts.activeExecID = ""
	ts.stateMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return ts.Info()
}

func (ts *TabSession) Close() error {
	ts.stateMu.Lock()
	ts.closed = true
	old := ts.conn
	ts.conn = nil
	ts.state = dto.SessionLost
	ts.activeExecID = ""
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
