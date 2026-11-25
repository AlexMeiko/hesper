package driver

import (
	"context"
	"net"
	"sync"
	"sync/atomic"

	"sql-manager/internal/dto"
)

// ResolvedConfig carries complete decrypted parameters needed by a driver Opener.
// Never serialized to frontend or stored in app.db.
type ResolvedConfig struct {
	ConnectionID string
	Engine       dto.EngineType
	Host         string
	Port         int
	Username     string
	Password     string
	Database     string
	FilePath     string
	ReadOnly     bool
	TLS          dto.TLSConfig
	TimeoutSec   int
	// DialContext allows injecting custom transports (SSH tunnel or deterministic fault-injection proxy in tests).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// BoundStatement is a parameterized SQL statement shared by execution and preview.
type BoundStatement struct {
	SQL  string
	Args []any
}

// ServerFact reports raw server identity facts obtained from the database after handshake.
type ServerFact struct {
	DetectedType  dto.EngineType
	ServerVersion string
	CurrentDB     string
	ServerConnID  string
}

// ExecFact reports raw execution outcomes from non-row-returning statements.
type ExecFact struct {
	AffectedRows int64
	LastInsertID int64
}

// BlobRegistrar allows drivers to offload binary or oversized cell values during streaming.
type BlobRegistrar func(contentType, filename string, data []byte) string

// RowStream streams query result rows incrementally without forcing full upfront buffering.
type RowStream interface {
	Columns() []dto.ColumnMeta
	Next(registerBlob BlobRegistrar) ([]dto.CellValue, bool, error)
	Close() error
}

// Conn is the minimal lifecycle interface held by internal/session for a single physical connection.
// Concrete driver connections implement additional small consumer interfaces as applicable.
type Conn interface {
	Ping(ctx context.Context) error
	Close() error
	ServerFact(ctx context.Context) (ServerFact, error)
	Capabilities(serverVersion string) dto.Capabilities
	Dialect() dto.EngineType
	// WriteSentWithoutResponse returns true if the last statement wrote bytes onto the wire
	// and encountered a connection error before receiving any response bytes from the server.
	WriteSentWithoutResponse() bool
	// ResetWireTracker resets the per-command wire tracking before issuing a new SQL command.
	ResetWireTracker()
	// ClassifyError maps a raw driver error into a structured AppError fact (e.g., syntax, canceled, read-only, tx aborted).
	ClassifyError(err error, inTx bool) *dto.AppError
}

// Opener opens a physical database connection for one engine type.
type Opener interface {
	Type() dto.EngineType
	Open(ctx context.Context, cfg ResolvedConfig) (Conn, error)
}

// QueryExecutor is consumed by QueryService / Session for executing statements.
type QueryExecutor interface {
	Query(ctx context.Context, stmt BoundStatement) (RowStream, error)
	Exec(ctx context.Context, stmt BoundStatement) (ExecFact, error)
}

// ExecutionCanceler is consumed by Session to cancel an in-flight statement via bypass/protocol control.
// confirmed == true means the cancel signal has been processed by the server and will NOT arrive late
// to disturb a subsequent statement on the same physical connection.
type ExecutionCanceler interface {
	CancelInFlight(ctx context.Context, controlConn Conn) (confirmed bool, err error)
}

// DatabaseSwitcher is implemented by drivers that can switch databases in-place on an existing connection (e.g., MySQL USE).
// Drivers with Capabilities.SwitchDBReconnects == true (e.g., PostgreSQL) do NOT implement this and instead open a new connection.
type DatabaseSwitcher interface {
	SwitchDatabase(ctx context.Context, db string) error
}

// SchemaSwitcher is an optional interface exposed by second-driver (PostgreSQL) verification
// for engines with a Database -> Schema -> Table hierarchy (e.g., SET search_path).
type SchemaSwitcher interface {
	SwitchSchema(ctx context.Context, schema string) error
}

// MetaInspector is consumed by SchemaService to inspect metadata and compute TableEditability.
type MetaInspector interface {
	ListDatabases(ctx context.Context) ([]dto.DatabaseInfo, error)
	ListSchemas(ctx context.Context, db string) ([]string, error)
	ListTables(ctx context.Context, db, schema string) ([]dto.TableSummary, error)
	GetTableDetail(ctx context.Context, db, schema, table string) (*dto.TableDetail, error)
	GetTableDDL(ctx context.Context, db, schema, table string) (string, error)
	InspectTableEditability(ctx context.Context, db, schema, table string, clientReadOnly bool) (dto.TableEditability, error)
	ListViews(ctx context.Context, db, schema string) ([]dto.ObjectSummary, error)
	FetchCompletionCatalog(ctx context.Context, db, schema string) (*dto.CompletionCatalog, error)
}

// TableBrowser is consumed by QueryService for live table pagination (separate from Execute-Once SQL snapshots).
type TableBrowser interface {
	BrowseTable(ctx context.Context, req dto.BrowseTableRequest, editability dto.TableEditability) (*dto.BrowseTableResponse, error)
}

// SequenceInspector is an optional capability interface implemented only by drivers supporting sequences (PG, MariaDB).
type SequenceInspector interface {
	ListSequences(ctx context.Context, db, schema string) ([]dto.ObjectSummary, error)
}

// --- Static Driver Registry ---

type Registry struct {
	mu      sync.RWMutex
	openers map[dto.EngineType]Opener
}

func NewRegistry() *Registry {
	return &Registry{openers: make(map[dto.EngineType]Opener)}
}

func (r *Registry) Register(o Opener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openers[o.Type()] = o
}

func (r *Registry) Get(engine dto.EngineType) (Opener, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.openers[engine]
	return o, ok
}

// --- Wire Tracker for Commit/Write Outcome-Unknown Detection ---

// TrackedConn wraps a net.Conn to track whether bytes were written to the socket
// for the current SQL command without yet receiving any response byte from the server.
type TrackedConn struct {
	net.Conn
	bytesWritten atomic.Int64
	bytesRead    atomic.Int64
}

func NewTrackedConn(c net.Conn) *TrackedConn {
	return &TrackedConn{Conn: c}
}

func (t *TrackedConn) Reset() {
	t.bytesWritten.Store(0)
	t.bytesRead.Store(0)
}

func (t *TrackedConn) Write(p []byte) (int, error) {
	n, err := t.Conn.Write(p)
	if n > 0 {
		t.bytesWritten.Add(int64(n))
	}
	return n, err
}

func (t *TrackedConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.bytesRead.Add(int64(n))
	}
	return n, err
}

// WriteSentWithoutResponse returns true if at least one byte was sent on the wire
// since Reset() and zero response bytes were received before the read failed.
func (t *TrackedConn) WriteSentWithoutResponse() bool {
	if t == nil {
		return false
	}
	return t.bytesWritten.Load() > 0 && t.bytesRead.Load() == 0
}
