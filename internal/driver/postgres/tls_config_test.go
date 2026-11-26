package postgres

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
)

func TestConfigureTLSClearsFallbacksAndHonorsModes(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGHOST", "10.255.255.254")
	t.Setenv("PGPORT", "6543")

	for _, mode := range []string{"disable", "require", "verify-ca", "verify-full"} {
		pgCfg, err := pgx.ParseConfig("")
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		if err := configureTLS(pgCfg, dto.TLSConfig{Mode: mode}, "db.internal", 5432); err != nil {
			t.Fatalf("configureTLS(%s): %v", mode, err)
		}
		if len(pgCfg.Fallbacks) != 0 {
			t.Fatalf("mode=%s MUST have zero Fallbacks, got %d", mode, len(pgCfg.Fallbacks))
		}
		if mode == "disable" && pgCfg.TLSConfig != nil {
			t.Fatalf("disable mode must set TLSConfig=nil")
		}
		if mode != "disable" && pgCfg.TLSConfig == nil {
			t.Fatalf("mode=%s must set non-nil TLSConfig", mode)
		}
		if mode == "verify-full" && pgCfg.TLSConfig.ServerName != "db.internal" {
			t.Fatalf("verify-full ServerName=%q, want db.internal", pgCfg.TLSConfig.ServerName)
		}
	}

	pgCfg, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if err := configureTLS(pgCfg, dto.TLSConfig{Mode: "prefer"}, "db.internal", 15432); err != nil {
		t.Fatalf("configureTLS(prefer): %v", err)
	}
	if pgCfg.TLSConfig == nil {
		t.Fatalf("prefer mode must try TLS first")
	}
	if len(pgCfg.Fallbacks) != 1 {
		t.Fatalf("prefer mode must configure exactly 1 fallback, got %d", len(pgCfg.Fallbacks))
	}
	fb := pgCfg.Fallbacks[0]
	if fb.Host != "db.internal" || fb.Port != 15432 || fb.TLSConfig != nil {
		t.Fatalf("prefer fallback must point to same target plaintext, got %+v", fb)
	}
}

// TestStrictTLSModesRefusePlaintextFallback starts a minimal PG wire stub that replies 'N'
// (TLS unsupported) to SSLRequest, verifying strict TLS modes never retry plaintext while
// prefer falls back to the exact same host:port.
func TestStrictTLSModesRefusePlaintextFallback(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGHOST", "127.0.0.1")

	startStub := func(t *testing.T) (host string, port int, plaintextAttempts *atomic.Int32) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		var plain atomic.Int32
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(conn net.Conn) {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
					hdr := make([]byte, 8)
					if _, err := io.ReadFull(conn, hdr); err != nil {
						return
					}
					length := binary.BigEndian.Uint32(hdr[0:4])
					code := binary.BigEndian.Uint32(hdr[4:8])
					if length == 8 && code == 80877103 { // PG SSLRequest
						_, _ = conn.Write([]byte{'N'})
						return
					}
					// Any non-SSLRequest startup packet means plaintext fallback occurred!
					plain.Add(1)
				}(c)
			}
		}()

		h, pStr, _ := net.SplitHostPort(ln.Addr().String())
		p, _ := strconv.Atoi(pStr)
		return h, p, &plain
	}

	opener := NewOpener()
	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		h, p, plain := startStub(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := opener.Open(ctx, driver.ResolvedConfig{
			Engine:   dto.EnginePostgres,
			Host:     h,
			Port:     p,
			Username: "postgres",
			Database: "postgres",
			TLS:      dto.TLSConfig{Mode: mode},
		})
		cancel()
		if err == nil {
			t.Fatalf("mode=%s unexpectedly succeeded against non-TLS stub", mode)
		}
		if plain.Load() != 0 {
			t.Fatalf("mode=%s MUST NOT fall back to plaintext, got %d plaintext startup attempts", mode, plain.Load())
		}
	}

	// prefer MUST attempt plaintext fallback to the same target after SSLRequest receives 'N'.
	h, p, plain := startStub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = opener.Open(ctx, driver.ResolvedConfig{
		Engine:   dto.EnginePostgres,
		Host:     h,
		Port:     p,
		Username: "postgres",
		Database: "postgres",
		TLS:      dto.TLSConfig{Mode: "prefer"},
	})
	cancel()
	if plain.Load() == 0 {
		t.Fatalf("prefer mode should fall back to plaintext against same target after SSLRequest='N'")
	}
}

func TestDialContextDelegatesHostnameLookupToRemote(t *testing.T) {
	opener := NewOpener()
	var dialedAddr string
	sentinel := errors.New("reached custom dialer")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := opener.Open(ctx, driver.ResolvedConfig{
		Engine:   dto.EnginePostgres,
		Host:     "bastion-only-db.nonexistent.invalid",
		Port:     5432,
		Username: "postgres",
		Database: "postgres",
		TLS:      dto.TLSConfig{Mode: "disable"},
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialedAddr = addr
			return nil, sentinel
		},
	})
	if err == nil {
		t.Fatalf("expected error from sentinel dialer")
	}
	if dialedAddr != "bastion-only-db.nonexistent.invalid:5432" {
		t.Fatalf("expected DialContext to receive unresolved bastion-only hostname, got %q (err=%v)", dialedAddr, err)
	}
}
