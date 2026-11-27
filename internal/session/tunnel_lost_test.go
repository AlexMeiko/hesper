package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hesper/internal/driver"
	"hesper/internal/dto"
	"hesper/internal/sqlsplit"
)

type stubSessionDriver struct {
	mu       sync.Mutex
	openHook func(ctx context.Context, cfg driver.ResolvedConfig) (*stubSessionConn, error)
	opened   []*stubSessionConn
}

func (d *stubSessionDriver) Type() dto.EngineType { return dto.EngineMySQL }
func (d *stubSessionDriver) Open(ctx context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	if d.openHook != nil {
		c, err := d.openHook(ctx, cfg)
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		d.opened = append(d.opened, c)
		d.mu.Unlock()
		return c, nil
	}
	c := &stubSessionConn{tunnelGen: cfg.TunnelGen}
	d.mu.Lock()
	d.opened = append(d.opened, c)
	d.mu.Unlock()
	return c, nil
}

type stubSessionConn struct {
	tunnelGen  uint64
	closed     atomic.Bool
	writeSent  atomic.Bool
	pingHook   func(ctx context.Context) error
	execHook   func(ctx context.Context, stmt driver.BoundStatement) (driver.ExecFact, error)
}

func (c *stubSessionConn) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return errors.New("closed")
	}
	if c.pingHook != nil {
		return c.pingHook(ctx)
	}
	return nil
}
func (c *stubSessionConn) Close() error { c.closed.Store(true); return nil }
func (c *stubSessionConn) ServerFact(_ context.Context) (driver.ServerFact, error) {
	return driver.ServerFact{DetectedType: dto.EngineMySQL, ServerVersion: "8.4.0", CurrentDB: "app"}, nil
}
func (c *stubSessionConn) Capabilities(_ string) dto.Capabilities { return dto.Capabilities{} }
func (c *stubSessionConn) Dialect() dto.EngineType                { return dto.EngineMySQL }
func (c *stubSessionConn) WriteSentWithoutResponse() bool         { return c.writeSent.Load() }
func (c *stubSessionConn) ResetWireTracker()                      { c.writeSent.Store(false) }
func (c *stubSessionConn) ClassifyError(err error, _ bool) *dto.AppError {
	return dto.NewAppError(dto.ErrSessionLost, "connection dropped", err.Error())
}
func (c *stubSessionConn) Query(_ context.Context, _ driver.BoundStatement) (driver.RowStream, error) {
	return nil, errors.New("not implemented")
}
func (c *stubSessionConn) Exec(ctx context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	if c.execHook != nil {
		return c.execHook(ctx, stmt)
	}
	return driver.ExecFact{AffectedRows: 1}, nil
}

func TestMarkConnectionLostKeepsTabSessionProtectsGenerationsAndEmitsEvents(t *testing.T) {
	drv := &stubSessionDriver{}
	reg := driver.NewRegistry()
	reg.Register(drv)

	var currentGen atomic.Uint64
	currentGen.Store(1)

	mgr := NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EngineMySQL,
			Database:     db,
			TunnelGen:    currentGen.Load(),
		}, nil
	})

	var events []dto.SessionStateEvent
	var evMu sync.Mutex
	mgr.SetOnStateChanged(func(evt dto.SessionStateEvent) {
		evMu.Lock()
		events = append(events, evt)
		evMu.Unlock()
	})

	ts, err := mgr.GetOrCreateTabSession(context.Background(), "tab-1", "conn-1", "app", "")
	if err != nil {
		t.Fatalf("GetOrCreateTabSession: %v", err)
	}
	initialEpoch := ts.Info().SessionEpoch
	if initialEpoch <= 0 {
		t.Fatalf("expected positive initial sessionEpoch, got %d", initialEpoch)
	}

	// Mark tunnel gen=1 lost -> TabSession MUST remain in manager (not deleted!) and enter SessionLost.
	mgr.MarkConnectionLost("conn-1", 1)

	gotTS, exists := mgr.GetTabSession("tab-1")
	if !exists || gotTS != ts {
		t.Fatalf("MarkConnectionLost MUST keep TabSession in manager for explicit reconnect")
	}
	lostInfo := ts.Info()
	if lostInfo.SessionState != dto.SessionLost {
		t.Fatalf("expected SessionLost, got %s", lostInfo.SessionState)
	}
	if lostInfo.SessionEpoch <= initialEpoch {
		t.Fatalf("expected sessionEpoch to advance (%d <= %d)", lostInfo.SessionEpoch, initialEpoch)
	}

	// Reconnect on new tunnel gen=2 -> delayed MarkConnectionLost(conn-1, 1) MUST NOT kill gen=2!
	currentGen.Store(2)
	if err := ts.Reconnect(context.Background(), "app", ""); err != nil {
		t.Fatalf("Reconnect gen=2: %v", err)
	}
	reconnEpoch := ts.Info().SessionEpoch
	if ts.Info().SessionState != dto.SessionAliveIdle || reconnEpoch <= lostInfo.SessionEpoch {
		t.Fatalf("unexpected state after reconnect: %+v", ts.Info())
	}

	mgr.MarkConnectionLost("conn-1", 1) // Stale generation!
	if ts.Info().SessionState != dto.SessionAliveIdle || ts.Info().SessionEpoch != reconnEpoch {
		t.Fatalf("stale MarkConnectionLost(gen=1) MUST NOT disturb gen=2 session, got %+v", ts.Info())
	}

	// Closing & recreating the SAME tabID MUST keep sessionEpoch strictly increasing (never reset to 0).
	if err := mgr.CloseTab("tab-1"); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	tsRecreated, err := mgr.GetOrCreateTabSession(context.Background(), "tab-1", "conn-1", "app", "")
	if err != nil {
		t.Fatalf("recreate tab-1: %v", err)
	}
	if tsRecreated.Info().SessionEpoch <= reconnEpoch {
		t.Fatalf("recreated tabID MUST NOT reset sessionEpoch (%d <= %d)", tsRecreated.Info().SessionEpoch, reconnEpoch)
	}

	evMu.Lock()
	if len(events) == 0 || events[0].TabID != "tab-1" || events[0].SessionState != dto.SessionLost {
		t.Fatalf("expected SessionStateEvent for tab-1 SessionLost, got %+v", events)
	}
	evMu.Unlock()
}

func TestCommitOutcomeUnknownInFlightAndForceAbortNeverDowngrades(t *testing.T) {
	inFlightEntered := make(chan struct{})
	releaseExec := make(chan struct{})

	drv := &stubSessionDriver{
		openHook: func(_ context.Context, cfg driver.ResolvedConfig) (*stubSessionConn, error) {
			c := &stubSessionConn{tunnelGen: cfg.TunnelGen}
			c.execHook = func(_ context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
				if stmt.SQL == "COMMIT" {
					c.writeSent.Store(true)
					close(inFlightEntered)
					<-releaseExec
					return driver.ExecFact{}, errors.New("EOF on tunnel")
				}
				return driver.ExecFact{AffectedRows: 1}, nil
			}
			return c, nil
		},
	}
	reg := driver.NewRegistry()
	reg.Register(drv)

	mgr := NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{ConnectionID: connID, Engine: dto.EngineMySQL, Database: db, TunnelGen: 1}, nil
	})

	ts, err := mgr.GetOrCreateTabSession(context.Background(), "tab-commit", "conn-c", "app", "")
	if err != nil {
		t.Fatalf("open tab: %v", err)
	}
	// Enter transaction first.
	beginOut := ts.ExecuteStatement(context.Background(), "e-begin", sqlsplit.Statement{Text: "BEGIN", Kind: sqlsplit.KindBegin}, nil)
	if beginOut.Error != nil || beginOut.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected SessionAliveInTx, got %+v", beginOut)
	}

	// Start COMMIT; while COMMIT bytes are sent on wire with zero response bytes, drop the SSH tunnel!
	outcomeCh := make(chan StatementOutcome, 1)
	go func() {
		outcomeCh <- ts.ExecuteStatement(context.Background(), "e-commit", sqlsplit.Statement{Text: "COMMIT", Kind: sqlsplit.KindCommit}, nil)
	}()

	<-inFlightEntered
	mgr.MarkConnectionLost("conn-c", 1)
	// Immediately after MarkConnectionLost, state MUST already be SessionCommitUnknown (not SessionLost!).
	if got := ts.Info().SessionState; got != dto.SessionCommitUnknown {
		t.Fatalf("in-flight COMMIT tunnel drop MUST enter SessionCommitUnknown immediately, got %s", got)
	}

	close(releaseExec)
	out := <-outcomeCh
	if out.SessionState != dto.SessionCommitUnknown || out.Error == nil || out.Error.Code != dto.ErrCommitOutcomeUnknown {
		t.Fatalf("expected CommitOutcomeUnknown outcome, got %+v", out)
	}

	// CRITICAL INVARIANT: ForceAbort after SessionCommitUnknown MUST NOT downgrade to SessionLost!
	afterAbort := ts.ForceAbort()
	if afterAbort.SessionState != dto.SessionCommitUnknown {
		t.Fatalf("ForceAbort MUST NOT downgrade SessionCommitUnknown to %s", afterAbort.SessionState)
	}
	if ts.Info().SessionState != dto.SessionCommitUnknown {
		t.Fatalf("session state MUST remain SessionCommitUnknown after ForceAbort")
	}

	// Repeated MarkConnectionLost also MUST NOT downgrade SessionCommitUnknown!
	mgr.MarkConnectionLost("conn-c", 0)
	if ts.Info().SessionState != dto.SessionCommitUnknown {
		t.Fatalf("repeated MarkConnectionLost MUST NOT downgrade SessionCommitUnknown")
	}
}

func TestConnectFreshAndUpdateStateAfterSuccessRespectLostTunnel(t *testing.T) {
	dialEntered := make(chan struct{})
	dialRelease := make(chan struct{})
	execReturned := make(chan struct{})

	var mode atomic.Int32 // 1 = block Open, 2 = race updateStateAfterSuccess
	drv := &stubSessionDriver{
		openHook: func(_ context.Context, cfg driver.ResolvedConfig) (*stubSessionConn, error) {
			if mode.Load() == 1 {
				close(dialEntered)
				<-dialRelease
			}
			c := &stubSessionConn{tunnelGen: cfg.TunnelGen}
			c.execHook = func(_ context.Context, _ driver.BoundStatement) (driver.ExecFact, error) {
				if mode.Load() == 2 {
					// Tunnel drops right as statement finishes on the wire!
					close(execReturned)
					time.Sleep(40 * time.Millisecond)
				}
				return driver.ExecFact{AffectedRows: 1}, nil
			}
			return c, nil
		},
	}
	reg := driver.NewRegistry()
	reg.Register(drv)

	var gen atomic.Uint64
	gen.Store(1)
	mgr := NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{ConnectionID: connID, Engine: dto.EngineMySQL, Database: db, TunnelGen: gen.Load()}, nil
	})

	// 1. connectFresh racing with MarkConnectionLost(gen=1): MUST reject installing dead tunnel's conn!
	mode.Store(1)
	errCh := make(chan error, 1)
	go func() {
		_, err := mgr.GetOrCreateTabSession(context.Background(), "tab-race-open", "conn-r", "app", "")
		errCh <- err
	}()
	<-dialEntered
	mgr.MarkConnectionLost("conn-r", 1)
	close(dialRelease)
	if err := <-errCh; err == nil {
		t.Fatalf("connectFresh MUST fail when tunnel gen=1 was marked lost before install")
	}

	// 2. updateStateAfterSuccess racing with MarkConnectionLost(gen=2): MUST NOT overwrite SessionLost back to Alive!
	gen.Store(2)
	mode.Store(0)
	ts, err := mgr.GetOrCreateTabSession(context.Background(), "tab-race-exec", "conn-r", "app", "")
	if err != nil {
		t.Fatalf("open tab-race-exec: %v", err)
	}
	mode.Store(2)
	// Swap execHook behavior on the live conn directly.
	drv.mu.Lock()
	liveConn := drv.opened[len(drv.opened)-1]
	drv.mu.Unlock()
	liveConn.execHook = func(_ context.Context, _ driver.BoundStatement) (driver.ExecFact, error) {
		mgr.MarkConnectionLost("conn-r", 2)
		return driver.ExecFact{AffectedRows: 1}, nil
	}

	out := ts.ExecuteStatement(context.Background(), "e-begin-race", sqlsplit.Statement{Text: "BEGIN", Kind: sqlsplit.KindBegin}, nil)
	if out.SessionState != dto.SessionLost || ts.Info().SessionState != dto.SessionLost {
		t.Fatalf("late updateStateAfterSuccess MUST NOT overwrite SessionLost back to alive, got %s", out.SessionState)
	}
}

func TestTrackAMetaPoolGenerationEvictionAndAcquireGuards(t *testing.T) {
	pingEntered := make(chan struct{})
	pingRelease := make(chan struct{})
	dialEntered := make(chan struct{})
	dialRelease := make(chan struct{})

	var blockPing atomic.Bool
	var blockDial atomic.Bool

	drv := &stubSessionDriver{
		openHook: func(_ context.Context, cfg driver.ResolvedConfig) (*stubSessionConn, error) {
			if blockDial.Load() {
				close(dialEntered)
				<-dialRelease
			}
			c := &stubSessionConn{tunnelGen: cfg.TunnelGen}
			c.pingHook = func(_ context.Context) error {
				if blockPing.Load() {
					blockPing.Store(false)
					close(pingEntered)
					<-pingRelease
				}
				return nil
			}
			return c, nil
		},
	}
	reg := driver.NewRegistry()
	reg.Register(drv)

	var gen atomic.Uint64
	gen.Store(1)
	mgr := NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{ConnectionID: connID, Engine: dto.EngineMySQL, Database: db, TunnelGen: gen.Load()}, nil
	})

	// 1. Put one gen=1 connection into idle pool.
	if err := mgr.WithMetaConn(context.Background(), "conn-a", "app", func(_ driver.Conn) error { return nil }); err != nil {
		t.Fatalf("initial WithMetaConn: %v", err)
	}

	// 2. Acquire idle gen=1 conn; while Ping is in flight, MarkConnectionLost(conn-a, 1) runs!
	// Post-Ping check MUST reject handing out the invalidated connection!
	blockPing.Store(true)
	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.WithMetaConn(context.Background(), "conn-a", "app", func(_ driver.Conn) error { return nil })
	}()
	<-pingEntered
	mgr.MarkConnectionLost("conn-a", 1)
	close(pingRelease)
	if err := <-errCh; err == nil {
		t.Fatalf("Track A acquire MUST reject idle conn whose tunnelGen=1 was invalidated during Ping")
	}

	// 3. Fresh OpenTransient in Track A acquire racing with MarkConnectionLost(conn-a, 2) MUST also be rejected!
	gen.Store(2)
	blockDial.Store(true)
	errCh2 := make(chan error, 1)
	go func() {
		errCh2 <- mgr.WithMetaConn(context.Background(), "conn-a", "app", func(_ driver.Conn) error { return nil })
	}()
	<-dialEntered
	mgr.MarkConnectionLost("conn-a", 2)
	close(dialRelease)
	if err := <-errCh2; err == nil {
		t.Fatalf("Track A acquire MUST reject fresh conn whose tunnelGen=2 was invalidated during OpenTransient")
	}

	// 4. Stale MarkConnectionLost(gen=2) MUST NOT evict an already-active gen=3 pooled connection.
	gen.Store(3)
	blockDial.Store(false)
	var conn3 *stubSessionConn
	if err := mgr.WithMetaConn(context.Background(), "conn-a", "app", func(c driver.Conn) error {
		conn3 = c.(*stubSessionConn)
		return nil
	}); err != nil {
		t.Fatalf("gen=3 WithMetaConn: %v", err)
	}
	mgr.MarkConnectionLost("conn-a", 2)
	if conn3.closed.Load() {
		t.Fatalf("stale MarkConnectionLost(gen=2) MUST NOT close gen=3 Track A pooled connection")
	}
}
