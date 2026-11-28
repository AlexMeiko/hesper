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

// FaultBehavior controls deterministic fault injection for pure unit tests in internal/session, internal/changeset & internal/service.
// Never counted as real database acceptance (per Criterion #4).
type FaultBehavior struct {
	QueryDelay                   time.Duration
	CancelDelay                  time.Duration
	OpenDelay                    time.Duration
	CancelConfirmed              bool
	DropAfterWrite               bool // simulates EOF after wire write with zero server response bytes
	ReturnReadOnlyErr            bool
	WriteSchema                  *driver.WriteSchema
	MutateSchemaBeforeBeginWrite func() driver.WriteSchema
	BeginWriteErr                error
	ExecAffectedRows             []int64
	ExecErrAtStep                map[int]error
	CommitResultOverride         *driver.CommitResult
	DropOnCommitZeroBytes        bool
	DropOnCommitPartialBytes     bool // simulates COMMIT written + partial response bytes received before EOF
}

type Opener struct {
	mu       sync.Mutex
	Behavior FaultBehavior
	Opened   atomic.Int32
	Closed   atomic.Int32
	ExecLog  []driver.BoundStatement
}

func NewOpener(b FaultBehavior) *Opener {
	return &Opener{Behavior: b}
}

func (o *Opener) SetBehavior(b FaultBehavior) {
	o.mu.Lock()
	o.Behavior = b
	o.mu.Unlock()
}

func (o *Opener) ExecutedStatements() []driver.BoundStatement {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]driver.BoundStatement, len(o.ExecLog))
	copy(out, o.ExecLog)
	return out
}

func (o *Opener) Type() dto.EngineType { return dto.EngineFake }

func (o *Opener) Open(ctx context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	o.mu.Lock()
	b := o.Behavior
	o.mu.Unlock()
	if b.OpenDelay > 0 {
		select {
		case <-time.After(b.OpenDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	o.Opened.Add(1)
	return &Conn{
		opener:   o,
		behavior: b,
		dbName:   cfg.Database,
		readOnly: cfg.ReadOnly,
	}, nil
}

type Conn struct {
	mu         sync.Mutex
	opener     *Opener
	behavior   FaultBehavior
	dbName     string
	readOnly   bool
	closed     bool
	writeSent  bool
	cancelCh   chan struct{}
	QueryCount atomic.Int32
	ExecCount  atomic.Int32
}

func (c *Conn) Ping(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
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

func (c *Conn) Exec(_ context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	c.ExecCount.Add(1)
	c.opener.mu.Lock()
	c.opener.ExecLog = append(c.opener.ExecLog, stmt)
	c.opener.mu.Unlock()
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

func (c *Conn) InspectWriteSchema(_ context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.WriteSchema, error) {
	c.opener.mu.Lock()
	b := c.opener.Behavior
	c.opener.mu.Unlock()

	if b.WriteSchema != nil {
		ws := *b.WriteSchema
		if clientReadOnly || c.readOnly {
			ws.Editability.CanWrite = false
		}
		return ws, nil
	}
	return defaultFakeWriteSchema(target, clientReadOnly || c.readOnly), nil
}

func (c *Conn) BeginTableWrite(_ context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.TableWriteTx, error) {
	c.opener.mu.Lock()
	b := c.opener.Behavior
	c.opener.mu.Unlock()

	if b.BeginWriteErr != nil {
		return nil, b.BeginWriteErr
	}
	var ws driver.WriteSchema
	if b.MutateSchemaBeforeBeginWrite != nil {
		ws = b.MutateSchemaBeforeBeginWrite()
	} else if b.WriteSchema != nil {
		ws = *b.WriteSchema
	} else {
		ws = defaultFakeWriteSchema(target, clientReadOnly || c.readOnly)
	}
	if clientReadOnly || c.readOnly {
		ws.Editability.CanWrite = false
	}
	return &fakeTableWriteTx{
		conn:     c,
		behavior: b,
		schema:   ws,
	}, nil
}

func defaultFakeWriteSchema(target driver.WriteTarget, ro bool) driver.WriteSchema {
	return driver.WriteSchema{
		Dialect:       dto.EngineMySQL,
		Database:      target.Database,
		Schema:        target.Schema,
		Table:         target.Table,
		StorageEngine: "InnoDB",
		Editability: dto.TableEditability{
			CanWrite:           !ro,
			RowIdentity:        dto.RowIdentityPrimaryKey,
			KeyColumns:         []string{"id"},
			BatchTransactional: true,
			StorageEngine:      "InnoDB",
		},
		Columns: []driver.WriteColumn{
			{
				Detail: dto.ColumnDetail{
					Name:         "id",
					DataType:     "bigint",
					ColumnType:   "bigint",
					Nullable:     false,
					IsPrimaryKey: true,
					Insertable:   true,
					Updatable:    true,
					OrdinalPos:   1,
				},
				NativeType:      "bigint",
				Deterministic:   true,
				CompareStrategy: driver.CompareExact,
				KeyComparable:   true,
			},
			{
				Detail: dto.ColumnDetail{
					Name:       "name",
					DataType:   "varchar",
					ColumnType: "varchar(64)",
					Collation:  "utf8mb4_0900_ai_ci",
					Nullable:   true,
					Insertable: true,
					Updatable:  true,
					OrdinalPos: 2,
				},
				NativeType:      "varchar",
				Collation:       "utf8mb4_0900_ai_ci",
				Deterministic:   true,
				CompareStrategy: driver.CompareMySQLBinary,
				KeyComparable:   true,
			},
		},
	}
}

type fakeTableWriteTx struct {
	conn     *Conn
	behavior FaultBehavior
	schema   driver.WriteSchema
	stepIdx  int
	done     bool
	rolled   bool
}

func (t *fakeTableWriteTx) Schema() driver.WriteSchema { return t.schema }

func (t *fakeTableWriteTx) Exec(ctx context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	if err := ctx.Err(); err != nil {
		return driver.ExecFact{}, err
	}
	idx := t.stepIdx
	t.stepIdx++

	t.conn.ExecCount.Add(1)
	t.conn.opener.mu.Lock()
	t.conn.opener.ExecLog = append(t.conn.opener.ExecLog, stmt)
	t.conn.opener.mu.Unlock()

	if t.behavior.ReturnReadOnlyErr {
		return driver.ExecFact{}, dto.NewAppError(dto.ErrReadOnlyDenied, "fake read-only denied", "")
	}
	if t.behavior.ExecErrAtStep != nil {
		if err, ok := t.behavior.ExecErrAtStep[idx]; ok && err != nil {
			return driver.ExecFact{}, err
		}
	}
	affected := int64(1)
	if idx < len(t.behavior.ExecAffectedRows) {
		affected = t.behavior.ExecAffectedRows[idx]
	}
	return driver.ExecFact{AffectedRows: affected}, nil
}

func (t *fakeTableWriteTx) Rollback(_ context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	t.rolled = true
	return nil
}

func (t *fakeTableWriteTx) Commit(ctx context.Context) driver.CommitResult {
	if t.done {
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     dto.NewAppError(dto.ErrTxAborted, "事务已结束", ""),
		}
	}
	t.done = true
	if t.behavior.CommitResultOverride != nil {
		return *t.behavior.CommitResultOverride
	}
	if err := ctx.Err(); err != nil {
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeUnknown,
			Err: &dto.AppError{
				Code:      dto.ErrCommitOutcomeUnknown,
				Message:   "提交过程上下文中断，结果未知",
				Detail:    err.Error(),
				RetrySafe: false,
			},
		}
	}
	if t.behavior.DropOnCommitZeroBytes || t.behavior.DropOnCommitPartialBytes {
		detail := "EOF after COMMIT written (0 response bytes)"
		if t.behavior.DropOnCommitPartialBytes {
			detail = "unexpected EOF after partial COMMIT response bytes / TLS record header"
		}
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeUnknown,
			Err: &dto.AppError{
				Code:      dto.ErrCommitOutcomeUnknown,
				Message:   "提交请求已发出但未收到完整确认，请先核对目标表数据状态，切勿盲目重试",
				Detail:    detail,
				RetrySafe: false,
			},
		}
	}
	return driver.CommitResult{Outcome: driver.CommitOutcomeCommitted}
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
