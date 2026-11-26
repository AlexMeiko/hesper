package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
	"sql-manager/internal/entity"
	"sql-manager/internal/repository"
	"sql-manager/internal/secret"
	"sql-manager/internal/session"
	"sql-manager/internal/tunnel"
)

type memSecretStore struct {
	mu                sync.Mutex
	vals              map[string]string
	failSet           error
	failDelete        error
	partialSetOnce    error // mutates vals[ref] THEN returns &secret.PartialMutationError once
	partialDeleteOnce error // deletes vals[ref] THEN returns &secret.PartialMutationError once
}

func newMemSecretStore() *memSecretStore {
	return &memSecretStore{vals: make(map[string]string)}
}

func (m *memSecretStore) Get(ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vals[ref], nil
}

func (m *memSecretStore) Set(ref, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSet != nil {
		return m.failSet
	}
	m.vals[ref] = value
	if m.partialSetOnce != nil {
		err := m.partialSetOnce
		m.partialSetOnce = nil
		return &secret.PartialMutationError{Op: "set", Err: err}
	}
	return nil
}

func (m *memSecretStore) Delete(ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDelete != nil {
		return m.failDelete
	}
	delete(m.vals, ref)
	if m.partialDeleteOnce != nil {
		err := m.partialDeleteOnce
		m.partialDeleteOnce = nil
		return &secret.PartialMutationError{Op: "delete", Err: err}
	}
	return nil
}

type fakeTunnelDBDriver struct {
	mu        sync.Mutex
	lastGens  []uint64
	openCount atomic.Int32
}

func (d *fakeTunnelDBDriver) Type() dto.EngineType { return dto.EngineMySQL }
func (d *fakeTunnelDBDriver) Open(_ context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	d.openCount.Add(1)
	d.mu.Lock()
	d.lastGens = append(d.lastGens, cfg.TunnelGen)
	d.mu.Unlock()
	return &fakeTunnelDBConn{gen: cfg.TunnelGen}, nil
}

type fakeTunnelDBConn struct{ gen uint64 }

func (c *fakeTunnelDBConn) Ping(_ context.Context) error  { return nil }
func (c *fakeTunnelDBConn) Close() error                  { return nil }
func (c *fakeTunnelDBConn) Dialect() dto.EngineType       { return dto.EngineMySQL }
func (c *fakeTunnelDBConn) WriteSentWithoutResponse() bool { return false }
func (c *fakeTunnelDBConn) ResetWireTracker()             {}
func (c *fakeTunnelDBConn) ServerFact(_ context.Context) (driver.ServerFact, error) {
	return driver.ServerFact{DetectedType: dto.EngineMySQL, ServerVersion: "8.4.0", CurrentDB: "app"}, nil
}
func (c *fakeTunnelDBConn) Capabilities(_ string) dto.Capabilities { return dto.Capabilities{} }
func (c *fakeTunnelDBConn) ClassifyError(err error, _ bool) *dto.AppError {
	return dto.NewAppError(dto.ErrConnectionFailed, err.Error(), "")
}

type miniSSHServer struct {
	host        string
	port        int
	addr        string
	algorithm   string
	fingerprint string
	ln          net.Listener
}

func startMiniSSHServer(t *testing.T, validPassword string) *miniSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == validPassword {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("bad password")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ssh: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			raw, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc, chans, reqs, serr := ssh.NewServerConn(c, cfg)
				if serr != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "not needed")
				}
			}(raw)
		}
	}()

	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	return &miniSSHServer{
		host:        h,
		port:        p,
		addr:        ln.Addr().String(),
		algorithm:   signer.PublicKey().Type(),
		fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		ln:          ln,
	}
}

func newTestSSHStack(t *testing.T, withTunnel bool) (*ConnectionService, *QueryService, *memSecretStore, *tunnel.Manager, *fakeTunnelDBDriver) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	repo, err := repository.OpenLocalDB(dbPath)
	if err != nil {
		t.Fatalf("open local db: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	secrets := newMemSecretStore()
	drv := &fakeTunnelDBDriver{}
	reg := driver.NewRegistry()
	reg.Register(drv)

	connSvc := NewConnectionService(repo, secrets, nil, nil)
	sessMgr := session.NewManager(reg, connSvc.ResolveConfig)
	connSvc.SetSessionManager(sessMgr)
	var tunnelMgr *tunnel.Manager
	if withTunnel {
		tunnelMgr = tunnel.NewManager(repo, sessMgr.MarkConnectionLost)
		t.Cleanup(func() { tunnelMgr.CloseAll() })
		connSvc.SetTunnelManager(tunnelMgr)
	}
	querySvc := NewQueryService(sessMgr, nil, repo)
	querySvc.SetSSHSecretPreparer(connSvc.PrepareRuntimeSSHCredential)
	return connSvc, querySvc, secrets, tunnelMgr, drv
}

type connReqOpt func(*dto.SaveConnectionRequest)

func withDBPass(pw string) connReqOpt {
	return func(r *dto.SaveConnectionRequest) {
		r.Password = pw
		r.UpdatePassword = true
	}
}

func withSSHPass(pw string) connReqOpt {
	return func(r *dto.SaveConnectionRequest) {
		r.SSHPassword = pw
		r.UpdateSSHPassword = true
	}
}

func withSSHKey(keyPath, passphrase string) connReqOpt {
	return func(r *dto.SaveConnectionRequest) {
		r.Params.SSH.AuthMethod = "key"
		r.Params.SSH.PrivateKeyPath = keyPath
		r.SSHPassphrase = passphrase
		r.UpdateSSHPassphrase = true
	}
}

func plainMySQLReq(id, name string, opts ...connReqOpt) dto.SaveConnectionRequest {
	req := dto.SaveConnectionRequest{
		ID:     id,
		Name:   name,
		Driver: dto.EngineMySQL,
		Params: dto.ConnectionParams{
			Host: "10.0.0.10", Port: 3306, Username: "root",
			TLS: dto.TLSConfig{Mode: "disable"},
		},
	}
	for _, opt := range opts {
		opt(&req)
	}
	return req
}

func sshMySQLReq(id, name, sshHost string, sshPort int, saveCred bool, opts ...connReqOpt) dto.SaveConnectionRequest {
	req := plainMySQLReq(id, name)
	req.Params.SSH = &dto.SSHConfig{
		Enabled:        true,
		Host:           sshHost,
		Port:           sshPort,
		Username:       "ops",
		AuthMethod:     "password",
		SaveCredential: saveCred,
	}
	for _, opt := range opts {
		opt(&req)
	}
	return req
}

func sqliteReq(id, name, filePath string) dto.SaveConnectionRequest {
	return dto.SaveConnectionRequest{
		ID:     id,
		Name:   name,
		Driver: dto.EngineSQLite,
		Params: dto.ConnectionParams{FilePath: filePath},
	}
}

func assertSecret(t *testing.T, secrets *memSecretStore, ref, want, ctxMsg string) {
	t.Helper()
	if got, _ := secrets.Get(ref); got != want {
		t.Fatalf("%s: expected secret %s=%q, got %q", ctxMsg, ref, want, got)
	}
}

func TestSSHCredentialLifecycleDisconnectPreservesAndDeleteCleans(t *testing.T) {
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	ctx := context.Background()

	saved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "prod-ssh", "bastion.internal", 22, true, withDBPass("db-pw"), withSSHPass("ssh-pw-1")))
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	if !saved.HasPassword || !saved.HasSSHPassword {
		t.Fatalf("expected HasPassword and HasSSHPassword true, got %+v", saved)
	}

	// Disconnect MUST preserve saved DB & SSH credentials!
	connSvc.Disconnect(saved.ID)
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "ssh-pw-1", "Disconnect MUST NOT delete SSH password")
	assertSecret(t, secrets, dbPasswordRef(saved.ID), "db-pw", "Disconnect MUST NOT delete DB password")

	// DeleteConnection MUST delete ALL persisted DB & SSH credentials!
	if err := connSvc.DeleteConnection(ctx, saved.ID); err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "", "DeleteConnection MUST delete SSH password")
	assertSecret(t, secrets, dbPasswordRef(saved.ID), "", "DeleteConnection MUST delete DB password")
}

func TestSSHPreAuthSaveSemanticsPriorityReuseAndIndependentFeedback(t *testing.T) {
	sshSrv := startMiniSSHServer(t, "right-ssh-pw")
	connSvc, querySvc, secrets, _, drv := newTestSSHStack(t, true)
	ctx := context.Background()

	// 1. Save connection with SaveCredential=true, but empty stored password (e.g. keyring lost).
	saved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "bastion-db", sshSrv.host, sshSrv.port, true))
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	// 2. Connect with empty stored password -> returns SSHAuthPrompt (NOT infinite loop).
	_, err = connSvc.Connect(ctx, saved.ID)
	var ae *dto.AppError
	if !errors.As(err, &ae) || ae.Code != dto.ErrAuthFailed || ae.SSHAuthPrompt == nil {
		t.Fatalf("expected ErrAuthFailed + SSHAuthPrompt, got %v", err)
	}

	// 3. Submit temporary password BEFORE host key is trusted -> first hits HostKeyChallenge!
	_, err = connSvc.ConnectWithSSHSecret(ctx, saved.ID, "right-ssh-pw", false)
	ae = nil
	if !errors.As(err, &ae) || ae.Code != dto.ErrTunnelFailed || ae.HostKeyChallenge == nil {
		t.Fatalf("expected HostKeyChallenge after providing temp password, got %v", err)
	}
	if err := connSvc.TrustSSHHostKey(ctx, ae.HostKeyChallenge.HostPort, ae.HostKeyChallenge.Algorithm, ae.HostKeyChallenge.Fingerprint); err != nil {
		t.Fatalf("TrustSSHHostKey: %v", err)
	}

	// 4. Seed "old-pw". Wrong password with saveSecret=false MUST preserve "old-pw" untouched!
	_ = secrets.Set(sshPasswordRef(saved.ID), "old-pw")
	_, err = connSvc.ConnectWithSSHSecret(ctx, saved.ID, "wrong-temp-pw", false)
	ae = nil
	if !errors.As(err, &ae) || ae.Code != dto.ErrAuthFailed || ae.SSHAuthPrompt == nil || ae.CredentialSaved {
		t.Fatalf("expected auth failure with CredentialSaved=false, got err=%v ae=%+v", err, ae)
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "old-pw", "saveSecret=false MUST preserve old stored password")

	// 5. Wrong password with saveSecret=true -> MUST STILL overwrite stored password & report CredentialSaved=true!
	_, err = connSvc.ConnectWithSSHSecret(ctx, saved.ID, "wrong-saved-pw", true)
	ae = nil
	if !errors.As(err, &ae) || ae.Code != dto.ErrAuthFailed || ae.SSHAuthPrompt == nil || !ae.CredentialSaved {
		t.Fatalf("expected auth failure with CredentialSaved=true, got err=%v ae=%+v", err, ae)
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "wrong-saved-pw", "wrong password with saveSecret=true MUST overwrite")

	// 6. Correct password with saveSecret=false overrides wrong stored password (runtime priority > stored!) and connects!
	res, err := connSvc.ConnectWithSSHSecret(ctx, saved.ID, "right-ssh-pw", false)
	if err != nil {
		t.Fatalf("explicit runtime secret MUST override stored secret and succeed, got %v", err)
	}
	if res.CredentialSaved {
		t.Fatalf("saveSecret=false should not mark CredentialSaved")
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "wrong-saved-pw", "saveSecret=false MUST leave stored secret unchanged")
	drv.mu.Lock()
	genBeforeSave := drv.lastGens[len(drv.lastGens)-1]
	drv.mu.Unlock()
	if genBeforeSave == 0 {
		t.Fatalf("expected non-zero SSH tunnel generation")
	}

	// 7. Reusing LIVE tunnel (TryAcquireExisting hit) with saveSecret=true MUST STILL save submitted secret,
	//    keep configVer unchanged, and continue reusing the EXACT SAME live tunnel generation!
	tabInfo, err := querySvc.OpenTabSessionWithSSHSecret(ctx, "tab-reuse-save", saved.ID, "app", "", "right-ssh-pw", true)
	if err != nil {
		t.Fatalf("OpenTabSessionWithSSHSecret on live tunnel: %v", err)
	}
	if !tabInfo.CredentialSaved {
		t.Fatalf("saveSecret=true on live tunnel reuse MUST still report CredentialSaved=true")
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "right-ssh-pw", "saveSecret=true on live tunnel reuse MUST persist")
	drv.mu.Lock()
	genAfterSave := drv.lastGens[len(drv.lastGens)-1]
	drv.mu.Unlock()
	if genAfterSave != genBeforeSave {
		t.Fatalf("credential metadata save MUST NOT bump configVer or close live tunnel (gen changed %d -> %d)", genBeforeSave, genAfterSave)
	}

	// 8. Save failure + auth success -> connection stays alive with SaveWarning!
	connSvc.Disconnect(saved.ID)
	secrets.failSet = errors.New("keyring locked")
	resWarn, err := connSvc.ConnectWithSSHSecret(ctx, saved.ID, "right-ssh-pw", true)
	if err != nil {
		t.Fatalf("save failure must NOT block valid SSH authentication, got %v", err)
	}
	if resWarn.CredentialSaved || resWarn.SaveWarning == "" {
		t.Fatalf("expected CredentialSaved=false and non-empty SaveWarning, got %+v", resWarn)
	}

	// 9. Save failure + auth failure -> AppError carries BOTH SSHAuthPrompt AND SaveWarning!
	connSvc.Disconnect(saved.ID)
	_, err = connSvc.ConnectWithSSHSecret(ctx, saved.ID, "still-wrong", true)
	ae = nil
	if !errors.As(err, &ae) || ae.Code != dto.ErrAuthFailed || ae.SSHAuthPrompt == nil || ae.SaveWarning == "" {
		t.Fatalf("expected AppError carrying BOTH SSHAuthPrompt and SaveWarning, got %+v", ae)
	}
	secrets.failSet = nil
}

func TestSSHWriteBackRespectsConcurrentDeleteAndEditAndNilTunnelMgr(t *testing.T) {
	// 1. SSH enabled while tunnelMgr == nil MUST fail closed with ErrTunnelFailed.
	connSvcNil, _, _, _, _ := newTestSSHStack(t, false)
	ctx := context.Background()
	saved, err := connSvcNil.SaveConnection(ctx, sshMySQLReq("", "ssh-no-mgr", "bastion", 22, false))
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	_, err = connSvcNil.ResolveConfig(ctx, saved.ID, "")
	var ae *dto.AppError
	if !errors.As(err, &ae) || ae.Code != dto.ErrTunnelFailed {
		t.Fatalf("expected ErrTunnelFailed when SSH enabled with nil tunnelMgr, got %v", err)
	}

	// 2. saveRuntimeSSHCredential with stale expectedCfgVer or deleted connection MUST NOT overwrite or resurrect!
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	c2, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "edit-race", "bastion-v1", 22, false))
	if err != nil {
		t.Fatalf("SaveConnection c2: %v", err)
	}
	connSvc.cfgMu.RLock()
	oldVer := connSvc.configVerLocked(c2.ID)
	connSvc.cfgMu.RUnlock()

	if err := connSvc.DeleteConnection(ctx, c2.ID); err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	carrier := &runtimeSSHSecret{secret: "zombie-pw", saveSecret: true}
	connSvc.saveRuntimeSSHCredential(ctx, c2.ID, oldVer, "password", "zombie-pw", carrier)
	if carrier.credentialSaved.Load() {
		t.Fatalf("write-back after DeleteConnection MUST be skipped")
	}
	if ent, _ := connSvc.repo.GetConnection(ctx, c2.ID); ent != nil {
		t.Fatalf("write-back MUST NOT resurrect deleted connection")
	}
	assertSecret(t, secrets, sshPasswordRef(c2.ID), "", "write-back after delete MUST NOT store secret")

	// 3. asAppError preserves wrapped *dto.AppError and *tunnel.HostKeyChallengeError via errors.As.
	wrapped := fmt.Errorf("outer wrap: %w", &tunnel.HostKeyChallengeError{
		HostPort: "b:22", Algorithm: "ssh-ed25519", Fingerprint: "SHA256:abc",
	})
	mapped := asAppError(wrapped, dto.ErrInternal, "fallback")
	if mapped.Code != dto.ErrTunnelFailed || mapped.HostKeyChallenge == nil || mapped.HostKeyChallenge.Fingerprint != "SHA256:abc" {
		t.Fatalf("asAppError lost HostKeyChallenge through %%w wrapping: %+v", mapped)
	}
}

func TestSaveConnectionUncheckSaveCredentialPropagatesRealDeleteErrorAndAllowsNotFound(t *testing.T) {
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	ctx := context.Background()

	saved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "ssh-delete-check", "bastion.internal", 22, true, withSSHPass("persisted-pw")))
	if err != nil {
		t.Fatalf("SaveConnection initial: %v", err)
	}
	uncheckReq := sshMySQLReq(saved.ID, "ssh-delete-check", "bastion.internal", 22, false)

	// 1. Real deletion error when user unchecks "记住" MUST fail SaveConnection and keep DB consistent.
	secrets.failDelete = errors.New("keyring access denied")
	if _, err := connSvc.SaveConnection(ctx, uncheckReq); err == nil {
		t.Fatalf("expected SaveConnection to return error when deleting saved SSH password fails")
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "persisted-pw", "secret must remain untouched after failed delete")
	list, _ := connSvc.ListConnections(ctx)
	if len(list) != 1 || list[0].Params.SSH == nil || !list[0].Params.SSH.SaveCredential {
		t.Fatalf("expected SaveCredential to remain true in DB when delete failed, got %+v", list)
	}

	// 2. Not-found error (e.g. credential already absent) MUST be treated as success.
	secrets.failDelete = os.ErrNotExist
	updated, err := connSvc.SaveConnection(ctx, uncheckReq)
	if err != nil {
		t.Fatalf("expected not-found delete to succeed cleanly, got %v", err)
	}
	if updated.Params.SSH == nil || updated.Params.SSH.SaveCredential {
		t.Fatalf("expected SaveCredential=false after successful edit, got %+v", updated.Params.SSH)
	}
	secrets.failDelete = nil
}

func TestConnectAndOpenTabReuseIdleDBConnStillRunsPreAuthSaveWithoutBumpingConfigVer(t *testing.T) {
	srv := startMiniSSHServer(t, "right-ssh-pw")
	connSvc, querySvc, secrets, _, drv := newTestSSHStack(t, true)
	ctx := context.Background()

	saved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "idle-reuse-save", srv.host, srv.port, false))
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	if err := connSvc.TrustSSHHostKey(ctx, tunnel.NormalizeHostPort(srv.host, srv.port), srv.algorithm, srv.fingerprint); err != nil {
		t.Fatalf("TrustSSHHostKey: %v", err)
	}

	// 1. Warm up Track A metaPool with an idle DB connection using saveSecret=false.
	if _, err := connSvc.ConnectWithSSHSecret(ctx, saved.ID, "right-ssh-pw", false); err != nil {
		t.Fatalf("initial ConnectWithSSHSecret: %v", err)
	}
	opensAfterWarmup := drv.openCount.Load()
	connSvc.cfgMu.RLock()
	cfgVerBefore := connSvc.configVerLocked(saved.ID)
	connSvc.cfgMu.RUnlock()

	// 2. ConnectWithSSHSecret with saveSecret=true while metaPool has an idle DB conn:
	//    must reuse idle DB conn, persist secret & SaveCredential=true, keep configVer unchanged.
	res, err := connSvc.ConnectWithSSHSecret(ctx, saved.ID, "saved-on-idle-reuse", true)
	if err != nil {
		t.Fatalf("ConnectWithSSHSecret on idle pool: %v", err)
	}
	if drv.openCount.Load() != opensAfterWarmup {
		t.Fatalf("expected idle DB connection reuse (opens %d -> %d)", opensAfterWarmup, drv.openCount.Load())
	}
	if !res.CredentialSaved || res.SaveWarning != "" {
		t.Fatalf("expected CredentialSaved=true and empty SaveWarning on idle reuse, got %+v", res)
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "saved-on-idle-reuse", "idle DB reuse must persist password")
	connSvc.cfgMu.RLock()
	cfgVerAfter := connSvc.configVerLocked(saved.ID)
	connSvc.cfgMu.RUnlock()
	if cfgVerAfter != cfgVerBefore {
		t.Fatalf("pre-auth save must NOT bump configVer (%d -> %d)", cfgVerBefore, cfgVerAfter)
	}

	// 3. Save failure while reusing idle DB connection must STILL succeed and surface SaveWarning!
	secrets.failSet = errors.New("vault read-only")
	resWarn, err := connSvc.ConnectWithSSHSecret(ctx, saved.ID, "another-pw", true)
	if err != nil {
		t.Fatalf("save failure on idle reuse must not break Connect: %v", err)
	}
	if resWarn.CredentialSaved || resWarn.SaveWarning == "" {
		t.Fatalf("expected CredentialSaved=false and non-empty SaveWarning, got %+v", resWarn)
	}
	secrets.failSet = nil

	// 4. OpenTabSessionWithSSHSecret reusing an already-healthy TabSession also runs pre-auth save!
	if _, err := querySvc.OpenTabSession(ctx, "tab-idle-save", saved.ID, "app", ""); err != nil {
		t.Fatalf("OpenTabSession initial: %v", err)
	}
	opensBeforeTabReuse := drv.openCount.Load()
	tabInfo, err := querySvc.OpenTabSessionWithSSHSecret(ctx, "tab-idle-save", saved.ID, "app", "", "tab-reused-pw", true)
	if err != nil {
		t.Fatalf("OpenTabSessionWithSSHSecret on live tab: %v", err)
	}
	if drv.openCount.Load() != opensBeforeTabReuse {
		t.Fatalf("expected live TabSession to be reused without reopening DB connection")
	}
	if !tabInfo.CredentialSaved {
		t.Fatalf("expected CredentialSaved=true when reusing live TabSession with saveSecret=true")
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "tab-reused-pw", "live TabSession reuse must persist password")
}

func TestDeleteConnectionPreservesRecordOnRealSecretDeleteFailure(t *testing.T) {
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	ctx := context.Background()

	saved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "delete-conn-check", "bastion.internal", 22, true, withDBPass("db-secret"), withSSHPass("ssh-secret")))
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	// 1. Real deletion error MUST fail DeleteConnection and preserve DB record for retry!
	secrets.failDelete = errors.New("keyring locked during delete")
	if err := connSvc.DeleteConnection(ctx, saved.ID); err == nil {
		t.Fatalf("expected DeleteConnection to return error when credential deletion fails")
	}
	list, err := connSvc.ListConnections(ctx)
	if err != nil || len(list) != 1 || list[0].ID != saved.ID {
		t.Fatalf("connection profile MUST be preserved for retry on credential delete failure, got %+v err=%v", list, err)
	}

	// 2. Not-found error MUST be treated as already-deleted success and remove the connection profile.
	secrets.failDelete = os.ErrNotExist
	if err := connSvc.DeleteConnection(ctx, saved.ID); err != nil {
		t.Fatalf("DeleteConnection with not-found credential should succeed, got %v", err)
	}
	list, err = connSvc.ListConnections(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("expected connection record to be deleted after successful DeleteConnection, got %+v err=%v", list, err)
	}
	secrets.failDelete = nil
}

func TestSQLiteAndNonSSHConnectionsRenameAndDeleteWithoutKeyring(t *testing.T) {
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	ctx := context.Background()

	sq, err := connSvc.SaveConnection(ctx, sqliteReq("", "local-sqlite", filepath.Join(t.TempDir(), "sample.db")))
	if err != nil {
		t.Fatalf("SaveConnection sqlite: %v", err)
	}
	plainMySQL, err := connSvc.SaveConnection(ctx, plainMySQLReq("", "plain-mysql"))
	if err != nil {
		t.Fatalf("SaveConnection plain mysql: %v", err)
	}
	sshMySQL, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "ssh-mysql", "bastion.internal", 22, true, withSSHPass("ssh-pw")))
	if err != nil {
		t.Fatalf("SaveConnection ssh mysql: %v", err)
	}

	// Simulate unavailable OS keyring on all Delete calls.
	secrets.failDelete = errors.New("no system keyring service")

	// 1. SQLite rename & delete MUST succeed without touching SSH keyring refs!
	renamedSq, err := connSvc.SaveConnection(ctx, sqliteReq(sq.ID, "local-sqlite-renamed", sq.Params.FilePath))
	if err != nil {
		t.Fatalf("renaming SQLite connection without keyring should succeed, got %v", err)
	}
	if renamedSq.Name != "local-sqlite-renamed" {
		t.Fatalf("expected renamed SQLite name, got %q", renamedSq.Name)
	}
	if err := connSvc.DeleteConnection(ctx, sq.ID); err != nil {
		t.Fatalf("deleting SQLite connection without keyring should succeed, got %v", err)
	}

	// 2. Non-SSH passwordless MySQL rename & delete MUST also succeed without touching SSH keyring refs!
	if _, err := connSvc.SaveConnection(ctx, plainMySQLReq(plainMySQL.ID, "plain-mysql-renamed")); err != nil {
		t.Fatalf("renaming non-SSH MySQL connection without keyring should succeed, got %v", err)
	}
	if err := connSvc.DeleteConnection(ctx, plainMySQL.ID); err != nil {
		t.Fatalf("deleting non-SSH passwordless MySQL connection without keyring should succeed, got %v", err)
	}

	// 3. SSH connection MUST STILL fail on DeleteConnection and SSH->SQLite conversion, preserving record!
	if err := connSvc.DeleteConnection(ctx, sshMySQL.ID); err == nil {
		t.Fatalf("deleting SSH connection with failing keyring MUST fail and keep record for retry")
	}
	if _, err := connSvc.SaveConnection(ctx, sqliteReq(sshMySQL.ID, "converted-to-sqlite", filepath.Join(t.TempDir(), "x.db"))); err == nil {
		t.Fatalf("converting SSH connection to SQLite when SSH secret delete fails MUST return error")
	}
	list, err := connSvc.ListConnections(ctx)
	if err != nil || len(list) != 1 || list[0].ID != sshMySQL.ID || list[0].Driver != dto.EngineMySQL {
		t.Fatalf("expected original SSH MySQL connection preserved after failed cleanup, got %+v err=%v", list, err)
	}
	secrets.failDelete = nil
}

func TestSaveConnectionFailureRollsBackAddedCredentialsAndTracksFailedRollbackForRetry(t *testing.T) {
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	ctx := context.Background()

	plainMySQL, err := connSvc.SaveConnection(ctx, plainMySQLReq("", "non-ssh-mysql"))
	if err != nil {
		t.Fatalf("SaveConnection non-ssh mysql: %v", err)
	}
	sqliteConn, err := connSvc.SaveConnection(ctx, sqliteReq("", "unrelated-sqlite", filepath.Join(t.TempDir(), "u.db")))
	if err != nil {
		t.Fatalf("SaveConnection sqlite: %v", err)
	}
	enableSSHReq := sshMySQLReq(plainMySQL.ID, "non-ssh-mysql", "bastion.internal", 22, true, withSSHPass("newly-added-ssh-pw"))

	// 1. Config persistence fails, rollback succeeds -> newly added SSH password MUST be deleted immediately!
	connSvc.upsertHook = func(_ context.Context, _ entity.ConnectionEntity) error {
		return errors.New("sqlite disk full")
	}
	if _, err := connSvc.SaveConnection(ctx, enableSSHReq); err == nil {
		t.Fatalf("expected SaveConnection to fail when upsert fails")
	}
	assertSecret(t, secrets, sshPasswordRef(plainMySQL.ID), "", "newly added SSH password must roll back on save failure")

	// 2. Config persistence fails AND rollback Delete ALSO fails ->
	//    must explicitly surface rollback failure AND preserve retry basis across restarts!
	secrets.failDelete = errors.New("keyring locked on rollback")
	_, err = connSvc.SaveConnection(ctx, enableSSHReq)
	var ae *dto.AppError
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "恢复保存前凭据状态失败") {
		t.Fatalf("expected explicit rollback failure feedback in AppError, got %v", err)
	}
	assertSecret(t, secrets, sshPasswordRef(plainMySQL.ID), "newly-added-ssh-pw", "orphaned secret stays tracked until delete succeeds")
	connSvc.upsertHook = nil

	// Unrelated SQLite connection MUST STILL rename cleanly even while keyring Delete fails!
	if _, err := connSvc.SaveConnection(ctx, sqliteReq(sqliteConn.ID, "unrelated-sqlite-renamed", sqliteConn.Params.FilePath)); err != nil {
		t.Fatalf("unrelated SQLite rename should still bypass keyring, got %v", err)
	}

	// Simulate app restart over the SAME repo & secret store: DeleteConnection MUST see persisted
	// pending cleanup ref, fail while keyring is locked, and clean it + app_kv marker once unlocked!
	restartedSvc := NewConnectionService(connSvc.repo, secrets, nil, nil)
	if err := restartedSvc.DeleteConnection(ctx, plainMySQL.ID); err == nil {
		t.Fatalf("DeleteConnection after failed rollback MUST fail while keyring is locked")
	}
	assertSecret(t, secrets, sshPasswordRef(plainMySQL.ID), "newly-added-ssh-pw", "orphaned secret remains until deletion succeeds")

	secrets.failDelete = nil
	if err := restartedSvc.DeleteConnection(ctx, plainMySQL.ID); err != nil {
		t.Fatalf("DeleteConnection retry after keyring recovery failed: %v", err)
	}
	assertSecret(t, secrets, sshPasswordRef(plainMySQL.ID), "", "orphaned SSH secret must be deleted on retry")
	if _, found, _ := connSvc.repo.GetKV(ctx, pendingCleanupKVKey(plainMySQL.ID)); found {
		t.Fatalf("expected pending cleanup KV marker to be removed after successful deletion")
	}
}

func TestSaveConnectionFailureRestoresPreSaveCredentialState(t *testing.T) {
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	ctx := context.Background()

	// Case 1: Existing SSH password connection with SaveCredential=false (NO stored password).
	// Checking "记住" for the first time + entering a password, then failing UpsertConnection
	// MUST roll back (delete) the newly written password!
	sshUnsaved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "ssh-unsaved", "bastion.internal", 22, false))
	if err != nil {
		t.Fatalf("SaveConnection sshUnsaved: %v", err)
	}
	connSvc.upsertHook = func(_ context.Context, _ entity.ConnectionEntity) error {
		return errors.New("upsert failed")
	}
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(sshUnsaved.ID, "ssh-unsaved", "bastion.internal", 22, true, withSSHPass("first-time-pw"))); err == nil {
		t.Fatalf("expected SaveConnection to fail")
	}
	assertSecret(t, secrets, sshPasswordRef(sshUnsaved.ID), "", "first-time saved SSH password must delete on save failure")
	connSvc.upsertHook = nil

	// Case 2: Existing connection with saved DB password + saved SSH password.
	sshSaved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "ssh-saved", "bastion.internal", 22, true, withDBPass("orig-db-pw"), withSSHPass("orig-ssh-pw")))
	if err != nil {
		t.Fatalf("SaveConnection sshSaved: %v", err)
	}

	// 2a. Switching saved SSH password -> key (deletes orig-ssh-pw, writes new-pp), then Upsert fails:
	//     MUST delete new-pp AND restore orig-ssh-pw AND restore orig-db-pw!
	connSvc.upsertHook = func(_ context.Context, _ entity.ConnectionEntity) error {
		return errors.New("upsert failed on switch")
	}
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(sshSaved.ID, "ssh-saved", "bastion.internal", 22, true, withDBPass("temp-db-pw"), withSSHKey("/tmp/id_ed25519", "temp-key-pp"))); err == nil {
		t.Fatalf("expected SaveConnection switch to fail")
	}
	assertSecret(t, secrets, sshPasswordRef(sshSaved.ID), "orig-ssh-pw", "restore orig-ssh-pw on failed password->key switch")
	assertSecret(t, secrets, sshPassphraseRef(sshSaved.ID), "", "delete newly added temp-key-pp on failed switch")
	assertSecret(t, secrets, dbPasswordRef(sshSaved.ID), "orig-db-pw", "restore orig-db-pw on failed save")

	// 2b. Unchecking "记住" (deletes orig-ssh-pw), then Upsert fails -> MUST restore orig-ssh-pw!
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(sshSaved.ID, "ssh-saved", "bastion.internal", 22, false)); err == nil {
		t.Fatalf("expected SaveConnection uncheck to fail")
	}
	assertSecret(t, secrets, sshPasswordRef(sshSaved.ID), "orig-ssh-pw", "restore orig-ssh-pw on failed uncheck")

	// 2c. Overwriting orig-ssh-pw -> new-ssh-pw, then Upsert fails -> MUST restore orig-ssh-pw!
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(sshSaved.ID, "ssh-saved", "bastion.internal", 22, true, withSSHPass("overwritten-ssh-pw"))); err == nil {
		t.Fatalf("expected SaveConnection overwrite to fail")
	}
	assertSecret(t, secrets, sshPasswordRef(sshSaved.ID), "orig-ssh-pw", "restore orig-ssh-pw on failed overwrite")
	connSvc.upsertHook = nil
}

func TestSaveConnectionPartialKeyringMutationRollsBackDBAndSSHCredentials(t *testing.T) {
	connSvc, _, secrets, _, _ := newTestSSHStack(t, true)
	ctx := context.Background()

	saved, err := connSvc.SaveConnection(ctx, sshMySQLReq("", "partial-mutation-conn", "bastion.internal", 22, true, withDBPass("orig-db-pw"), withSSHPass("orig-ssh-pw")))
	if err != nil {
		t.Fatalf("SaveConnection initial: %v", err)
	}

	// 1. DB password Delete partially mutates keyring then fails -> rollback MUST restore orig-db-pw!
	secrets.partialDeleteOnce = errors.New("vault read error after keyring delete")
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(saved.ID, "partial-mutation-conn", "bastion.internal", 22, true, withDBPass(""))); err == nil {
		t.Fatalf("expected SaveConnection to fail on DB password partial delete")
	}
	assertSecret(t, secrets, dbPasswordRef(saved.ID), "orig-db-pw", "restore orig-db-pw after partial delete")

	// 2. DB password Set partially mutates keyring then fails -> rollback MUST restore orig-db-pw!
	secrets.partialSetOnce = errors.New("vault read error after keyring set")
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(saved.ID, "partial-mutation-conn", "bastion.internal", 22, true, withDBPass("new-db-pw"))); err == nil {
		t.Fatalf("expected SaveConnection to fail on DB password partial set")
	}
	assertSecret(t, secrets, dbPasswordRef(saved.ID), "orig-db-pw", "restore orig-db-pw after partial set")

	// 3. SSH password Delete partially mutates keyring (uncheck SaveCredential) -> restore orig-ssh-pw!
	secrets.partialDeleteOnce = errors.New("vault read error after ssh keyring delete")
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(saved.ID, "partial-mutation-conn", "bastion.internal", 22, false)); err == nil {
		t.Fatalf("expected SaveConnection to fail on SSH password partial delete")
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "orig-ssh-pw", "restore orig-ssh-pw after SSH partial delete")

	// 4. SSH password Set partially mutates keyring (overwrite) -> restore orig-ssh-pw!
	secrets.partialSetOnce = errors.New("vault read error after ssh keyring set")
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(saved.ID, "partial-mutation-conn", "bastion.internal", 22, true, withSSHPass("new-ssh-pw"))); err == nil {
		t.Fatalf("expected SaveConnection to fail on SSH password partial set")
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "orig-ssh-pw", "restore orig-ssh-pw after SSH partial set")

	// 5. Newly created connection where DB password Set partially mutates keyring -> delete newly written password!
	secrets.partialSetOnce = errors.New("vault read error after new keyring set")
	if _, err := connSvc.SaveConnection(ctx, plainMySQLReq("conn-partial-new", "brand-new", withDBPass("brand-new-db-pw"))); err == nil {
		t.Fatalf("expected new SaveConnection to fail on partial set")
	}
	assertSecret(t, secrets, dbPasswordRef("conn-partial-new"), "", "delete newly added DB password after partial set rollback")

	// 6. Combined edit: DB password Set succeeds, then SSH password Delete partially mutates keyring ->
	//    rollback MUST restore BOTH orig-ssh-pw AND orig-db-pw!
	secrets.partialDeleteOnce = errors.New("vault read error after ssh keyring delete in combined edit")
	if _, err := connSvc.SaveConnection(ctx, sshMySQLReq(saved.ID, "partial-mutation-conn", "bastion.internal", 22, false, withDBPass("combined-new-db-pw"))); err == nil {
		t.Fatalf("expected combined SaveConnection to fail on SSH partial delete")
	}
	assertSecret(t, secrets, sshPasswordRef(saved.ID), "orig-ssh-pw", "restore orig-ssh-pw in combined edit rollback")
	assertSecret(t, secrets, dbPasswordRef(saved.ID), "orig-db-pw", "restore orig-db-pw in combined edit rollback")
}
