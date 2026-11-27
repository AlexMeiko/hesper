package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

// FaultBehavior controls deterministic fault injection for pure unit tests in internal/session & internal/service.
// Never counted as real database acceptance (per Criterion #4).
type FaultBehavior struct {
	QueryDelay        time.Duration
	CancelDelay       time.Duration
	CancelConfirmed   bool
	DropAfterWrite    bool // simulates EOF after wire write with zero server response bytes
	ReturnReadOnlyErr bool
}

type Opener struct {
	mu       sync.Mutex
	Behavior FaultBehavior
	Opened   atomic.Int32
	Closed   atomic.Int32
}

func NewOpener(b FaultBehavior) *Opener {
	return &Opener{Behavior: b}
}

func (o *Opener) Type() dto.EngineType { return dto.EngineFake }

func (o *Opener) Open(_ context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	o.Opened.Add(1)
	o.mu.Lock()
	b := o.Behavior
	o.mu.Unlock()
	return &Conn{
		opener:   o,
		behavior: b,
		dbName:   cfg.Database,
		readOnly: cfg.ReadOnly,
	}, nil
}

type Conn struct {
	mu           sync.Mutex
	opener       *Opener
	behavior     FaultBehavior
	dbName       string
	readOnly     bool
	closed       bool
	writeSent    bool
	cancelCh     chan struct{}
	QueryCount   atomic.Int32
	ExecCount    atomic.Int32
}

func (c *Conn) Ping(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("connection closed")
	}
	return nil
}

func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.opener.Closed.Add(1)
	}
	return nil
}

func (c *Conn) ServerFact(_ context.Context) (driver.ServerFact, error) {
	return driver.ServerFact{
		DetectedType:  dto.EngineFake,
		ServerVersion: "fake-1.0.0",
		CurrentDB:     c.dbName,
		ServerConnID:  "fake-1",
	}, nil
}

func (c *Conn) Dialect() dto.EngineType { return dto.EngineFake }

func (c *Conn) Capabilities(_ string) dto.Capabilities {
	return dto.Capabilities{
		HasDatabases:   true,
		SupportsCancel: true,
		SupportsViews:  true,
	}
}

func (c *Conn) ResetWireTracker() {
	c.mu.Lock()
	c.writeSent = false
	c.mu.Unlock()
}

func (c *Conn) WriteSentWithoutResponse() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeSent
}

func (c *Conn) ClassifyError(err error, _ bool) *dto.AppError {
	if err == nil {
		return nil
	}
	var ae *dto.AppError
	if errors.As(err, &ae) {
		return ae
	}
	msg := err.Error()
	if msg == "unexpected EOF" || msg == "EOF" || msg == "connection closed" {
		return dto.NewAppError(dto.ErrSessionLost, "fake session lost", msg)
	}
	return dto.NewAppError(dto.ErrInternal, msg, msg)
}

func (c *Conn) Query(_ context.Context, stmt driver.BoundStatement) (driver.RowStream, error) {
	c.QueryCount.Add(1)
	c.mu.Lock()
	ch := make(chan struct{})
	c.cancelCh = ch
	delay := c.behavior.QueryDelay
	c.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ch:
			return nil, dto.NewAppError(dto.ErrStatementCanceled, "fake query canceled", "")
		}
	}
	cols := []dto.ColumnMeta{{Name: "val", DatabaseType: "VARCHAR", Kind: dto.KindString}}
	rows := [][]dto.CellValue{
		{{Kind: dto.KindString, Text: stmt.SQL}},
		{{Kind: dto.KindString, Text: "row-2"}},
	}
	return &fakeStream{cols: cols, rows: rows}, nil
}

func (c *Conn) Exec(_ context.Context, _ driver.BoundStatement) (driver.ExecFact, error) {
	c.ExecCount.Add(1)
	c.mu.Lock()
	b := c.behavior
	c.mu.Unlock()

	if b.ReturnReadOnlyErr {
		return driver.ExecFact{}, dto.NewAppError(dto.ErrReadOnlyDenied, "fake read-only denied", "")
	}
	if b.DropAfterWrite {
		c.mu.Lock()
		c.writeSent = true
		c.closed = true
		c.mu.Unlock()
		return driver.ExecFact{}, errors.New("unexpected EOF")
	}
	return driver.ExecFact{AffectedRows: 1}, nil
}

func (c *Conn) CancelInFlight(_ context.Context, _ driver.Conn) (bool, error) {
	c.mu.Lock()
	delay := c.behavior.CancelDelay
	confirmed := c.behavior.CancelConfirmed
	ch := c.cancelCh
	c.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	if !confirmed {
		return false, fmt.Errorf("cancel confirmation timed out")
	}
	return true, nil
}

func (c *Conn) SwitchDatabase(_ context.Context, db string) error {
	c.mu.Lock()
	c.dbName = db
	c.mu.Unlock()
	return nil
}

type fakeStream struct {
	cols []dto.ColumnMeta
	rows [][]dto.CellValue
	idx  int
}

func (s *fakeStream) Columns() []dto.ColumnMeta { return s.cols }
func (s *fakeStream) Close() error              { return nil }
func (s *fakeStream) Next(_ driver.BlobRegistrar) ([]dto.CellValue, bool, error) {
	if s.idx >= len(s.rows) {
		return nil, false, nil
	}
	r := s.rows[s.idx]
	s.idx++
	return r, true, nil
}
