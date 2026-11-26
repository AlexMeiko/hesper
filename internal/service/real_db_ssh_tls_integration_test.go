package service_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"sql-manager/internal/driver"
	"sql-manager/internal/driver/mysql"
	"sql-manager/internal/driver/postgres"
	"sql-manager/internal/dto"
	"sql-manager/internal/repository"
	"sql-manager/internal/resultbuf"
	"sql-manager/internal/secret"
	"sql-manager/internal/service"
	"sql-manager/internal/session"
	"sql-manager/internal/tunnel"
)

type realDBTarget struct {
	name            string
	engine          dto.EngineType
	host            string
	port            int
	remoteAliasHost string
	remoteAliasPort int
	rwUser          string
	rwPass          string
	mtlsUser        string
	mtlsPass        string
	database        string
	verifySQL       string
	sleepSQL        string
}

func realDBTargets() []realDBTarget {
	return []realDBTarget{
		{
			name:            "MySQL84",
			engine:          dto.EngineMySQL,
			host:            "127.0.0.1",
			port:            13306,
			remoteAliasHost: "mysql-remote.internal",
			remoteAliasPort: 3306,
			rwUser:          "sqlm_rw",
			rwPass:          "sqlm_rw_pw",
			mtlsUser:        "sqlm_mtls",
			mtlsPass:        "sqlm_mtls_pw",
			database:        "sqlm_demo",
			verifySQL:       "SELECT COUNT(*) FROM customers",
			sleepSQL:        "SELECT SLEEP(5) FROM customers",
		},
		{
			name:            "MariaDB114",
			engine:          dto.EngineMariaDB,
			host:            "127.0.0.1",
			port:            13307,
			remoteAliasHost: "mariadb-remote.internal",
			remoteAliasPort: 3306,
			rwUser:          "sqlm_rw",
			rwPass:          "sqlm_rw_pw",
			mtlsUser:        "sqlm_mtls",
			mtlsPass:        "sqlm_mtls_pw",
			database:        "sqlm_demo",
			verifySQL:       "SELECT COUNT(*) FROM customers",
			sleepSQL:        "SELECT SLEEP(5)",
		},
		{
			name:            "Postgres16",
			engine:          dto.EnginePostgres,
			host:            "127.0.0.1",
			port:            15432,
			remoteAliasHost: "pg-remote.internal",
			remoteAliasPort: 5432,
			rwUser:          "sqlm_rw",
			rwPass:          "sqlm_rw_pw",
			mtlsUser:        "sqlm_mtls",
			mtlsPass:        "sqlm_mtls_pw",
			database:        "sqlm_demo",
			verifySQL:       "SELECT COUNT(*) FROM public.accounts",
			sleepSQL:        "SELECT pg_sleep(5)",
		},
	}
}

func requireRealDBsAndCerts(t *testing.T) string {
	t.Helper()
	strict := os.Getenv("SQLM_REQUIRE_REAL_DB") == "1"
	failOrSkip := func(format string, args ...any) {
		if strict {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("failed to resolve caller path")
	}
	certDir := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "test", ".tls-certs"))
	for _, name := range []string{"ca.crt", "other-ca.crt", "client.crt", "client.key", "wrong-client.crt", "wrong-client.key"} {
		if _, err := os.Stat(filepath.Join(certDir, name)); err != nil {
			failOrSkip("TLS fixture %s missing (%v); run ./scripts/gen-test-tls-certs.sh and start docker-compose.tls.test.yml", name, err)
		}
	}

	for _, target := range realDBTargets() {
		addr := net.JoinHostPort(target.host, strconv.Itoa(target.port))
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			failOrSkip("real %s not reachable at %s (%v); start docker-compose.test.yml + docker-compose.tls.test.yml", target.name, addr, err)
		}
		_ = c.Close()
	}
	return certDir
}

// forwardingSSHServer is a real RFC 4254 SSH server supporting password auth, keepalive replies,
// and direct-tcpip forwarding with optional remote-only hostname alias mapping.
type forwardingSSHServer struct {
	host             string
	port             int
	algorithm        string
	fingerprint      string
	routes           map[string]string
	directTCPIPCount atomic.Int64

	ln       net.Listener
	mu       sync.Mutex
	conns    map[*ssh.ServerConn]struct{}
	rawConns map[net.Conn]struct{}
}

func startForwardingSSHServer(t *testing.T, password string, routes map[string]string) *forwardingSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ssh host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}

	srvCfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == password {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("invalid password")
		},
	}
	srvCfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ssh: %v", err)
	}

	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	s := &forwardingSSHServer{
		host:        h,
		port:        p,
		algorithm:   signer.PublicKey().Type(),
		fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		routes:      routes,
		ln:          ln,
		conns:       make(map[*ssh.ServerConn]struct{}),
		rawConns:    make(map[net.Conn]struct{}),
	}

	t.Cleanup(func() {
		_ = ln.Close()
		s.KillAllConnections()
	})

	go func() {
		for {
			raw, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go s.serveConn(raw, srvCfg)
		}
	}()
	return s
}

func (s *forwardingSSHServer) serveConn(raw net.Conn, cfg *ssh.ServerConfig) {
	s.mu.Lock()
	s.rawConns[raw] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.rawConns, raw)
		s.mu.Unlock()
		_ = raw.Close()
	}()

	sc, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conns[sc] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sc)
		s.mu.Unlock()
		_ = sc.Close()
	}()

	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()

	for newChan := range chans {
		if newChan.ChannelType() != "direct-tcpip" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only direct-tcpip supported")
			continue
		}
		var payload struct {
			DestAddr string
			DestPort uint32
			OrigAddr string
			OrigPort uint32
		}
		if err := ssh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
			_ = newChan.Reject(ssh.ConnectionFailed, "bad payload")
			continue
		}

		requested := net.JoinHostPort(payload.DestAddr, strconv.Itoa(int(payload.DestPort)))
		target := requested
		if mapped, ok := s.routes[requested]; ok {
			target = mapped
		}

		dst, derr := net.DialTimeout("tcp", target, 5*time.Second)
		if derr != nil {
			_ = newChan.Reject(ssh.ConnectionFailed, derr.Error())
			continue
		}

		ch, chReqs, aerr := newChan.Accept()
		if aerr != nil {
			_ = dst.Close()
			continue
		}
		s.directTCPIPCount.Add(1)
		go ssh.DiscardRequests(chReqs)
		go func(c ssh.Channel, d net.Conn) {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(d, c)
				if tc, ok := d.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
			}()
			go func() {
				defer wg.Done()
				_, _ = io.Copy(c, d)
				_ = c.CloseWrite()
			}()
			wg.Wait()
			_ = c.Close()
			_ = d.Close()
		}(ch, dst)
	}
}

func (s *forwardingSSHServer) KillAllConnections() {
	s.mu.Lock()
	conns := make([]*ssh.ServerConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	raws := make([]net.Conn, 0, len(s.rawConns))
	for r := range s.rawConns {
		raws = append(raws, r)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	for _, r := range raws {
		_ = r.Close()
	}
}

type realSSHTLSHarness struct {
	repo      *repository.LocalDB
	secrets   *secret.Store
	tunnelMgr *tunnel.Manager
	sessMgr   *session.Manager
	resMgr    *resultbuf.Manager
	connSvc   *service.ConnectionService
	qrySvc    *service.QueryService
	sshSrv    *forwardingSSHServer

	evtMu  sync.Mutex
	events []dto.SessionStateEvent
	evtCh  chan dto.SessionStateEvent
}

func newRealSSHTLSHarness(t *testing.T) *realSSHTLSHarness {
	t.Helper()
	dir := t.TempDir()
	repo, err := repository.OpenLocalDB(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatalf("open local db: %v", err)
	}
	sec := secret.NewFileOnlyStore(dir)
	reg := driver.NewRegistry()
	reg.Register(mysql.NewOpener(dto.EngineMySQL))
	reg.Register(mysql.NewOpener(dto.EngineMariaDB))
	reg.Register(postgres.NewOpener())

	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: filepath.Join(dir, "results")})

	routes := make(map[string]string)
	for _, target := range realDBTargets() {
		routes[net.JoinHostPort(target.remoteAliasHost, strconv.Itoa(target.remoteAliasPort))] =
			net.JoinHostPort(target.host, strconv.Itoa(target.port))
	}
	sshSrv := startForwardingSSHServer(t, "ssh_test_pw", routes)

	h := &realSSHTLSHarness{
		repo:    repo,
		secrets: sec,
		resMgr:  resMgr,
		sshSrv:  sshSrv,
		evtCh:   make(chan dto.SessionStateEvent, 32),
	}

	h.tunnelMgr = tunnel.NewManager(repo, func(connID string, lostGen uint64) {
		if h.sessMgr != nil {
			h.sessMgr.MarkConnectionLost(connID, lostGen)
		}
	})
	h.connSvc = service.NewConnectionService(repo, sec, nil, resMgr)
	h.connSvc.SetTunnelManager(h.tunnelMgr)
	h.sessMgr = session.NewManager(reg, h.connSvc.ResolveConfig)
	h.sessMgr.SetOnStateChanged(func(evt dto.SessionStateEvent) {
		h.evtMu.Lock()
		h.events = append(h.events, evt)
		h.evtMu.Unlock()
		select {
		case h.evtCh <- evt:
		default:
		}
	})
	h.connSvc.SetSessionManager(h.sessMgr)
	h.qrySvc = service.NewQueryService(h.sessMgr, resMgr, repo)

	t.Cleanup(func() {
		h.sessMgr.CloseAll()
		h.tunnelMgr.CloseAll()
		h.resMgr.CloseAll()
		_ = h.repo.Close()
	})
	return h
}

func (h *realSSHTLSHarness) saveConn(
	t *testing.T,
	name string,
	engine dto.EngineType,
	host string,
	port int,
	user, pass, db string,
	tlsCfg dto.TLSConfig,
	useSSH bool,
) dto.ConnectionDTO {
	t.Helper()
	req := dto.SaveConnectionRequest{
		Name:           name,
		Driver:         engine,
		EnvColor:       "green",
		Password:       pass,
		UpdatePassword: true,
		Params: dto.ConnectionParams{
			Host:            host,
			Port:            port,
			Username:        user,
			DefaultDatabase: db,
			TLS:             tlsCfg,
		},
	}
	if useSSH {
		req.Params.SSH = &dto.SSHConfig{
			Enabled:        true,
			Host:           h.sshSrv.host,
			Port:           h.sshSrv.port,
			Username:       "bastion",
			AuthMethod:     "password",
			SaveCredential: true,
		}
		req.SSHPassword = "ssh_test_pw"
		req.UpdateSSHPassword = true
	}
	c, err := h.connSvc.SaveConnection(context.Background(), req)
	if err != nil {
		t.Fatalf("SaveConnection(%s): %v", name, err)
	}
	return c
}

func (h *realSSHTLSHarness) trustSSHHost(t *testing.T) {
	t.Helper()
	hp := tunnel.NormalizeHostPort(h.sshSrv.host, h.sshSrv.port)
	if err := h.connSvc.TrustSSHHostKey(context.Background(), hp, h.sshSrv.algorithm, h.sshSrv.fingerprint); err != nil {
		t.Fatalf("TrustSSHHostKey: %v", err)
	}
}

func (h *realSSHTLSHarness) runSingleQueryOverTab(t *testing.T, connID, db, sqlText string) *dto.ExecuteSQLResponse {
	t.Helper()
	ctx := context.Background()
	tabID := fmt.Sprintf("tab-%d", time.Now().UnixNano())
	if _, err := h.qrySvc.OpenTabSession(ctx, tabID, connID, db, ""); err != nil {
		t.Fatalf("OpenTabSession(%s): %v", connID, err)
	}
	defer h.qrySvc.CloseTab(tabID)

	resp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID:        tabID,
		ConnectionID: connID,
		ExecID:       "exec-1",
		SQL:          sqlText,
		RunMode:      "all",
	})
	if err != nil {
		t.Fatalf("ExecuteSQL(%s): %v", sqlText, err)
	}
	if len(resp.Statements) == 0 || resp.Statements[0].Error != nil {
		t.Fatalf("unexpected statement error: %+v", resp.Statements)
	}
	return resp
}

// TestRealDB_SSH_TLS_mTLS_VerifyModes exercises MySQL 8.4, MariaDB 11.4, and PostgreSQL 16
// over a real Go SSH Server + tunnel.Manager with verify-ca, verify-full, remote-only DNS alias,
// untrusted CA rejection, hostname mismatch rejection, and true mTLS client-cert enforcement.
func TestRealDB_SSH_TLS_mTLS_VerifyModes(t *testing.T) {
	certDir := requireRealDBsAndCerts(t)
	caPath := filepath.Join(certDir, "ca.crt")
	otherCAPath := filepath.Join(certDir, "other-ca.crt")
	clientCert := filepath.Join(certDir, "client.crt")
	clientKey := filepath.Join(certDir, "client.key")
	wrongClientCert := filepath.Join(certDir, "wrong-client.crt")
	wrongClientKey := filepath.Join(certDir, "wrong-client.key")

	for _, target := range realDBTargets() {
		target := target
		t.Run(target.name, func(t *testing.T) {
			ctx := context.Background()
			h := newRealSSHTLSHarness(t)

			// 1. TOFU challenge must trigger on first SSH connection before trusting host key.
			cVerifyFull := h.saveConn(t, target.name+"-verify-full", target.engine,
				target.host, target.port, target.rwUser, target.rwPass, target.database,
				dto.TLSConfig{Mode: "verify-full", CACertPath: caPath}, true)

			_, err := h.connSvc.Connect(ctx, cVerifyFull.ID)
			var appErr *dto.AppError
			if !errors.As(err, &appErr) || appErr.HostKeyChallenge == nil {
				t.Fatalf("expected SSH TOFU HostKeyChallenge on first connect, got: %v", err)
			}
			if appErr.HostKeyChallenge.Fingerprint != h.sshSrv.fingerprint {
				t.Fatalf("TOFU fingerprint mismatch: got %q want %q", appErr.HostKeyChallenge.Fingerprint, h.sshSrv.fingerprint)
			}

			// Trust host key and retry -> verify-full over SSH MUST succeed!
			h.trustSSHHost(t)
			beforeChannels := h.sshSrv.directTCPIPCount.Load()
			res, err := h.connSvc.Connect(ctx, cVerifyFull.ID)
			if err != nil {
				t.Fatalf("verify-full over SSH failed: %v", err)
			}
			if res.DetectedType != target.engine {
				t.Fatalf("detected engine = %s, want %s", res.DetectedType, target.engine)
			}
			resp := h.runSingleQueryOverTab(t, cVerifyFull.ID, target.database, target.verifySQL)
			if len(resp.Statements[0].Rows) != 1 || resp.Statements[0].Rows[0][0].Text == "0" {
				t.Fatalf("unexpected verify query result: %+v", resp.Statements[0].Rows)
			}
			if h.sshSrv.directTCPIPCount.Load() <= beforeChannels {
				t.Fatalf("expected real direct-tcpip SSH forwarding channels to be opened")
			}

			// 2. Remote-only hostname alias (unresolvable locally AND absent from server SAN):
			//    - verify-ca MUST SUCCEED (proves CA chain checked, hostname skipped, PG LookupFunc pass-through works!)
			//    - verify-full MUST FAIL (proves ServerName hostname validation enforced!)
			cRemoteVerifyCA := h.saveConn(t, target.name+"-remote-verify-ca", target.engine,
				target.remoteAliasHost, target.remoteAliasPort, target.rwUser, target.rwPass, target.database,
				dto.TLSConfig{Mode: "verify-ca", CACertPath: caPath}, true)
			if _, err := h.connSvc.Connect(ctx, cRemoteVerifyCA.ID); err != nil {
				t.Fatalf("verify-ca over SSH remote-only alias %s failed: %v", target.remoteAliasHost, err)
			}
			h.runSingleQueryOverTab(t, cRemoteVerifyCA.ID, target.database, target.verifySQL)

			cRemoteVerifyFull := h.saveConn(t, target.name+"-remote-verify-full-mismatch", target.engine,
				target.remoteAliasHost, target.remoteAliasPort, target.rwUser, target.rwPass, target.database,
				dto.TLSConfig{Mode: "verify-full", CACertPath: caPath}, true)
			if _, err := h.connSvc.Connect(ctx, cRemoteVerifyFull.ID); err == nil {
				t.Fatalf("expected verify-full to FAIL for SAN-mismatched host %s, but succeeded", target.remoteAliasHost)
			}

			// 3. Untrusted CA in verify-ca MUST FAIL (proves verify-ca no longer skips chain verification!).
			cUntrustedCA := h.saveConn(t, target.name+"-untrusted-ca", target.engine,
				target.host, target.port, target.rwUser, target.rwPass, target.database,
				dto.TLSConfig{Mode: "verify-ca", CACertPath: otherCAPath}, true)
			if _, err := h.connSvc.Connect(ctx, cUntrustedCA.ID); err == nil {
				t.Fatalf("expected verify-ca with untrusted CA to FAIL, but succeeded")
			}

			// 4. Real mTLS enforcement against sqlm_mtls user:
			//    - without client cert -> MUST FAIL
			//    - with wrong-CA client cert -> MUST FAIL
			//    - with valid CA-signed client cert + key -> MUST SUCCEED and query real rows!
			cMTLSNoCert := h.saveConn(t, target.name+"-mtls-no-cert", target.engine,
				target.host, target.port, target.mtlsUser, target.mtlsPass, target.database,
				dto.TLSConfig{Mode: "verify-full", CACertPath: caPath}, true)
			if _, err := h.connSvc.Connect(ctx, cMTLSNoCert.ID); err == nil {
				t.Fatalf("expected sqlm_mtls without client cert to FAIL, but succeeded")
			}

			cMTLSWrongCert := h.saveConn(t, target.name+"-mtls-wrong-cert", target.engine,
				target.host, target.port, target.mtlsUser, target.mtlsPass, target.database,
				dto.TLSConfig{Mode: "verify-full", CACertPath: caPath, CertPath: wrongClientCert, KeyPath: wrongClientKey}, true)
			if _, err := h.connSvc.Connect(ctx, cMTLSWrongCert.ID); err == nil {
				t.Fatalf("expected sqlm_mtls with untrusted client cert to FAIL, but succeeded")
			}

			cMTLSValid := h.saveConn(t, target.name+"-mtls-valid", target.engine,
				target.host, target.port, target.mtlsUser, target.mtlsPass, target.database,
				dto.TLSConfig{Mode: "verify-full", CACertPath: caPath, CertPath: clientCert, KeyPath: clientKey}, true)
			if _, err := h.connSvc.Connect(ctx, cMTLSValid.ID); err != nil {
				t.Fatalf("expected sqlm_mtls with valid mTLS cert over SSH to SUCCEED, got: %v", err)
			}
			mtlsResp := h.runSingleQueryOverTab(t, cMTLSValid.ID, target.database, target.verifySQL)
			if len(mtlsResp.Statements[0].Rows) != 1 || mtlsResp.Statements[0].Rows[0][0].Text == "0" {
				t.Fatalf("unexpected mTLS query result: %+v", mtlsResp.Statements[0].Rows)
			}
		})
	}
}

// startPGNoSSLProxy answers PostgreSQL SSLRequest with 'N' (no SSL support) and proxies plaintext
// traffic to the real PostgreSQL 16 container.
func startPGNoSSLProxy(t *testing.T, targetAddr string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen pg no-ssl proxy: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		sslRequest := [8]byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}
		for {
			cli, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var head [8]byte
				if _, rerr := io.ReadFull(c, head[:]); rerr != nil {
					return
				}
				var initial []byte
				if head == sslRequest {
					if _, werr := c.Write([]byte{'N'}); werr != nil {
						return
					}
				} else {
					initial = head[:]
				}
				dst, derr := net.DialTimeout("tcp", targetAddr, 3*time.Second)
				if derr != nil {
					return
				}
				defer dst.Close()
				if len(initial) > 0 {
					if _, werr := dst.Write(initial); werr != nil {
						return
					}
				}
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); _, _ = io.Copy(dst, c) }()
				go func() { defer wg.Done(); _, _ = io.Copy(c, dst) }()
				wg.Wait()
			}(cli)
		}
	}()

	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	return h, p
}

// startMySQLNoSSLProxy strips CLIENT_SSL (0x0800) from the real MySQL 8.4 / MariaDB 11.4 server's
// HandshakeV10 capability flags so we can verify prefer fallback vs strict TLS refusal against real servers.
func startMySQLNoSSLProxy(t *testing.T, targetAddr string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mysql no-ssl proxy: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			cli, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				dst, derr := net.DialTimeout("tcp", targetAddr, 3*time.Second)
				if derr != nil {
					return
				}
				defer dst.Close()

				// Read initial HandshakeV10 packet: 4-byte header + payload.
				var hdr [4]byte
				if _, err := io.ReadFull(dst, hdr[:]); err != nil {
					return
				}
				plen := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
				payload := make([]byte, plen)
				if _, err := io.ReadFull(dst, payload); err != nil {
					return
				}

				// Locate lower 2 bytes of capability flags:
				// protocol_version(1) + NUL-terminated server_version + conn_id(4) + auth_data_1(8) + filler(1)
				idx := 1
				for idx < len(payload) && payload[idx] != 0 {
					idx++
				}
				capOffset := idx + 1 + 4 + 8 + 1
				if capOffset+2 <= len(payload) {
					capLow := binary.LittleEndian.Uint16(payload[capOffset : capOffset+2])
					capLow &^= 0x0800 // strip CLIENT_SSL
					binary.LittleEndian.PutUint16(payload[capOffset:capOffset+2], capLow)
				}

				if _, err := c.Write(hdr[:]); err != nil {
					return
				}
				if _, err := c.Write(payload); err != nil {
					return
				}

				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); _, _ = io.Copy(dst, c) }()
				go func() { defer wg.Done(); _, _ = io.Copy(c, dst) }()
				wg.Wait()
			}(cli)
		}
	}()

	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	return h, p
}

// TestRealDB_StrictTLS_ForbidsPlaintextDowngrade verifies against real MySQL 8.4, MariaDB 11.4,
// and PostgreSQL 16 (behind a TLS-stripping proxy) that prefer falls back cleanly to plaintext
// while require / verify-ca / verify-full NEVER downgrade to plaintext.
func TestRealDB_StrictTLS_ForbidsPlaintextDowngrade(t *testing.T) {
	certDir := requireRealDBsAndCerts(t)
	caPath := filepath.Join(certDir, "ca.crt")

	// Inject polluting PG env vars to prove PG driver ignores them and never adds plaintext Fallbacks.
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGHOST", "127.0.0.1")

	for _, target := range realDBTargets() {
		target := target
		t.Run(target.name, func(t *testing.T) {
			ctx := context.Background()
			h := newRealSSHTLSHarness(t)
			realAddr := net.JoinHostPort(target.host, strconv.Itoa(target.port))

			var proxyHost string
			var proxyPort int
			if target.engine == dto.EnginePostgres {
				proxyHost, proxyPort = startPGNoSSLProxy(t, realAddr)
			} else {
				proxyHost, proxyPort = startMySQLNoSSLProxy(t, realAddr)
			}

			// 1. prefer MUST fall back to plaintext on the same target and succeed.
			cPrefer := h.saveConn(t, target.name+"-prefer-fallback", target.engine,
				proxyHost, proxyPort, target.rwUser, target.rwPass, target.database,
				dto.TLSConfig{Mode: "prefer"}, false)
			if _, err := h.connSvc.Connect(ctx, cPrefer.ID); err != nil {
				t.Fatalf("expected prefer to fall back cleanly to plaintext for %s, got: %v", target.name, err)
			}

			// 2. require, verify-ca, verify-full MUST all refuse plaintext downgrade.
			strictCases := []struct {
				mode string
				ca   string
			}{
				{mode: "require"},
				{mode: "verify-ca", ca: caPath},
				{mode: "verify-full", ca: caPath},
			}
			for _, sc := range strictCases {
				cStrict := h.saveConn(t, target.name+"-strict-"+sc.mode, target.engine,
					proxyHost, proxyPort, target.rwUser, target.rwPass, target.database,
					dto.TLSConfig{Mode: sc.mode, CACertPath: sc.ca}, false)
				if _, err := h.connSvc.Connect(ctx, cStrict.ID); err == nil {
					t.Fatalf("expected strict mode %q to REFUSE plaintext downgrade for %s, but succeeded", sc.mode, target.name)
				}
			}
		})
	}
}

// TestRealDB_SSH_TLS_SlowQueryCancelExecution verifies in-flight query cancellation over real
// SSH tunnel + TLS (including PostgreSQL PgConn.CancelRequest over tunnelConn with real RemoteAddr)
// across MySQL 8.4, MariaDB 11.4, and PostgreSQL 16.
func TestRealDB_SSH_TLS_SlowQueryCancelExecution(t *testing.T) {
	certDir := requireRealDBsAndCerts(t)
	caPath := filepath.Join(certDir, "ca.crt")

	for _, target := range realDBTargets() {
		target := target
		t.Run(target.name, func(t *testing.T) {
			ctx := context.Background()
			h := newRealSSHTLSHarness(t)
			h.trustSSHHost(t)

			c := h.saveConn(t, target.name+"-ssh-cancel", target.engine,
				target.host, target.port, target.rwUser, target.rwPass, target.database,
				dto.TLSConfig{Mode: "verify-full", CACertPath: caPath}, true)
			if _, err := h.connSvc.Connect(ctx, c.ID); err != nil {
				t.Fatalf("connect %s over SSH+TLS: %v", target.name, err)
			}

			tabID := "tab-cancel-" + target.name
			if _, err := h.qrySvc.OpenTabSession(ctx, tabID, c.ID, target.database, ""); err != nil {
				t.Fatalf("OpenTabSession: %v", err)
			}
			defer h.qrySvc.CloseTab(tabID)

			execID := "exec-slow-" + target.name
			type execOutcome struct {
				resp *dto.ExecuteSQLResponse
				err  error
				dur  time.Duration
			}
			done := make(chan execOutcome, 1)
			start := time.Now()
			go func() {
				resp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
					TabID:        tabID,
					ConnectionID: c.ID,
					ExecID:       execID,
					SQL:          target.sleepSQL,
					RunMode:      "all",
				})
				done <- execOutcome{resp: resp, err: err, dur: time.Since(start)}
			}()

			// Let query reach the server, then cancel over SSH.
			time.Sleep(350 * time.Millisecond)
			cancelResp := h.qrySvc.CancelExecution(ctx, tabID, execID)
			if cancelResp.Status != "canceled" && cancelResp.Status != "cancel_sent" {
				t.Fatalf("CancelExecution over SSH unexpected status for %s: %+v", target.name, cancelResp)
			}
			if cancelResp.SessionEpoch <= 0 {
				t.Fatalf("expected positive SessionEpoch from CancelExecution, got %+v", cancelResp)
			}

			select {
			case out := <-done:
				if out.dur >= 3500*time.Millisecond {
					t.Fatalf("slow query was NOT canceled promptly over SSH (%s took %v)", target.name, out.dur)
				}
				// Verify the error/statement reflects cancellation rather than running to completion.
				canceledSeen := out.err != nil
				if out.resp != nil && len(out.resp.Statements) > 0 && out.resp.Statements[0].Error != nil {
					canceledSeen = true
				}
				if !canceledSeen {
					t.Fatalf("expected cancellation error on slow query for %s, got resp=%+v err=%v", target.name, out.resp, out.err)
				}
			case <-time.After(4 * time.Second):
				t.Fatalf("timed out waiting for canceled slow query on %s", target.name)
			}

			// Follow-up query on the SAME tab must still succeed cleanly over the SSH tunnel.
			followResp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				TabID:        tabID,
				ConnectionID: c.ID,
				ExecID:       "exec-followup",
				SQL:          "SELECT 1",
				RunMode:      "all",
			})
			if err != nil {
				t.Fatalf("follow-up SELECT 1 after cancel failed on %s: %v", target.name, err)
			}
			if len(followResp.Statements) != 1 || followResp.Statements[0].Error != nil || followResp.Statements[0].Rows[0][0].Text != "1" {
				t.Fatalf("unexpected follow-up query response on %s: %+v", target.name, followResp.Statements)
			}
		})
	}
}

// TestRealDB_SSH_TunnelDrop_MarksSessionLostAndReconnects verifies end-to-end real SSH tunnel drop
// propagation to an idle DB tab (background SessionStateEvent + monotonic SessionEpoch) and clean reconnect.
func TestRealDB_SSH_TunnelDrop_MarksSessionLostAndReconnects(t *testing.T) {
	certDir := requireRealDBsAndCerts(t)
	caPath := filepath.Join(certDir, "ca.crt")

	for _, target := range []realDBTarget{realDBTargets()[0], realDBTargets()[2]} { // MySQL 8.4 + PostgreSQL 16
		target := target
		t.Run(target.name, func(t *testing.T) {
			ctx := context.Background()
			h := newRealSSHTLSHarness(t)
			h.trustSSHHost(t)

			c := h.saveConn(t, target.name+"-ssh-drop", target.engine,
				target.host, target.port, target.rwUser, target.rwPass, target.database,
				dto.TLSConfig{Mode: "verify-full", CACertPath: caPath}, true)
			if _, err := h.connSvc.Connect(ctx, c.ID); err != nil {
				t.Fatalf("connect: %v", err)
			}

			tabID := "tab-drop-" + target.name
			openInfo, err := h.qrySvc.OpenTabSession(ctx, tabID, c.ID, target.database, "")
			if err != nil {
				t.Fatalf("OpenTabSession: %v", err)
			}
			defer h.qrySvc.CloseTab(tabID)

			// Kill SSH server transport while DB tab is idle.
			h.sshSrv.KillAllConnections()

			var lostEvt dto.SessionStateEvent
			deadline := time.After(4 * time.Second)
			for {
				select {
				case evt := <-h.evtCh:
					if evt.TabID == tabID && evt.SessionState == dto.SessionLost {
						lostEvt = evt
						goto gotLost
					}
				case <-deadline:
					t.Fatalf("timed out waiting for SessionLost event on idle tab %s after SSH tunnel drop", tabID)
				}
			}
		gotLost:
			if lostEvt.SessionEpoch <= openInfo.SessionEpoch {
				t.Fatalf("expected SessionEpoch to advance on drop: open=%d lost=%d", openInfo.SessionEpoch, lostEvt.SessionEpoch)
			}

			// Reconnect establishes a new SSH tunnel & TLS DB session with strictly higher epoch.
			reInfo, err := h.qrySvc.ReconnectTabSession(ctx, tabID, target.database, "")
			if err != nil {
				t.Fatalf("ReconnectTabSession after SSH drop failed: %v", err)
			}
			if reInfo.SessionState != dto.SessionAliveIdle || reInfo.SessionEpoch <= lostEvt.SessionEpoch {
				t.Fatalf("unexpected reconnect info: %+v (lostEpoch=%d)", reInfo, lostEvt.SessionEpoch)
			}
			resp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				TabID:        tabID,
				ConnectionID: c.ID,
				ExecID:       "exec-after-reconnect",
				SQL:          target.verifySQL,
				RunMode:      "all",
			})
			if err != nil || len(resp.Statements) != 1 || resp.Statements[0].Error != nil {
				t.Fatalf("query after SSH reconnect failed: resp=%+v err=%v", resp, err)
			}
		})
	}
}
