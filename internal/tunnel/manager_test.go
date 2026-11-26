package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func generateEncryptedPrivateKeyFile(t *testing.T, passphrase string) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed25519: %v", err)
	}
	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test-key", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "test-key")
	}
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pubkey: %v", err)
	}
	return path, sshPub
}

func TestKnownHostsTOFUAtomicConcurrentAndMismatch(t *testing.T) {
	kv := newMemKVStore()
	store := NewKnownHostsStore(kv)

	// Concurrent writes for different hosts must never lose entries (per-host atomic KV keys).
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			hp := fmt.Sprintf("bastion-%d.internal:22", idx)
			fp := fmt.Sprintf("SHA256:fp-%d", idx)
			if err := store.TrustHostKey(context.Background(), hp, "ssh-ed25519", fp); err != nil {
				t.Errorf("TrustHostKey %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < 20; i++ {
		hp := fmt.Sprintf("bastion-%d.internal:22", i)
		entry, found, err := store.Lookup(context.Background(), hp)
		if err != nil || !found || entry.Fingerprint != fmt.Sprintf("SHA256:fp-%d", i) {
			t.Fatalf("lost known host entry for %s: found=%v entry=%+v err=%v", hp, found, entry, err)
		}
	}

	// End-to-end TOFU: unknown -> trust -> pass -> key rotation mismatch.
	srv1 := startTestSSHServer(t, testSSHServerOpts{password: "pw"})
	mgr := NewManager(kv, nil)
	defer mgr.CloseAll()

	_, _, err := mgr.AcquireDialer(context.Background(), "c-tofu", 1, SSHConfig{
		Host:       srv1.host,
		Port:       srv1.port,
		Username:   "u",
		AuthMethod: "password",
		Password:   "pw",
	})
	var challenge *HostKeyChallengeError
	if !errors.As(err, &challenge) {
		t.Fatalf("expected HostKeyChallengeError on first connect, got %v", err)
	}
	if challenge.Mismatch || challenge.Fingerprint != ssh.FingerprintSHA256(srv1.hostKey.PublicKey()) {
		t.Fatalf("unexpected challenge: %+v", challenge)
	}

	// Trust and retry -> succeeds!
	if err := mgr.TrustHostKey(context.Background(), challenge.HostPort, challenge.Algorithm, challenge.Fingerprint); err != nil {
		t.Fatalf("TrustHostKey: %v", err)
	}
	if _, _, err := mgr.AcquireDialer(context.Background(), "c-tofu", 1, SSHConfig{
		Host:       srv1.host,
		Port:       srv1.port,
		Username:   "u",
		AuthMethod: "password",
		Password:   "pw",
	}); err != nil {
		t.Fatalf("AcquireDialer after trust failed: %v", err)
	}
	mgr.CloseConnection("c-tofu")

	// Simulate MITM / host key rotation by trusting a different fingerprint for srv1.addr.
	if err := mgr.TrustHostKey(context.Background(), srv1.addr, "ssh-ed25519", "SHA256:old-fingerprint"); err != nil {
		t.Fatalf("overwrite known host: %v", err)
	}
	_, _, err = mgr.AcquireDialer(context.Background(), "c-tofu", 1, SSHConfig{
		Host:       srv1.host,
		Port:       srv1.port,
		Username:   "u",
		AuthMethod: "password",
		Password:   "pw",
	})
	challenge = nil
	if !errors.As(err, &challenge) || !challenge.Mismatch || challenge.PreviousFingerprint != "SHA256:old-fingerprint" {
		t.Fatalf("expected Mismatch=true HostKeyChallengeError, got %+v (err=%v)", challenge, err)
	}
}

func TestKeyAuthEncryptedAndAgentAuthLifecycleAndCancel(t *testing.T) {
	// 1. Encrypted private key auth
	keyPath, pubKey := generateEncryptedPrivateKeyFile(t, "top-secret-passphrase")
	srv := startTestSSHServer(t, testSSHServerOpts{authorizedPubKey: pubKey})
	kv := newMemKVStore()
	mgr := NewManager(kv, nil)
	defer mgr.CloseAll()
	_ = mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srv.hostKey.PublicKey()))

	// Missing passphrase -> AuthFailedError with NeedPassphrase=true
	_, _, err := mgr.AcquireDialer(context.Background(), "c-key", 1, SSHConfig{
		Host:           srv.host,
		Port:           srv.port,
		Username:       "u",
		AuthMethod:     "key",
		PrivateKeyPath: keyPath,
	})
	var authErr *AuthFailedError
	if !errors.As(err, &authErr) || !authErr.NeedPassphrase {
		t.Fatalf("expected NeedPassphrase AuthFailedError, got %v", err)
	}

	// Correct passphrase -> succeeds
	if _, _, err := mgr.AcquireDialer(context.Background(), "c-key", 1, SSHConfig{
		Host:           srv.host,
		Port:           srv.port,
		Username:       "u",
		AuthMethod:     "key",
		PrivateKeyPath: keyPath,
		Passphrase:     "top-secret-passphrase",
	}); err != nil {
		t.Fatalf("key auth with passphrase failed: %v", err)
	}

	// 2. Mock SSH Agent over real Unix/pipe socket: must stay open DURING handshake and close AFTER!
	_, agentPriv, _ := ed25519.GenerateKey(rand.Reader)
	agentSigner, _ := ssh.NewSignerFromKey(agentPriv)
	srvAgent := startTestSSHServer(t, testSSHServerOpts{authorizedPubKey: agentSigner.PublicKey()})
	_ = mgr.TrustHostKey(context.Background(), srvAgent.addr, srvAgent.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srvAgent.hostKey.PublicKey()))

	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: agentPriv}); err != nil {
		t.Fatalf("add agent key: %v", err)
	}

	var agentClosed atomic.Bool
	mgr.openAgent = func() (agent.ExtendedAgent, io.Closer, error) {
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			_ = agent.ServeAgent(keyring, c2)
		}()
		closer := &trackingCloser{
			Closer: c1,
			onClose: func() {
				agentClosed.Store(true)
			},
		}
		return agent.NewClient(c1), closer, nil
	}

	if _, _, err := mgr.AcquireDialer(context.Background(), "c-agent", 1, SSHConfig{
		Host:       srvAgent.host,
		Port:       srvAgent.port,
		Username:   "u",
		AuthMethod: "agent",
	}); err != nil {
		t.Fatalf("agent auth failed: %v", err)
	}
	if !agentClosed.Load() {
		t.Fatalf("agent closer must be closed after handshake completes")
	}

	// 3. Blocked Agent signing RPC MUST be unblocked by closing agentCloser on ctx cancel.
	agentClosedCh := make(chan struct{}, 2)
	mgr.openAgent = func() (agent.ExtendedAgent, io.Closer, error) {
		c1, c2 := net.Pipe()
		// Never serve c2 so agentClient.Signers() blocks until c1 is closed!
		t.Cleanup(func() { _ = c2.Close() })
		return agent.NewClient(c1), &trackingCloser{
			Closer: c1,
			onClose: func() {
				select {
				case agentClosedCh <- struct{}{}:
				default:
				}
			},
		}, nil
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, _, err = mgr.AcquireDialer(cancelCtx, "c-agent-cancel", 1, SSHConfig{
		Host:       srvAgent.host,
		Port:       srvAgent.port,
		Username:   "u",
		AuthMethod: "agent",
	})
	if err == nil {
		t.Fatalf("expected error on canceled blocked agent handshake")
	}
	select {
	case <-agentClosedCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("agent closer must be closed immediately when handshake ctx cancels")
	}
}

type trackingCloser struct {
	io.Closer
	onClose func()
}

func (tc *trackingCloser) Close() error {
	if tc.onClose != nil {
		tc.onClose()
	}
	return tc.Closer.Close()
}

func TestKeepaliveNegativeReplyAliveAndBlackholeTimeout(t *testing.T) {
	// 1. Server replies ok==false && err==nil to keepalive@openssh.com -> MUST stay alive!
	srvNeg := startTestSSHServer(t, testSSHServerOpts{
		password:        "pw",
		rejectKeepalive: true,
	})
	var negDisconnected atomic.Int32
	kv := newMemKVStore()
	mgr := NewManager(kv, func(connID string, _ uint64) {
		if connID == "c-neg" {
			negDisconnected.Add(1)
		}
	})
	mgr.KeepaliveInterval = 25 * time.Millisecond
	mgr.KeepaliveTimeout = 100 * time.Millisecond
	defer mgr.CloseAll()

	_ = mgr.TrustHostKey(context.Background(), srvNeg.addr, srvNeg.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srvNeg.hostKey.PublicKey()))

	_, gen1, err := mgr.AcquireDialer(context.Background(), "c-neg", 1, SSHConfig{
		Host:       srvNeg.host,
		Port:       srvNeg.port,
		Username:   "u",
		AuthMethod: "password",
		Password:   "pw",
	})
	if err != nil {
		t.Fatalf("AcquireDialer c-neg: %v", err)
	}
	time.Sleep(120 * time.Millisecond) // multiple keepalive ticks with ok==false replies
	if negDisconnected.Load() != 0 {
		t.Fatalf("negative keepalive reply (ok=false, err=nil) MUST NOT mark tunnel dead")
	}
	if _, existingGen, ok := mgr.TryAcquireExisting("c-neg", 1, EndpointDigest(SSHConfig{
		Host: srvNeg.host, Port: srvNeg.port, Username: "u", AuthMethod: "password",
	})); !ok || existingGen != gen1 {
		t.Fatalf("tunnel c-neg should remain alive and reusable")
	}

	// 2. Network blackhole (server never replies to keepalive) -> MUST disconnect within KeepaliveTimeout!
	srvBlack := startTestSSHServer(t, testSSHServerOpts{
		password:           "pw",
		blackholeKeepalive: true,
	})
	blackLost := make(chan uint64, 2)
	mgrBlack := NewManager(kv, func(connID string, lostGen uint64) {
		if connID == "c-black" {
			blackLost <- lostGen
		}
	})
	mgrBlack.KeepaliveInterval = 25 * time.Millisecond
	mgrBlack.KeepaliveTimeout = 60 * time.Millisecond
	defer mgrBlack.CloseAll()
	_ = mgrBlack.TrustHostKey(context.Background(), srvBlack.addr, srvBlack.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srvBlack.hostKey.PublicKey()))

	_, blackGen, err := mgrBlack.AcquireDialer(context.Background(), "c-black", 1, SSHConfig{
		Host:       srvBlack.host,
		Port:       srvBlack.port,
		Username:   "u",
		AuthMethod: "password",
		Password:   "pw",
	})
	if err != nil {
		t.Fatalf("AcquireDialer c-black: %v", err)
	}

	select {
	case gotGen := <-blackLost:
		if gotGen != blackGen {
			t.Fatalf("got lostGen=%d, want %d", gotGen, blackGen)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("blackhole keepalive did not trigger disconnect within timeout")
	}
}

func TestSharedDialFirstCancelsSecondSucceedsAndDifferentPasswordsIsolated(t *testing.T) {
	srv := startTestSSHServer(t, testSSHServerOpts{
		password:       "good-pw",
		handshakeDelay: 100 * time.Millisecond,
	})
	kv := newMemKVStore()
	mgr := NewManager(kv, nil)
	defer mgr.CloseAll()
	_ = mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srv.hostKey.PublicKey()))

	// 1. Concurrent AcquireDialer with same credentials: first cancels, second still succeeds!
	ctx1, cancel1 := context.WithCancel(context.Background())
	errCh1 := make(chan error, 1)
	genCh2 := make(chan uint64, 1)
	errCh2 := make(chan error, 1)

	cfgGood := SSHConfig{
		Host:       srv.host,
		Port:       srv.port,
		Username:   "u",
		AuthMethod: "password",
		Password:   "good-pw",
	}

	go func() {
		_, _, err := mgr.AcquireDialer(ctx1, "c-shared", 1, cfgGood)
		errCh1 <- err
	}()
	time.Sleep(15 * time.Millisecond)
	go func() {
		_, gen, err := mgr.AcquireDialer(context.Background(), "c-shared", 1, cfgGood)
		genCh2 <- gen
		errCh2 <- err
	}()
	time.Sleep(15 * time.Millisecond)
	cancel1()

	if err := <-errCh1; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller 1 expected context.Canceled, got %v", err)
	}
	if err := <-errCh2; err != nil {
		t.Fatalf("caller 2 MUST succeed after caller 1 cancels, got %v", err)
	}
	if gen := <-genCh2; gen == 0 {
		t.Fatalf("expected non-zero tunnel generation")
	}

	// 2. Concurrent different passwords MUST NOT poison each other (isolated by authDigest).
	cfgBad := cfgGood
	cfgBad.Password = "wrong-pw"
	var wg sync.WaitGroup
	var badErr, goodErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, badErr = mgr.AcquireDialer(context.Background(), "c-diff-pw", 1, cfgBad)
	}()
	go func() {
		defer wg.Done()
		_, _, goodErr = mgr.AcquireDialer(context.Background(), "c-diff-pw", 1, cfgGood)
	}()
	wg.Wait()

	var authErr *AuthFailedError
	if !errors.As(badErr, &authErr) {
		t.Fatalf("bad password expected AuthFailedError, got %v", badErr)
	}
	if goodErr != nil {
		t.Fatalf("good password MUST succeed concurrently with wrong password, got %v", goodErr)
	}
}

func TestTunnelGenerationsVoluntaryCloseAndStaleConfigVer(t *testing.T) {
	srv := startTestSSHServer(t, testSSHServerOpts{password: "pw"})
	kv := newMemKVStore()

	var disconnectedGens []uint64
	var mu sync.Mutex
	mgr := NewManager(kv, func(_ string, lostGen uint64) {
		mu.Lock()
		disconnectedGens = append(disconnectedGens, lostGen)
		mu.Unlock()
	})
	defer mgr.CloseAll()
	_ = mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srv.hostKey.PublicKey()))

	cfg := SSHConfig{
		Host:       srv.host,
		Port:       srv.port,
		Username:   "u",
		AuthMethod: "password",
		Password:   "pw",
	}

	// 1. Voluntary CloseConnection must NOT fire abnormal onDisconnect; old gen cannot kill new tunnel.
	_, gen1, err := mgr.AcquireDialer(context.Background(), "c-gen", 1, cfg)
	if err != nil {
		t.Fatalf("gen1 dial: %v", err)
	}
	mgr.CloseConnection("c-gen")
	_, gen2, err := mgr.AcquireDialer(context.Background(), "c-gen", 1, cfg)
	if err != nil {
		t.Fatalf("gen2 dial: %v", err)
	}
	if gen2 <= gen1 {
		t.Fatalf("expected gen2 > gen1 (%d <= %d)", gen2, gen1)
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	if len(disconnectedGens) != 0 {
		t.Fatalf("voluntary close must not fire abnormal onDisconnect, got %v", disconnectedGens)
	}
	mu.Unlock()
	if _, gotGen, ok := mgr.TryAcquireExisting("c-gen", 1, EndpointDigest(cfg)); !ok || gotGen != gen2 {
		t.Fatalf("new tunnel gen2 must remain active, ok=%v gotGen=%d", ok, gotGen)
	}

	// 2. Stale configVer after InvalidateConfig MUST be rejected and never insert into pool.
	mgr.InvalidateConfig("c-gen", 2)
	if _, _, err := mgr.AcquireDialer(context.Background(), "c-gen", 1, cfg); !errors.Is(err, ErrStaleConfig) {
		t.Fatalf("expected ErrStaleConfig for cfgVer=1 < minConfigVer=2, got %v", err)
	}
	if _, _, ok := mgr.TryAcquireExisting("c-gen", 1, EndpointDigest(cfg)); ok {
		t.Fatalf("stale cfgVer must not exist in pool")
	}

	// 3. In-flight dial aborted by CloseConnection before completion must not insert into pool.
	srvSlow := startTestSSHServer(t, testSSHServerOpts{password: "pw", handshakeDelay: 120 * time.Millisecond})
	_ = mgr.TrustHostKey(context.Background(), srvSlow.addr, srvSlow.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srvSlow.hostKey.PublicKey()))
	slowCfg := SSHConfig{Host: srvSlow.host, Port: srvSlow.port, Username: "u", AuthMethod: "password", Password: "pw"}
	errCh := make(chan error, 1)
	go func() {
		_, _, err := mgr.AcquireDialer(context.Background(), "c-abort", 1, slowCfg)
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond)
	mgr.CloseConnection("c-abort")
	if err := <-errCh; err == nil {
		t.Fatalf("expected in-flight dial to fail after CloseConnection")
	}
	if _, _, ok := mgr.TryAcquireExisting("c-abort", 1, EndpointDigest(slowCfg)); ok {
		t.Fatalf("aborted in-flight dial must never enter pool")
	}
}

// ctxCheckingKVStore enforces that GetKV always receives a live, deadline-bounded context
// (just like real SQLite / repository.LocalDB does when context.Canceled fails a query).
type ctxCheckingKVStore struct {
	inner     *memKVStore
	getCalls  atomic.Int32
	badCtxErr atomic.Value
}

func (c *ctxCheckingKVStore) GetKV(ctx context.Context, key string) (string, bool, error) {
	c.getCalls.Add(1)
	if err := ctx.Err(); err != nil {
		c.badCtxErr.Store(err)
		return "", false, err
	}
	if _, ok := ctx.Deadline(); !ok {
		err := errors.New("expected bounded deadline on HostKeyCallback lookup context")
		c.badCtxErr.Store(err)
		return "", false, err
	}
	return c.inner.GetKV(ctx, key)
}

func (c *ctxCheckingKVStore) SetKV(ctx context.Context, key, val string) error {
	return c.inner.SetKV(ctx, key, val)
}

func TestSSHRekeyUsesTunnelLifetimeContextAndKeepsConnectionAlive(t *testing.T) {
	echoHandler := func(ch ssh.Channel, _ string) {
		defer ch.Close()
		_, _ = io.Copy(ch, ch)
	}
	srv := startTestSSHServer(t, testSSHServerOpts{password: "pw", onDirectTCPIP: echoHandler})
	baseKV := newMemKVStore()
	kv := &ctxCheckingKVStore{inner: baseKV}

	var disconnectCalls atomic.Int32
	mgr := NewManager(kv, func(_ string, _ uint64) {
		disconnectCalls.Add(1)
	})
	// Low rekey threshold forces genuine x/crypto/ssh key re-exchange during channel traffic.
	mgr.rekeyThreshold = 4 * 1024
	defer mgr.CloseAll()

	if err := mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srv.hostKey.PublicKey())); err != nil {
		t.Fatalf("TrustHostKey: %v", err)
	}

	cfg := SSHConfig{
		Host:       srv.host,
		Port:       srv.port,
		Username:   "rekey-user",
		AuthMethod: "password",
		Password:   "pw",
	}
	dialFn, gen, err := mgr.AcquireDialer(context.Background(), "c-rekey", 1, cfg)
	if err != nil {
		t.Fatalf("AcquireDialer: %v", err)
	}
	if gen == 0 {
		t.Fatalf("expected non-zero tunnel generation")
	}

	// Stream 64KB over an echo channel after AcquireDialer's handshake ctx was already canceled.
	ch, err := dialFn(context.Background(), "tcp", "db.internal:5432")
	if err != nil {
		t.Fatalf("dial channel: %v", err)
	}
	defer ch.Close()

	chunk := make([]byte, 4096)
	for i := range chunk {
		chunk[i] = byte(i % 251)
	}
	got := make([]byte, len(chunk))
	for iter := 0; iter < 16; iter++ {
		if _, err := ch.Write(chunk); err != nil {
			t.Fatalf("write iter %d failed (SSH rekey likely tore down healthy tunnel): %v", iter, err)
		}
		if _, err := io.ReadFull(ch, got); err != nil {
			t.Fatalf("read iter %d failed (SSH rekey likely tore down healthy tunnel): %v", iter, err)
		}
	}

	if v := kv.badCtxErr.Load(); v != nil {
		t.Fatalf("HostKeyCallback received invalid context during handshake/rekey: %v", v)
	}
	if calls := kv.getCalls.Load(); calls < 2 {
		t.Fatalf("expected HostKeyCallback to run at least twice (initial handshake + SSH rekey), got %d", calls)
	}
	if disconnectCalls.Load() != 0 {
		t.Fatalf("healthy tunnel should not disconnect during rekey, got %d disconnects", disconnectCalls.Load())
	}

	// Also verify rekey continues enforcing host-key mismatch if stored fingerprint changed!
	if err := mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), "SHA256:rotated-other-fingerprint"); err != nil {
		t.Fatalf("overwrite known host: %v", err)
	}
	var rekeyMismatchErr error
	for iter := 0; iter < 16; iter++ {
		if _, err := ch.Write(chunk); err != nil {
			rekeyMismatchErr = err
			break
		}
		if _, err := io.ReadFull(ch, got); err != nil {
			rekeyMismatchErr = err
			break
		}
	}
	if rekeyMismatchErr == nil {
		t.Fatalf("expected SSH rekey with mismatched known-host fingerprint to fail and close tunnel")
	}
}

func TestStaleConfigConcurrentInvalidateAndWatcherHandoffSafety(t *testing.T) {
	srv := startTestSSHServer(t, testSSHServerOpts{password: "pw", handshakeDelay: 5 * time.Millisecond})
	kv := newMemKVStore()
	mgr := NewManager(kv, nil)
	defer mgr.CloseAll()
	_ = mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srv.hostKey.PublicKey()))

	cfg := SSHConfig{
		Host:       srv.host,
		Port:       srv.port,
		Username:   "race-user",
		AuthMethod: "password",
		Password:   "pw",
	}

	// 1. Hammer AcquireDialer and InvalidateConfig concurrently to verify zero unlocked map reads under -race.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				_, _, _ = mgr.AcquireDialer(context.Background(), "c-race", uint64(idx+1), cfg)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := uint64(2); v <= 20; v++ {
			mgr.InvalidateConfig("c-race", v)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()

	// 2. Repeated OpenTransient handoffs (where defer cancel() runs immediately after dialSSHClient returns)
	// MUST NEVER race with the cancel watcher to close the handed-off live tunnel.
	echoHandler := func(ch ssh.Channel, _ string) {
		defer ch.Close()
		_, _ = io.Copy(ch, ch)
	}
	srvFast := startTestSSHServer(t, testSSHServerOpts{password: "pw", onDirectTCPIP: echoHandler})
	_ = mgr.TrustHostKey(context.Background(), srvFast.addr, srvFast.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srvFast.hostKey.PublicKey()))
	fastCfg := SSHConfig{
		Host:       srvFast.host,
		Port:       srvFast.port,
		Username:   "handoff-user",
		AuthMethod: "password",
		Password:   "pw",
	}
	for i := 0; i < 20; i++ {
		dialFn, closeFn, err := mgr.OpenTransient(context.Background(), fastCfg)
		if err != nil {
			t.Fatalf("OpenTransient iter %d: %v", i, err)
		}
		c, err := dialFn(context.Background(), "tcp", "db.internal:3306")
		if err != nil {
			closeFn()
			t.Fatalf("dial channel after OpenTransient handoff iter %d failed (watcher likely closed live conn): %v", i, err)
		}
		if _, err := c.Write([]byte("ping")); err != nil {
			_ = c.Close()
			closeFn()
			t.Fatalf("write iter %d: %v", i, err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
			_ = c.Close()
			closeFn()
			t.Fatalf("echo iter %d failed: %q err=%v", i, string(buf), err)
		}
		_ = c.Close()
		closeFn()
	}
}
