package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"hesper/internal/driver"
)

type testSSHServerOpts struct {
	password           string
	authorizedPubKey   ssh.PublicKey
	rejectKeepalive    bool
	blackholeKeepalive bool
	ignoreChannelClose bool
	stallChannelReads  bool // Accept direct-tcpip channel but never read from it (exhausts remote SSH window)
	handshakeDelay     time.Duration
	onDirectTCPIP      func(ch ssh.Channel, targetAddr string)
}

type testSSHServer struct {
	addr     string
	host     string
	port     int
	hostKey  ssh.Signer
	ln       net.Listener
	mu       sync.Mutex
	rawConns []net.Conn
	opts     testSSHServerOpts
}

func startTestSSHServer(t *testing.T, opts testSSHServerOpts) *testSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new host signer: %v", err)
	}
	return startTestSSHServerWithSigner(t, signer, opts)
}

func startTestSSHServerWithSigner(t *testing.T, hostSigner ssh.Signer, opts testSSHServerOpts) *testSSHServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ssh: %v", err)
	}

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}

	s := &testSSHServer{
		addr:    ln.Addr().String(),
		host:    host,
		port:    port,
		hostKey: hostSigner,
		ln:      ln,
		opts:    opts,
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if opts.handshakeDelay > 0 {
				time.Sleep(opts.handshakeDelay)
			}
			if opts.password != "" && string(pass) == opts.password {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("invalid password")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if opts.handshakeDelay > 0 {
				time.Sleep(opts.handshakeDelay)
			}
			if opts.authorizedPubKey != nil && bytes.Equal(key.Marshal(), opts.authorizedPubKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	cfg.AddHostKey(hostSigner)

	go func() {
		for {
			raw, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			s.mu.Lock()
			s.rawConns = append(s.rawConns, raw)
			s.mu.Unlock()
			go s.handleConn(raw, cfg)
		}
	}()

	t.Cleanup(func() {
		s.Close()
	})
	return s
}

func (s *testSSHServer) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	conns := s.rawConns
	s.rawConns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *testSSHServer) handleConn(raw net.Conn, cfg *ssh.ServerConfig) {
	defer raw.Close()
	sConn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer sConn.Close()

	go func() {
		for req := range reqs {
			if req.Type == "keepalive@openssh.com" {
				if s.opts.blackholeKeepalive {
					// Deliberately never reply to simulate a network blackhole.
					continue
				}
				if req.WantReply {
					_ = req.Reply(!s.opts.rejectKeepalive, nil)
				}
				continue
			}
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()

	for newChan := range chans {
		if newChan.ChannelType() != "direct-tcpip" {
			_ = newChan.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		var payload struct {
			DestAddr   string
			DestPort   uint32
			OriginAddr string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
			_ = newChan.Reject(ssh.ConnectionFailed, "bad payload")
			continue
		}
		target := NormalizeHostPort(payload.DestAddr, int(payload.DestPort))
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(chReqs)

		if s.opts.onDirectTCPIP != nil {
			go s.opts.onDirectTCPIP(ch, target)
			continue
		}

		if s.opts.stallChannelReads || s.opts.ignoreChannelClose {
			// Hold the channel open without reading or closing to exercise window exhaustion
			// and unresponsive peer close escalation against real x/crypto/ssh v0.57.0.
			go func(c ssh.Channel) {
				if !s.opts.ignoreChannelClose {
					defer c.Close()
				}
				// Wait until transport tears down.
				_ = sConn.Wait()
			}(ch)
			continue
		}

		go func(c ssh.Channel, dst string) {
			defer c.Close()
			up, derr := net.DialTimeout("tcp", dst, 3*time.Second)
			if derr != nil {
				return
			}
			defer up.Close()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(up, c)
				if tc, ok := up.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
			}()
			go func() {
				defer wg.Done()
				_, _ = io.Copy(c, up)
				_ = c.CloseWrite()
			}()
			wg.Wait()
		}(ch, target)
	}
}

type memKVStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func newMemKVStore() *memKVStore {
	return &memKVStore{data: make(map[string]string)}
}

func (m *memKVStore) GetKV(_ context.Context, key string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	return v, ok, nil
}

func (m *memKVStore) SetKV(_ context.Context, key, val string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = val
	return nil
}

func TestTunnelConnRemoteAddrAndReadDeadlines(t *testing.T) {
	// Echo TCP target
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 1024)
				for {
					n, rerr := conn.Read(buf)
					if n > 0 {
						// Delay response slightly so read deadline tests can control timing.
						time.Sleep(120 * time.Millisecond)
						_, _ = conn.Write(buf[:n])
					}
					if rerr != nil {
						return
					}
				}
			}(c)
		}
	}()

	srv := startTestSSHServer(t, testSSHServerOpts{password: "secret"})
	kv := newMemKVStore()
	mgr := NewManager(kv, nil)
	defer mgr.CloseAll()
	if err := mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srv.hostKey.PublicKey())); err != nil {
		t.Fatalf("trust host key: %v", err)
	}

	dialFn, _, err := mgr.AcquireDialer(context.Background(), "c1", 1, SSHConfig{
		Host:       srv.host,
		Port:       srv.port,
		Username:   "tester",
		AuthMethod: "password",
		Password:   "secret",
	})
	if err != nil {
		t.Fatalf("AcquireDialer: %v", err)
	}

	conn, err := dialFn(context.Background(), "tcp", echoLn.Addr().String())
	if err != nil {
		t.Fatalf("dial through tunnel: %v", err)
	}
	defer conn.Close()

	// 1. RemoteAddr() MUST report real target address, NOT 0.0.0.0:0.
	if conn.RemoteAddr().Network() != "tcp" || conn.RemoteAddr().String() != echoLn.Addr().String() {
		t.Fatalf("RemoteAddr = %s://%s, want tcp://%s", conn.RemoteAddr().Network(), conn.RemoteAddr().String(), echoLn.Addr().String())
	}

	// 2. Short ReadDeadline times out non-destructively, then clearing deadline recovers the exact bytes.
	if _, err := conn.Write([]byte("hello-ssh")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	buf := make([]byte, 32)
	_, err = conn.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected os.ErrDeadlineExceeded, got %v", err)
	}

	// Clear deadline -> subsequent Read receives intact stream!
	_ = conn.SetReadDeadline(time.Time{})
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read after clearing deadline failed: %v", err)
	}
	if string(buf[:n]) != "hello-ssh" {
		t.Fatalf("got %q, want hello-ssh", string(buf[:n]))
	}

	// 3. Extending ReadDeadline (t2 > t1) prevents stale t1 timer from firing early.
	if _, err := conn.Write([]byte("extend-ok")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	go func() {
		time.Sleep(15 * time.Millisecond)
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	}()
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("read after extending deadline failed: %v", err)
	}
	if string(buf[:n]) != "extend-ok" {
		t.Fatalf("got %q, want extend-ok", string(buf[:n]))
	}
}

// freezableConn wraps a real TCP net.Conn so a test can freeze all inbound/outbound traffic
// mid-flight (simulating a true TCP blackhole where SSH_MSG_CHANNEL_CLOSE gets no reply)
// until Close() tears down the underlying socket.
type freezableConn struct {
	net.Conn
	frozen   atomic.Bool
	unfreeze chan struct{}
	once     sync.Once
}

func newFreezableConn(c net.Conn) *freezableConn {
	return &freezableConn{Conn: c, unfreeze: make(chan struct{})}
}

func (f *freezableConn) freeze() { f.frozen.Store(true) }

func (f *freezableConn) Read(p []byte) (int, error) {
	if f.frozen.Load() {
		<-f.unfreeze
		return 0, net.ErrClosed
	}
	n, err := f.Conn.Read(p)
	if f.frozen.Load() {
		<-f.unfreeze
		return 0, net.ErrClosed
	}
	return n, err
}

func (f *freezableConn) Write(p []byte) (int, error) {
	if f.frozen.Load() {
		<-f.unfreeze
		return 0, net.ErrClosed
	}
	return f.Conn.Write(p)
}

func (f *freezableConn) Close() error {
	f.once.Do(func() { close(f.unfreeze) })
	return f.Conn.Close()
}

// TestTunnelConnWindowExhaustedWriteTimeoutPreservesPartialNAndEscalatesWithoutDeadlock
// exercises real golang.org/x/crypto/ssh v0.57.0 when remote window hits 0 AND the underlying
// TCP transport blackholes SSH_MSG_CHANNEL_CLOSE.
// Verifies: exact partial n > 0 preserved, TrackedConn.WriteSentWithoutResponse() == true,
// fatal-stall escalation fires onDisconnect, and a simulated database/sql execution lock inside
// onDisconnect NEVER deadlocks Write!
func TestTunnelConnWindowExhaustedWriteTimeoutPreservesPartialNAndEscalatesWithoutDeadlock(t *testing.T) {
	srv := startTestSSHServer(t, testSSHServerOpts{
		password:           "secret",
		stallChannelReads:  true,
		ignoreChannelClose: true,
	})

	var execLock sync.Mutex
	disconnectFired := make(chan uint64, 1)

	kv := newMemKVStore()
	mgr := NewManager(kv, func(connID string, lostGen uint64) {
		// Simulate mysql.Conn.Close() -> sql.Conn.Close() waiting on the in-flight Exec lock!
		// Because fatalStall dispatches onDisconnect asynchronously, Write unwinds and releases
		// execLock BEFORE this completes — zero re-entrant deadlock!
		execLock.Lock()
		defer execLock.Unlock()
		select {
		case disconnectFired <- lostGen:
		default:
		}
	})
	mgr.ChannelCloseGrace = 60 * time.Millisecond
	defer mgr.CloseAll()

	var fcMu sync.Mutex
	var activeFC *freezableConn
	mgr.dialTCP = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		raw, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		fc := newFreezableConn(raw)
		fcMu.Lock()
		activeFC = fc
		fcMu.Unlock()
		return fc, nil
	}

	_ = mgr.TrustHostKey(context.Background(), srv.addr, srv.hostKey.PublicKey().Type(), ssh.FingerprintSHA256(srv.hostKey.PublicKey()))

	dialFn, gen, err := mgr.AcquireDialer(context.Background(), "c-stall", 1, SSHConfig{
		Host:       srv.host,
		Port:       srv.port,
		Username:   "tester",
		AuthMethod: "password",
		Password:   "secret",
	})
	if err != nil {
		t.Fatalf("AcquireDialer: %v", err)
	}

	rawCh, err := dialFn(context.Background(), "tcp", "127.0.0.1:3306")
	if err != nil {
		t.Fatalf("dial channel: %v", err)
	}
	tracked := driver.NewTrackedConn(rawCh)
	tracked.Reset()

	// Payload larger than SSH default initial channel window (~2MB) so partial bytes are
	// sent onto the wire before remoteWin.reserve blocks indefinitely!
	bigPayload := bytes.Repeat([]byte("A"), 4*1024*1024)

	// Freeze the underlying SSH TCP transport right after the first partial chunks hit the wire
	// so SSH_MSG_CHANNEL_CLOSE cannot receive a reply and ChannelCloseGrace MUST escalate!
	go func() {
		time.Sleep(30 * time.Millisecond)
		fcMu.Lock()
		if activeFC != nil {
			activeFC.freeze()
		}
		fcMu.Unlock()
	}()

	execLock.Lock()
	_ = tracked.SetWriteDeadline(time.Now().Add(80 * time.Millisecond))
	n, werr := tracked.Write(bigPayload)
	execLock.Unlock()

	if !errors.Is(werr, os.ErrDeadlineExceeded) {
		t.Fatalf("expected os.ErrDeadlineExceeded on stalled write, got n=%d err=%v", n, werr)
	}
	if n <= 0 {
		t.Fatalf("MUST preserve partial written byte count n > 0 before window stall, got n=%d", n)
	}
	if !tracked.WriteSentWithoutResponse() {
		t.Fatalf("TrackedConn.WriteSentWithoutResponse() MUST be true after partial write n=%d", n)
	}

	// Escalation must fire onDisconnect(connID, gen) and tear down the tunnel cleanly.
	select {
	case lostGen := <-disconnectFired:
		if lostGen != gen {
			t.Fatalf("expected lostGen=%d, got %d", gen, lostGen)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected fatal-stall escalation to fire onDisconnect without deadlock")
	}

	_ = tracked.Close()
}

// TestTunnelConnCloseWhenReadBufferFullAndUnresponsivePeer verifies zero goroutine leaks when
// closing a tunnelConn whose 32KB ring buffer is completely full or whose peer ignores Close.
func TestTunnelConnCloseWhenReadBufferFullAndUnresponsivePeer(t *testing.T) {
	// 1. Buffer-full Close test (reader goroutine blocked waiting for ring buffer space).
	c1, c2 := net.Pipe()
	defer c2.Close()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		chunk := bytes.Repeat([]byte("X"), 4096)
		for i := 0; i < 16; i++ { // 64KB > 32KB ring buffer
			if _, err := c2.Write(chunk); err != nil {
				return
			}
		}
	}()

	tc := newTunnelConn(c1, "127.0.0.1:5432", 60*time.Millisecond, nil)
	// Wait until 32KB ring buffer is full and readLoop is waiting on rSpaceNotify.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tc.rMu.Lock()
		full := tc.rLen == len(tc.rBuf)
		tc.rMu.Unlock()
		if full {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	closeStart := time.Now()
	if err := tc.Close(); err != nil {
		t.Fatalf("Close on full buffer failed: %v", err)
	}
	if time.Since(closeStart) > 500*time.Millisecond {
		t.Fatalf("Close on full buffer took too long: %v", time.Since(closeStart))
	}
	<-writerDone

	// 2. Blocked ch.Close() + unresponsive Read still triggers independent closeGrace -> onFatalStall!
	beforeG := runtime.NumGoroutine()
	enteredRead := make(chan struct{})
	blockedRead := make(chan struct{})
	var escalated atomic.Bool
	fakeRaw := &blockingCloseConn{
		enteredRead: enteredRead,
		readBlock:   blockedRead,
		closeBlock:  make(chan struct{}),
	}
	tc2 := newTunnelConn(fakeRaw, "127.0.0.1:5432", 50*time.Millisecond, func() {
		escalated.Store(true)
		close(blockedRead) // simulates raw TCP close tearing down ssh.mux
	})
	<-enteredRead

	start := time.Now()
	if err := tc2.Close(); err != nil {
		t.Fatalf("Close with blocked raw.Close failed: %v", err)
	}
	if !escalated.Load() {
		t.Fatalf("expected independent closeGrace timer to trigger onFatalStall even when raw.Close() blocks")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("escalation took too long: %v", time.Since(start))
	}
	// Release fake raw.Close() helper goroutine so we can check goroutine count cleanly.
	close(fakeRaw.closeBlock)
	time.Sleep(30 * time.Millisecond)
	if afterG := runtime.NumGoroutine(); afterG > beforeG+2 {
		t.Fatalf("goroutine leak detected: before=%d after=%d", beforeG, afterG)
	}
}

type blockingCloseConn struct {
	enteredOnce sync.Once
	enteredRead chan struct{}
	readBlock   chan struct{}
	closeBlock  chan struct{}
}

func (b *blockingCloseConn) Read(_ []byte) (int, error) {
	b.enteredOnce.Do(func() {
		if b.enteredRead != nil {
			close(b.enteredRead)
		}
	})
	<-b.readBlock
	return 0, io.EOF
}
func (b *blockingCloseConn) Write(p []byte) (int, error)       { return len(p), nil }
func (b *blockingCloseConn) Close() error                      { <-b.closeBlock; return nil }
func (b *blockingCloseConn) LocalAddr() net.Addr               { return staticTCPAddr("127.0.0.1:1") }
func (b *blockingCloseConn) RemoteAddr() net.Addr              { return staticTCPAddr("0.0.0.0:0") }
func (b *blockingCloseConn) SetDeadline(_ time.Time) error     { return nil }
func (b *blockingCloseConn) SetReadDeadline(_ time.Time) error { return nil }
func (b *blockingCloseConn) SetWriteDeadline(_ time.Time) error {
	return nil
}
