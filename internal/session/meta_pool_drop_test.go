package session_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hesper/internal/driver"
	"hesper/internal/driver/fake"
	"hesper/internal/dto"
	"hesper/internal/session"
)

func TestMetaPoolMaxIdleCapAndTTLReaping(t *testing.T) {
	ctx := context.Background()
	reg := driver.NewRegistry()
	opener := fake.NewOpener(fake.FaultBehavior{})
	reg.Register(opener)

	mgr := session.NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EngineFake,
			Database:     db,
		}, nil
	})
	defer mgr.CloseAll()

	// Configure fast idle TTL for deterministic test execution; maxIdle=1 matches production default.
	mgr.SetMetaPoolPolicy(1, 60*time.Millisecond)

	// Simulate ExplorerSidebar's 3 concurrent metadata RPCs (ListTables, ListViews, ListSequences).
	ready := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := mgr.WithMetaConn(ctx, "pg-1", "target_db", func(_ driver.Conn) error {
				ready <- struct{}{}
				<-release
				return nil
			})
			if err != nil {
				t.Errorf("WithMetaConn failed: %v", err)
			}
		}()
	}

	for i := 0; i < 3; i++ {
		<-ready
	}
	if got := opener.Opened.Load(); got != 3 {
		t.Fatalf("expected 3 concurrent opened conns, got %d", got)
	}
	close(release)
	wg.Wait()

	// Excess 2 burst connections must be closed immediately (maxIdle=1).
	if closed := opener.Closed.Load(); closed != 2 {
		t.Fatalf("expected 2 excess burst conns closed immediately (maxIdle=1), got %d closed", closed)
	}

	// Remaining 1 idle connection must be reaped automatically after idleTTL expires.
	deadline := time.Now().Add(2 * time.Second)
	for opener.Closed.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if closed := opener.Closed.Load(); closed != 3 {
		t.Fatalf("expected final idle conn to be reaped after TTL, got %d/3 closed", closed)
	}
}

func TestCloseDatabaseMetaPoolMatchesExplicitAndDefaultDB(t *testing.T) {
	ctx := context.Background()
	reg := driver.NewRegistry()
	opener := fake.NewOpener(fake.FaultBehavior{})
	reg.Register(opener)

	mgr := session.NewManager(reg, func(_ context.Context, connID, db string) (driver.ResolvedConfig, error) {
		resolved := db
		if resolved == "" {
			// Default connection DB happens to be the one we'll drop.
			resolved = "blue_m2"
		}
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EngineFake,
			Database:     resolved,
		}, nil
	})
	defer mgr.CloseAll()

	// Warm up 3 pools: default ("" -> blue_m2), explicit ("blue_m2"), and an unrelated ("other_db").
	for _, db := range []string{"", "blue_m2", "other_db"} {
		if err := mgr.WithMetaConn(ctx, "c1", db, func(_ driver.Conn) error { return nil }); err != nil {
			t.Fatalf("warm pool %q: %v", db, err)
		}
	}
	if opener.Opened.Load() != 3 || opener.Closed.Load() != 0 {
		t.Fatalf("expected 3 open idle pools, opened=%d closed=%d", opener.Opened.Load(), opener.Closed.Load())
	}

	closedPools := mgr.CloseDatabaseMetaPool("c1", "blue_m2")
	if closedPools != 2 {
		t.Fatalf("expected 2 pools (default + explicit) closed for blue_m2, got %d", closedPools)
	}
	if opener.Closed.Load() != 2 {
		t.Fatalf("expected 2 physical conns closed (other_db must remain warm), got %d", opener.Closed.Load())
	}
}
