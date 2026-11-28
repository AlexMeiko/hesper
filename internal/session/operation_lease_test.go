package session_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hesper/internal/driver"
	"hesper/internal/driver/fake"
	"hesper/internal/dto"
	"hesper/internal/session"
)

func TestAcquireOperationSourceRefAndConfigVersionMismatch(t *testing.T) {
	reg := driver.NewRegistry()
	opener := fake.NewOpener(fake.FaultBehavior{})
	reg.Register(opener)

	var cfgVer atomic.Uint64
	cfgVer.Store(1)
	var hostMu sync.Mutex
	host := "127.0.0.1"

	mgr := session.NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		hostMu.Lock()
		h := host
		hostMu.Unlock()
		return driver.ResolvedConfig{
			ConnectionID:  connID,
			Engine:        dto.EngineFake,
			Host:          h,
			Port:          3306,
			Database:      db,
			ConfigVersion: cfgVer.Load(),
		}, nil
	})

	// 1. Preview acquires with expected == nil and gets authoritative SourceRef directly from lease.
	lease, err := mgr.AcquireOperation(context.Background(), "conn-1", "demo", nil)
	if err != nil {
		t.Fatalf("Preview AcquireOperation failed: %v", err)
	}
	src := lease.SourceRef()
	if src.ConnectionID != "conn-1" || src.ConfigVersion != 1 || src.LifecycleEpoch != 0 || src.Target.Database != "demo" {
		t.Fatalf("unexpected Preview SourceRef: %+v", src)
	}
	lease.Release()
	if opener.Opened.Load() != 1 || opener.Closed.Load() != 1 {
		t.Fatalf("expected 1 open / 1 close after preview release, got open=%d close=%d", opener.Opened.Load(), opener.Closed.Load())
	}

	// 2. ConfigVersion bump after Preview MUST reject BEFORE dialing (zero extra Open calls).
	cfgVer.Store(2)
	_, err = mgr.AcquireOperation(context.Background(), "conn-1", "demo", &src)
	var ae *dto.AppError
	if !errors.As(err, &ae) || ae.Code != dto.ErrPlanInvalidated {
		t.Fatalf("expected ErrPlanInvalidated on ConfigVersion mismatch, got %v", err)
	}
	if opener.Opened.Load() != 1 {
		t.Fatalf("expected zero dial on ConfigVersion mismatch, got Opened=%d", opener.Opened.Load())
	}

	// 3. Target host mismatch even with same ConfigVersion MUST reject BEFORE dialing.
	cfgVer.Store(1)
	hostMu.Lock()
	host = "10.0.0.99"
	hostMu.Unlock()
	_, err = mgr.AcquireOperation(context.Background(), "conn-1", "demo", &src)
	if !errors.As(err, &ae) || ae.Code != dto.ErrPlanInvalidated {
		t.Fatalf("expected ErrPlanInvalidated on target host mismatch, got %v", err)
	}
	if opener.Opened.Load() != 1 {
		t.Fatalf("expected zero dial on target mismatch, got Opened=%d", opener.Opened.Load())
	}
}

func TestAcquireOperationConcurrentCloseConnectionDuringDial(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tunnelGen uint64
	}{
		{name: "direct_connection", tunnelGen: 0},
		{name: "ssh_tunnel_connection", tunnelGen: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := driver.NewRegistry()
			opener := fake.NewOpener(fake.FaultBehavior{OpenDelay: 120 * time.Millisecond})
			reg.Register(opener)

			mgr := session.NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
				return driver.ResolvedConfig{
					ConnectionID:  connID,
					Engine:        dto.EngineFake,
					Host:          "127.0.0.1",
					Port:          3306,
					Database:      db,
					ConfigVersion: 1,
					TunnelGen:     tc.tunnelGen,
				}, nil
			})

			expected := driver.SourceRef{
				ConnectionID:   "conn-race",
				ConfigVersion:  1,
				LifecycleEpoch: mgr.CurrentLifecycleEpoch("conn-race"),
				Target: driver.ResolvedTarget{
					Engine:   dto.EngineFake,
					Host:     "127.0.0.1",
					Port:     3306,
					Database: "demo",
				},
			}

			errCh := make(chan error, 1)
			go func() {
				lease, err := mgr.AcquireOperation(context.Background(), "conn-race", "demo", &expected)
				if lease != nil {
					lease.Release()
				}
				errCh <- err
			}()

			// Wait briefly until AcquireOperation is inside OpenDelay, then close connection.
			time.Sleep(30 * time.Millisecond)
			mgr.CloseConnection("conn-race")

			err := <-errCh
			var ae *dto.AppError
			if !errors.As(err, &ae) || ae.Code != dto.ErrPlanInvalidated {
				t.Fatalf("expected ErrPlanInvalidated when CloseConnection races dial, got %v", err)
			}
			if mgr.CurrentLifecycleEpoch("conn-race") != 1 {
				t.Fatalf("expected lifecycle epoch 1 after CloseConnection, got %d", mgr.CurrentLifecycleEpoch("conn-race"))
			}
			if opener.Opened.Load() != opener.Closed.Load() {
				t.Fatalf("leaked connection during CloseConnection race: opened=%d closed=%d", opener.Opened.Load(), opener.Closed.Load())
			}
		})
	}
}

func TestMarkConnectionLostCancelsOnlyMatchingTunnelGenOps(t *testing.T) {
	reg := driver.NewRegistry()
	opener := fake.NewOpener(fake.FaultBehavior{})
	reg.Register(opener)

	var gen atomic.Uint64
	mgr := session.NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{
			ConnectionID:  connID,
			Engine:        dto.EngineFake,
			Host:          "127.0.0.1",
			Port:          3306,
			Database:      db,
			ConfigVersion: 1,
			TunnelGen:     gen.Load(),
		}, nil
	})

	gen.Store(1)
	leaseOld, err := mgr.AcquireOperation(context.Background(), "conn-ssh", "demo", nil)
	if err != nil {
		t.Fatalf("acquire gen 1: %v", err)
	}
	defer leaseOld.Release()

	gen.Store(2)
	leaseNew, err := mgr.AcquireOperation(context.Background(), "conn-ssh", "demo", nil)
	if err != nil {
		t.Fatalf("acquire gen 2: %v", err)
	}
	defer leaseNew.Release()

	// Lose ONLY tunnel generation 1.
	mgr.MarkConnectionLost("conn-ssh", 1)

	select {
	case <-leaseOld.Context().Done():
	case <-time.After(time.Second):
		t.Fatalf("expected gen 1 operation context to be canceled by MarkConnectionLost(1)")
	}
	if err := leaseOld.Conn().Ping(leaseOld.Context()); err == nil {
		t.Fatalf("expected gen 1 operation ping with lease context to fail after cancel")
	}
	// Lease holder owns serial Close() via Release() (avoiding concurrent Close() races on pgx/sql).
	leaseOld.Release()
	if opener.Closed.Load() != 1 {
		t.Fatalf("expected gen 1 connection closed once on Release(), got Closed=%d", opener.Closed.Load())
	}

	// Gen 2 operation MUST remain completely untouched and alive!
	if leaseNew.Context().Err() != nil {
		t.Fatalf("gen 2 operation MUST NOT be canceled by MarkConnectionLost(1): %v", leaseNew.Context().Err())
	}
	if err := leaseNew.Conn().Ping(leaseNew.Context()); err != nil {
		t.Fatalf("gen 2 physical connection MUST stay open: %v", err)
	}
}
