package tunnel

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

const (
	defaultChannelCloseGrace = 2 * time.Second
	tunnelReadBufSize        = 32 * 1024
)

type staticTCPAddr string

func (a staticTCPAddr) Network() string { return "tcp" }
func (a staticTCPAddr) String() string  { return string(a) }

type writeResult struct {
	n   int
	err error
}

// tunnelConn wraps an SSH direct-tcpip channel net.Conn to provide:
//  1. Real target RemoteAddr() so PostgreSQL PgConn.CancelRequest dials the target DB over SSH
//     instead of 0.0.0.0:0.
//  2. Full SetDeadline / SetReadDeadline / SetWriteDeadline semantics (SSH chanConn rejects all deadlines).
//  3. Exact partial-write byte accounting (preserving n > 0 for TrackedConn.WriteSentWithoutResponse).
//  4. Independent close-grace escalation (onFatalStall) that tears down the underlying SSH raw TCP
//     transport if the remote peer ignores SSH_MSG_CHANNEL_CLOSE or the shared TCP transport stalls.
type tunnelConn struct {
	raw          net.Conn
	remoteAddr   net.Addr
	localAddr    net.Addr
	onFatalStall func()
	closeGrace   time.Duration

	closedCh     chan struct{}
	closeOnce    sync.Once
	rawCloseOnce sync.Once
	escalateOnce sync.Once

	readerDone chan struct{}

	// Read ring buffer & deadline state.
	rMu             sync.Mutex
	rBuf            [tunnelReadBufSize]byte
	rHead           int
	rLen            int
	rErr            error
	rClosed         bool
	rDeadline       time.Time
	rDataNotify     chan struct{}
	rSpaceNotify    chan struct{}
	rDeadlineNotify chan struct{}

	// Write serialization & deadline state.
	// wExecMu serializes raw.Write calls (ssh.channel.Write is not concurrency-safe).
	// wMu guards wDeadline/wClosed independently so SetWriteDeadline never blocks on a stalled Write.
	wExecMu         sync.Mutex
	wMu             sync.Mutex
	wClosed         bool
	wDeadline       time.Time
	wDeadlineNotify chan struct{}
}

func newTunnelConn(raw net.Conn, targetAddr string, closeGrace time.Duration, onFatalStall func()) *tunnelConn {
	if closeGrace <= 0 {
		closeGrace = defaultChannelCloseGrace
	}
	laddr := raw.LocalAddr()
	if laddr == nil {
		laddr = staticTCPAddr("127.0.0.1:0")
	}
	tc := &tunnelConn{
		raw:             raw,
		remoteAddr:      staticTCPAddr(targetAddr),
		localAddr:       laddr,
		onFatalStall:    onFatalStall,
		closeGrace:      closeGrace,
		closedCh:        make(chan struct{}),
		readerDone:      make(chan struct{}),
		rDataNotify:     make(chan struct{}, 1),
		rSpaceNotify:    make(chan struct{}, 1),
		rDeadlineNotify: make(chan struct{}, 1),
		wDeadlineNotify: make(chan struct{}, 1),
	}
	go tc.readLoop()
	return tc
}

func notifyChan(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (c *tunnelConn) beginAsyncRawClose() {
	c.rawCloseOnce.Do(func() {
		go func() {
			_ = c.raw.Close()
		}()
	})
}

func (c *tunnelConn) escalateFatalStall() {
	c.escalateOnce.Do(func() {
		if c.onFatalStall != nil {
			c.onFatalStall()
		}
	})
}

func (c *tunnelConn) readLoop() {
	defer close(c.readerDone)
	var scratch [8192]byte

	for {
		c.rMu.Lock()
		for c.rLen == len(c.rBuf) && !c.rClosed {
			c.rMu.Unlock()
			select {
			case <-c.rSpaceNotify:
			case <-c.closedCh:
				return
			}
			c.rMu.Lock()
		}
		if c.rClosed {
			c.rMu.Unlock()
			return
		}
		avail := len(c.rBuf) - c.rLen
		if avail > len(scratch) {
			avail = len(scratch)
		}
		c.rMu.Unlock()

		n, err := c.raw.Read(scratch[:avail])

		c.rMu.Lock()
		if n > 0 {
			c.pushRingLocked(scratch[:n])
		}
		if err != nil {
			c.rErr = err
		}
		notifyChan(c.rDataNotify)
		shouldExit := err != nil || c.rClosed
		c.rMu.Unlock()

		if shouldExit {
			return
		}
	}
}

func (c *tunnelConn) pushRingLocked(src []byte) {
	tail := (c.rHead + c.rLen) % len(c.rBuf)
	first := len(c.rBuf) - tail
	if first >= len(src) {
		copy(c.rBuf[tail:], src)
	} else {
		copy(c.rBuf[tail:], src[:first])
		copy(c.rBuf[0:], src[first:])
	}
	c.rLen += len(src)
}

func (c *tunnelConn) popRingLocked(dst []byte) int {
	n := len(dst)
	if n > c.rLen {
		n = c.rLen
	}
	first := len(c.rBuf) - c.rHead
	if first >= n {
		copy(dst[:n], c.rBuf[c.rHead:c.rHead+n])
	} else {
		copy(dst[:first], c.rBuf[c.rHead:])
		copy(dst[first:n], c.rBuf[:n-first])
	}
	c.rHead = (c.rHead + n) % len(c.rBuf)
	c.rLen -= n
	return n
}

func (c *tunnelConn) Read(p []byte) (int, error) {
	for {
		c.rMu.Lock()
		if c.rClosed {
			c.rMu.Unlock()
			return 0, net.ErrClosed
		}
		dl := c.rDeadline
		if !dl.IsZero() && !time.Now().Before(dl) {
			c.rMu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if c.rLen > 0 {
			n := c.popRingLocked(p)
			notifyChan(c.rSpaceNotify)
			c.rMu.Unlock()
			return n, nil
		}
		if c.rErr != nil {
			err := c.rErr
			c.rMu.Unlock()
			return 0, err
		}
		c.rMu.Unlock()

		if dl.IsZero() {
			select {
			case <-c.rDataNotify:
			case <-c.rDeadlineNotify:
			case <-c.closedCh:
				return 0, net.ErrClosed
			}
			continue
		}

		wait := time.Until(dl)
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-c.rDataNotify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-c.rDeadlineNotify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			// Loop back and re-verify c.rDeadline under rMu so a concurrently extended
			// or cleared deadline cannot be falsely timed out by a stale timer.
		case <-c.closedCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return 0, net.ErrClosed
		}
	}
}

func (c *tunnelConn) Write(p []byte) (int, error) {
	c.wExecMu.Lock()
	defer c.wExecMu.Unlock()

	c.wMu.Lock()
	if c.wClosed {
		c.wMu.Unlock()
		return 0, net.ErrClosed
	}
	if !c.wDeadline.IsZero() && !time.Now().Before(c.wDeadline) {
		c.wMu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	c.wMu.Unlock()

	done := make(chan writeResult, 1)
	go func() {
		n, err := c.raw.Write(p)
		done <- writeResult{n: n, err: err}
	}()

	for {
		c.wMu.Lock()
		closed := c.wClosed
		dl := c.wDeadline
		c.wMu.Unlock()

		if closed {
			return c.awaitWriteUnwind(done, net.ErrClosed)
		}
		if !dl.IsZero() && !time.Now().Before(dl) {
			return c.awaitWriteUnwind(done, os.ErrDeadlineExceeded)
		}

		if dl.IsZero() {
			select {
			case res := <-done:
				return res.n, res.err
			case <-c.wDeadlineNotify:
			case <-c.closedCh:
				return c.awaitWriteUnwind(done, net.ErrClosed)
			}
			continue
		}

		wait := time.Until(dl)
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case res := <-done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return res.n, res.err
		case <-c.wDeadlineNotify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			// Re-verify under wMu on next iteration so an extended/cleared deadline wins cleanly.
		case <-c.closedCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return c.awaitWriteUnwind(done, net.ErrClosed)
		}
	}
}

// awaitWriteUnwind closes the underlying channel asynchronously, waits up to closeGrace for
// the in-flight raw.Write to unwind and report exact partial n, and escalates to onFatalStall
// if the peer ignores close or the shared TCP transport stalls.
func (c *tunnelConn) awaitWriteUnwind(done <-chan writeResult, timeoutOrCloseErr error) (int, error) {
	c.beginAsyncRawClose()
	grace := time.NewTimer(c.closeGrace)
	defer grace.Stop()

	select {
	case res := <-done:
		if res.err != nil {
			return res.n, timeoutOrCloseErr
		}
		return res.n, nil
	case <-grace.C:
		c.escalateFatalStall()
		res := <-done
		if res.err != nil {
			return res.n, timeoutOrCloseErr
		}
		return res.n, nil
	}
}

func (c *tunnelConn) Close() error {
	c.closeOnce.Do(func() {
		c.rMu.Lock()
		c.rClosed = true
		c.rMu.Unlock()

		c.wMu.Lock()
		c.wClosed = true
		c.wMu.Unlock()

		close(c.closedCh)
		notifyChan(c.rDataNotify)
		notifyChan(c.rSpaceNotify)
		notifyChan(c.wDeadlineNotify)
		c.beginAsyncRawClose()
	})

	// Wait independently of raw.Close() for the background reader goroutine to exit.
	grace := time.NewTimer(c.closeGrace)
	defer grace.Stop()

	select {
	case <-c.readerDone:
		return nil
	case <-grace.C:
		c.escalateFatalStall()
		if c.onFatalStall != nil {
			<-c.readerDone
			return nil
		}
		return errors.New("ssh channel close timed out without fatal-stall escalation callback")
	}
}

func (c *tunnelConn) LocalAddr() net.Addr  { return c.localAddr }
func (c *tunnelConn) RemoteAddr() net.Addr { return c.remoteAddr }

func (c *tunnelConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	_ = c.SetWriteDeadline(t)
	return nil
}

func (c *tunnelConn) SetReadDeadline(t time.Time) error {
	c.rMu.Lock()
	c.rDeadline = t
	c.rMu.Unlock()
	notifyChan(c.rDeadlineNotify)
	return nil
}

func (c *tunnelConn) SetWriteDeadline(t time.Time) error {
	c.wMu.Lock()
	c.wDeadline = t
	c.wMu.Unlock()
	notifyChan(c.wDeadlineNotify)
	return nil
}
