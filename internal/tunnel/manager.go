package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	defaultKeepaliveInterval = 30 * time.Second
	defaultKeepaliveTimeout  = 10 * time.Second
	defaultDialTimeout       = 15 * time.Second
)

// DialFunc matches driver.ResolvedConfig.DialContext.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ErrStaleConfig indicates an AcquireDialer call carried a configVer older than the latest invalidation.
var ErrStaleConfig = errors.New("ssh tunnel config version is stale")

// ErrTunnelClosed indicates the tunnel or manager was closed while dialing.
var ErrTunnelClosed = errors.New("ssh tunnel was closed before handshake completed")

type dialKey struct {
	connID         string
	cfgVer         uint64
	endpointDigest string
	authDigest     string
}

type dialCall struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int

	dialFn DialFunc
	gen    uint64
	err    error
}

type pooledTunnel struct {
	mgr            *Manager
	connID         string
	gen            uint64
	cfgVer         uint64
	endpointDigest string

	rawConn      net.Conn
	client       *ssh.Client
	tunnelCancel context.CancelFunc

	stopCh         chan struct{}
	stopOnce       sync.Once
	voluntary      atomic.Bool
	dead           atomic.Bool
	disconnectOnce sync.Once
}

func (pt *pooledTunnel) stopMonitor() {
	pt.stopOnce.Do(func() {
		close(pt.stopCh)
		if pt.tunnelCancel != nil {
			pt.tunnelCancel()
		}
	})
}

// fatalStall is invoked by tunnelConn when single-channel close exceeds ChannelCloseGrace.
// CRITICAL: It synchronously closes ONLY rawConn (non-blocking OS socket close) and schedules
// pool eviction + onDisconnect asynchronously outside any channel/write lock to avoid re-entrant
// deadlocks with driver.Conn.Close() (e.g. database/sql Conn.Close waiting on active Exec).
func (pt *pooledTunnel) fatalStall() {
	pt.dead.Store(true)
	_ = pt.rawConn.Close()
	go pt.mgr.handleTunnelFailure(pt)
}

func (pt *pooledTunnel) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if pt.dead.Load() {
		return nil, net.ErrClosed
	}
	raw, err := pt.client.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return newTunnelConn(raw, addr, pt.mgr.channelCloseGrace(), pt.fatalStall), nil
}

// Manager manages a zero-local-port pool of SSH tunnels keyed by connection ID,
// protected by monotonic tunnel generation, config versions, independent shared handshakes,
// and keepalive response timeouts.
type Manager struct {
	knownHosts   *KnownHostsStore
	onDisconnect func(connID string, lostGen uint64)

	// Configurable durations (override in tests for fast deterministic verification).
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	ChannelCloseGrace time.Duration
	DialTimeout       time.Duration

	// Test hook to substitute SSH agent discovery without mutating process env.
	openAgent func() (agent.ExtendedAgent, io.Closer, error)
	// Test hook to wrap or replace the underlying TCP dialer (e.g. blackhole / fault injection).
	dialTCP func(ctx context.Context, network, addr string) (net.Conn, error)
	// Test hook to lower ssh.Config.RekeyThreshold so key re-exchange can be tested deterministically.
	rekeyThreshold uint64

	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu           sync.Mutex
	closed       bool
	tunnels      map[string]*pooledTunnel
	epochs       map[string]uint64
	minConfigVer map[string]uint64
	inflight     map[dialKey]*dialCall
	nextGen      atomic.Uint64
}

// NewManager creates an SSH tunnel pool backed by kv for TOFU Known Hosts persistence.
func NewManager(kv KVStore, onDisconnect func(connID string, lostGen uint64)) *Manager {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &Manager{
		knownHosts:        NewKnownHostsStore(kv),
		onDisconnect:      onDisconnect,
		KeepaliveInterval: defaultKeepaliveInterval,
		KeepaliveTimeout:  defaultKeepaliveTimeout,
		ChannelCloseGrace: defaultChannelCloseGrace,
		DialTimeout:       defaultDialTimeout,
		openAgent:         openSSHAgent,
		baseCtx:           baseCtx,
		baseCancel:        baseCancel,
		tunnels:           make(map[string]*pooledTunnel),
		epochs:            make(map[string]uint64),
		minConfigVer:      make(map[string]uint64),
		inflight:          make(map[dialKey]*dialCall),
	}
}

func (m *Manager) keepaliveInterval() time.Duration {
	if m.KeepaliveInterval > 0 {
		return m.KeepaliveInterval
	}
	return defaultKeepaliveInterval
}

func (m *Manager) keepaliveTimeout() time.Duration {
	if m.KeepaliveTimeout > 0 {
		return m.KeepaliveTimeout
	}
	return defaultKeepaliveTimeout
}

func (m *Manager) channelCloseGrace() time.Duration {
	if m.ChannelCloseGrace > 0 {
		return m.ChannelCloseGrace
	}
	return defaultChannelCloseGrace
}

func (m *Manager) dialTimeout(cfg SSHConfig) time.Duration {
	if cfg.ConnectTimeout > 0 {
		return cfg.ConnectTimeout
	}
	if m.DialTimeout > 0 {
		return m.DialTimeout
	}
	return defaultDialTimeout
}

// KnownHosts returns the underlying TOFU known-hosts store.
func (m *Manager) KnownHosts() *KnownHostsStore {
	return m.knownHosts
}

// TrustHostKey persists a user-confirmed SSH host key fingerprint.
func (m *Manager) TrustHostKey(ctx context.Context, hostPort, algorithm, fingerprint string) error {
	return m.knownHosts.TrustHostKey(ctx, hostPort, algorithm, fingerprint)
}

// TryAcquireExisting returns an active pooled tunnel dialer for (connID, cfgVer, endpointDigest)
// without requiring runtime credentials or initiating a new SSH handshake.
func (m *Manager) TryAcquireExisting(connID string, cfgVer uint64, endpointDigest string) (DialFunc, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, 0, false
	}
	if cfgVer < m.minConfigVer[connID] {
		return nil, 0, false
	}
	entry, ok := m.tunnels[connID]
	if !ok || entry.dead.Load() || entry.voluntary.Load() {
		return nil, 0, false
	}
	if entry.cfgVer != cfgVer || entry.endpointDigest != endpointDigest {
		return nil, 0, false
	}
	return entry.dialContext, entry.gen, true
}

// AcquireDialer returns a pooled SSH DialFunc and its monotonic tunnel generation.
// Concurrent calls matching (connID, cfgVer, endpointDigest, authDigest) coalesce into one
// independent handshake whose context is canceled only if all waiters cancel or the connection closes.
func (m *Manager) AcquireDialer(ctx context.Context, connID string, cfgVer uint64, cfg SSHConfig) (DialFunc, uint64, error) {
	epDigest := EndpointDigest(cfg)
	authDigest := AuthDigest(cfg)
	key := dialKey{
		connID:         connID,
		cfgVer:         cfgVer,
		endpointDigest: epDigest,
		authDigest:     authDigest,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, 0, ErrTunnelClosed
	}
	if cfgVer < m.minConfigVer[connID] {
		m.mu.Unlock()
		return nil, 0, ErrStaleConfig
	}
	if entry, ok := m.tunnels[connID]; ok && !entry.dead.Load() && !entry.voluntary.Load() {
		if entry.cfgVer == cfgVer && entry.endpointDigest == epDigest {
			m.mu.Unlock()
			return entry.dialContext, entry.gen, nil
		}
	}

	call, joining := m.inflight[key]
	if joining {
		call.waiters++
	} else {
		callCtx, callCancel := context.WithTimeout(m.baseCtx, m.dialTimeout(cfg))
		call = &dialCall{
			ctx:     callCtx,
			cancel:  callCancel,
			done:    make(chan struct{}),
			waiters: 1,
		}
		startEpoch := m.epochs[connID]
		m.inflight[key] = call
		go m.runDialCall(key, call, startEpoch, cfg)
	}
	m.mu.Unlock()

	select {
	case <-call.done:
		return call.dialFn, call.gen, call.err
	case <-ctx.Done():
		m.mu.Lock()
		call.waiters--
		if call.waiters <= 0 && m.inflight[key] == call {
			call.cancel()
		}
		m.mu.Unlock()
		return nil, 0, ctx.Err()
	}
}

func (m *Manager) runDialCall(key dialKey, call *dialCall, startEpoch uint64, cfg SSHConfig) {
	defer func() {
		call.cancel()
		m.mu.Lock()
		if m.inflight[key] == call {
			delete(m.inflight, key)
		}
		m.mu.Unlock()
		close(call.done)
	}()

	// Dedicated tunnel-lifetime context for HostKeyCallback so SSH rekey after initial handshake
	// continues verifying fingerprints until this tunnel or the manager closes.
	tunnelCtx, tunnelCancel := context.WithCancel(m.baseCtx)
	client, rawConn, err := m.dialSSHClient(call.ctx, tunnelCtx, cfg)
	if err != nil {
		tunnelCancel()
		call.err = err
		return
	}

	m.mu.Lock()
	// Re-verify generation/epoch/configVer before inserting into the pool.
	staleConfig := key.cfgVer < m.minConfigVer[key.connID]
	if m.closed || m.epochs[key.connID] != startEpoch || staleConfig {
		m.mu.Unlock()
		tunnelCancel()
		_ = client.Close()
		_ = rawConn.Close()
		if staleConfig {
			call.err = ErrStaleConfig
		} else {
			call.err = ErrTunnelClosed
		}
		return
	}

	// If another concurrent handshake (e.g. different authDigest) already installed a live tunnel
	// for the exact same (connID, cfgVer, endpointDigest), close the duplicate quietly and reuse.
	if existing, ok := m.tunnels[key.connID]; ok && !existing.dead.Load() && !existing.voluntary.Load() &&
		existing.cfgVer == key.cfgVer && existing.endpointDigest == key.endpointDigest {
		m.mu.Unlock()
		tunnelCancel()
		_ = client.Close()
		_ = rawConn.Close()
		call.dialFn = existing.dialContext
		call.gen = existing.gen
		return
	}

	// Replace any dead/older entry if present (voluntarily closing it without triggering onDisconnect).
	var replaced *pooledTunnel
	if old, ok := m.tunnels[key.connID]; ok {
		old.voluntary.Store(true)
		old.stopMonitor()
		replaced = old
	}

	entry := &pooledTunnel{
		mgr:            m,
		connID:         key.connID,
		gen:            m.nextGen.Add(1),
		cfgVer:         key.cfgVer,
		endpointDigest: key.endpointDigest,
		rawConn:        rawConn,
		client:         client,
		tunnelCancel:   tunnelCancel,
		stopCh:         make(chan struct{}),
	}
	m.tunnels[key.connID] = entry
	m.mu.Unlock()

	if replaced != nil {
		_ = replaced.client.Close()
		_ = replaced.rawConn.Close()
	}

	go m.monitorTunnel(entry)
	call.dialFn = entry.dialContext
	call.gen = entry.gen
}

// OpenTransient opens a one-shot SSH tunnel for TestConnection without inserting it into the pool
// or firing onDisconnect callbacks.
func (m *Manager) OpenTransient(ctx context.Context, cfg SSHConfig) (DialFunc, func(), error) {
	dialCtx, cancel := context.WithTimeout(ctx, m.dialTimeout(cfg))
	defer cancel()

	tunnelCtx, tunnelCancel := context.WithCancel(m.baseCtx)
	client, rawConn, err := m.dialSSHClient(dialCtx, tunnelCtx, cfg)
	if err != nil {
		tunnelCancel()
		return nil, nil, err
	}

	var closeOnce sync.Once
	closeFn := func() {
		closeOnce.Do(func() {
			tunnelCancel()
			_ = client.Close()
			_ = rawConn.Close()
		})
	}
	fatalStall := func() {
		_ = rawConn.Close()
	}

	dialFn := func(dctx context.Context, network, addr string) (net.Conn, error) {
		raw, derr := client.DialContext(dctx, network, addr)
		if derr != nil {
			return nil, derr
		}
		return newTunnelConn(raw, addr, m.channelCloseGrace(), fatalStall), nil
	}

	return dialFn, closeFn, nil
}

// InvalidateConfig advances minConfigVer and epoch for connID and voluntarily closes any active
// or in-flight tunnel for connID without firing abnormal onDisconnect callbacks.
func (m *Manager) InvalidateConfig(connID string, newMinVer uint64) {
	m.mu.Lock()
	if newMinVer > m.minConfigVer[connID] {
		m.minConfigVer[connID] = newMinVer
	}
	m.epochs[connID]++
	entry, ok := m.tunnels[connID]
	if ok {
		entry.voluntary.Store(true)
		entry.dead.Store(true)
		entry.stopMonitor()
		delete(m.tunnels, connID)
	}
	var toCancel []*dialCall
	for k, c := range m.inflight {
		if k.connID == connID {
			toCancel = append(toCancel, c)
		}
	}
	m.mu.Unlock()

	for _, c := range toCancel {
		c.cancel()
	}
	if ok && entry != nil {
		_ = entry.client.Close()
		_ = entry.rawConn.Close()
	}
}

// CloseConnection voluntarily closes the active tunnel and cancels any in-flight handshakes for connID.
// Voluntary closes NEVER invoke abnormal onDisconnect callbacks.
func (m *Manager) CloseConnection(connID string) {
	m.mu.Lock()
	m.epochs[connID]++
	entry, ok := m.tunnels[connID]
	if ok {
		entry.voluntary.Store(true)
		entry.dead.Store(true)
		entry.stopMonitor()
		delete(m.tunnels, connID)
	}
	var toCancel []*dialCall
	for k, c := range m.inflight {
		if k.connID == connID {
			toCancel = append(toCancel, c)
		}
	}
	m.mu.Unlock()

	for _, c := range toCancel {
		c.cancel()
	}
	if ok && entry != nil {
		_ = entry.client.Close()
		_ = entry.rawConn.Close()
	}
}

// CloseAll gracefully tears down all tunnels and cancels all in-flight handshakes (called on shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	m.closed = true
	entries := make([]*pooledTunnel, 0, len(m.tunnels))
	for id, entry := range m.tunnels {
		entry.voluntary.Store(true)
		entry.dead.Store(true)
		entry.stopMonitor()
		entries = append(entries, entry)
		delete(m.tunnels, id)
	}
	calls := make([]*dialCall, 0, len(m.inflight))
	for _, c := range m.inflight {
		calls = append(calls, c)
	}
	m.mu.Unlock()

	m.baseCancel()
	for _, c := range calls {
		c.cancel()
	}
	for _, entry := range entries {
		_ = entry.client.Close()
		_ = entry.rawConn.Close()
	}
}

func (m *Manager) monitorTunnel(entry *pooledTunnel) {
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- entry.client.Wait()
	}()

	ticker := time.NewTicker(m.keepaliveInterval())
	defer ticker.Stop()

	for {
		select {
		case <-entry.stopCh:
			return
		case <-waitCh:
			m.handleTunnelFailure(entry)
			return
		case <-ticker.C:
			if !m.sendKeepalive(entry) {
				m.handleTunnelFailure(entry)
				return
			}
		}
	}
}

// sendKeepalive sends one keepalive@openssh.com request with an independent response timeout.
// Returns true if the tunnel is alive (including ok == false && err == nil negative replies!).
func (m *Manager) sendKeepalive(entry *pooledTunnel) bool {
	type kaResult struct {
		ok  bool
		err error
	}
	resCh := make(chan kaResult, 1)
	go func() {
		ok, _, err := entry.client.SendRequest("keepalive@openssh.com", true, nil)
		resCh <- kaResult{ok: ok, err: err}
	}()

	timer := time.NewTimer(m.keepaliveTimeout())
	defer timer.Stop()

	select {
	case <-entry.stopCh:
		return false
	case res := <-resCh:
		// ok == false && err == nil means the server replied SSH_MSG_REQUEST_FAILURE:
		// the SSH transport IS alive and responsive, so never treat it as disconnected!
		return res.err == nil
	case <-timer.C:
		// Network blackhole: force-close rawConn to unblock SendRequest & client.Wait() and
		// reclaim the waiting goroutine cleanly.
		entry.dead.Store(true)
		_ = entry.rawConn.Close()
		<-resCh
		return false
	}
}

func (m *Manager) handleTunnelFailure(entry *pooledTunnel) {
	entry.dead.Store(true)
	entry.stopMonitor()
	_ = entry.rawConn.Close()
	_ = entry.client.Close()

	m.mu.Lock()
	if m.tunnels[entry.connID] == entry {
		delete(m.tunnels, entry.connID)
	}
	m.mu.Unlock()

	if !entry.voluntary.Load() && m.onDisconnect != nil {
		entry.disconnectOnce.Do(func() {
			go m.onDisconnect(entry.connID, entry.gen)
		})
	}
}

func (m *Manager) dialSSHClient(handshakeCtx, hostKeyParentCtx context.Context, cfg SSHConfig) (*ssh.Client, net.Conn, error) {
	normAddr := NormalizeHostPort(cfg.Host, cfg.Port)
	authMethod := strings.ToLower(strings.TrimSpace(cfg.AuthMethod))
	if authMethod == "" {
		authMethod = "password"
	}

	authMethods, agentCloser, err := m.buildAuthMethods(normAddr, authMethod, cfg)
	if err != nil {
		return nil, nil, err
	}
	if agentCloser != nil {
		// Register immediately so the agent socket/pipe stays open through ssh.NewClientConn
		// and is always closed when dialSSHClient returns.
		defer func() { _ = agentCloser.Close() }()
	}

	var rawConn net.Conn
	if m.dialTCP != nil {
		rawConn, err = m.dialTCP(handshakeCtx, "tcp", normAddr)
	} else {
		d := &net.Dialer{Timeout: m.dialTimeout(cfg), KeepAlive: 30 * time.Second}
		rawConn, err = d.DialContext(handshakeCtx, "tcp", normAddr)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("连接 SSH 跳板机 %s 失败: %w", normAddr, err)
	}

	sshCfg := &ssh.ClientConfig{
		User:            strings.TrimSpace(cfg.Username),
		Auth:            authMethods,
		HostKeyCallback: m.knownHosts.HostKeyCallback(hostKeyParentCtx, normAddr),
		Timeout:         m.dialTimeout(cfg),
	}
	if m.rekeyThreshold > 0 {
		sshCfg.Config.RekeyThreshold = m.rekeyThreshold
	}

	// Abort both rawConn and agentCloser if handshakeCtx is canceled during SSH handshake/signing.
	// context.AfterFunc guarantees that once stopCancel() returns true, the callback will NEVER run,
	// so cleaning up handshakeCtx after handoff can never close a live tunnel.
	afterDone := make(chan struct{})
	stopCancel := context.AfterFunc(handshakeCtx, func() {
		defer close(afterDone)
		_ = rawConn.Close()
		if agentCloser != nil {
			_ = agentCloser.Close()
		}
	})

	c, chans, reqs, err := ssh.NewClientConn(rawConn, normAddr, sshCfg)
	if !stopCancel() {
		// Cancellation callback already fired or started; wait for it to finish cleanly
		// and never hand back a connection whose rawConn is being closed.
		<-afterDone
		if c != nil {
			_ = c.Close()
		}
		_ = rawConn.Close()
		if err != nil {
			var challenge *HostKeyChallengeError
			if errors.As(err, &challenge) {
				return nil, nil, challenge
			}
		}
		ctxErr := handshakeCtx.Err()
		if ctxErr == nil {
			ctxErr = context.Canceled
		}
		return nil, nil, fmt.Errorf("SSH 握手已取消或超时 (%s): %w", normAddr, ctxErr)
	}
	if err != nil {
		_ = rawConn.Close()
		var challenge *HostKeyChallengeError
		if errors.As(err, &challenge) {
			return nil, nil, challenge
		}
		if handshakeCtx.Err() != nil {
			return nil, nil, fmt.Errorf("SSH 握手已取消或超时 (%s): %w", normAddr, handshakeCtx.Err())
		}
		if isSSHAuthError(err) {
			return nil, nil, &AuthFailedError{
				HostPort:       normAddr,
				Username:       strings.TrimSpace(cfg.Username),
				AuthMethod:     authMethod,
				NeedPassphrase: authMethod == "key",
				Reason:         "SSH 认证失败，请核对凭据后重试",
				Err:            err,
			}
		}
		return nil, nil, fmt.Errorf("SSH 握手失败 (%s): %w", normAddr, err)
	}

	return ssh.NewClient(c, chans, reqs), rawConn, nil
}

func (m *Manager) buildAuthMethods(normAddr, authMethod string, cfg SSHConfig) ([]ssh.AuthMethod, io.Closer, error) {
	username := strings.TrimSpace(cfg.Username)
	switch authMethod {
	case "password":
		if cfg.Password == "" {
			return nil, nil, &AuthFailedError{
				HostPort:   normAddr,
				Username:   username,
				AuthMethod: "password",
				Reason:     "请输入 SSH 密码",
			}
		}
		return []ssh.AuthMethod{
			ssh.Password(cfg.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = cfg.Password
				}
				return answers, nil
			}),
		}, nil, nil

	case "key":
		keyPath := strings.TrimSpace(cfg.PrivateKeyPath)
		if keyPath == "" {
			return nil, nil, fmt.Errorf("SSH 私钥路径不能为空")
		}
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 SSH 私钥文件失败: %w", err)
		}
		var signer ssh.Signer
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(cfg.Passphrase))
			if err != nil {
				return nil, nil, &AuthFailedError{
					HostPort:       normAddr,
					Username:       username,
					AuthMethod:     "key",
					NeedPassphrase: true,
					Reason:         "SSH 私钥口令不正确，请重新输入",
					Err:            err,
				}
			}
		} else {
			signer, err = ssh.ParsePrivateKey(keyBytes)
			if err != nil {
				var missing *ssh.PassphraseMissingError
				if errors.As(err, &missing) {
					return nil, nil, &AuthFailedError{
						HostPort:       normAddr,
						Username:       username,
						AuthMethod:     "key",
						NeedPassphrase: true,
						Reason:         "SSH 私钥受口令保护，请输入私钥口令",
						Err:            err,
					}
				}
				return nil, nil, fmt.Errorf("解析 SSH 私钥失败: %w", err)
			}
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil, nil

	case "agent":
		opener := m.openAgent
		if opener == nil {
			opener = openSSHAgent
		}
		agentClient, closer, err := opener()
		if err != nil {
			return nil, nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeysCallback(agentClient.Signers)}, closer, nil

	default:
		return nil, nil, fmt.Errorf("不支持的 SSH 认证方式: %s", cfg.AuthMethod)
	}
}

func isSSHAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain") ||
		strings.Contains(msg, "permission denied")
}
