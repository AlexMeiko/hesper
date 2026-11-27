package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
)

const (
	textTruncLimit = 64 * 1024
	binPreviewLen  = 64
)

// Opener opens physical PostgreSQL connections via pgx/v5.
type Opener struct{}

func NewOpener() *Opener { return &Opener{} }

func (o *Opener) Type() dto.EngineType { return dto.EnginePostgres }

func (o *Opener) Open(ctx context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	port := cfg.Port
	if port <= 0 {
		port = 5432
	}
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	dbName := cfg.Database
	if dbName == "" {
		dbName = "postgres"
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	pgCfg, err := pgx.ParseConfig("host=127.0.0.1 port=5432 sslmode=disable")
	if err != nil {
		return nil, err
	}
	pgCfg.Host = host
	pgCfg.Port = uint16(port)
	pgCfg.User = cfg.Username
	pgCfg.Password = cfg.Password
	pgCfg.Database = dbName
	pgCfg.ConnectTimeout = timeout
	pgCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	if pgCfg.RuntimeParams == nil {
		pgCfg.RuntimeParams = map[string]string{}
	}
	pgCfg.RuntimeParams["application_name"] = "sql-manager"
	if cfg.ReadOnly {
		pgCfg.RuntimeParams["default_transaction_read_only"] = "on"
	}

	if err := configureTLS(pgCfg, cfg.TLS, host, port); err != nil {
		return nil, err
	}

	if cfg.DialContext != nil {
		// Delegate hostname resolution to the remote SSH bastion / custom transport.
		pgCfg.LookupFunc = func(_ context.Context, h string) ([]string, error) {
			return []string{h}, nil
		}
	}

	var tracked atomic.Pointer[driver.TrackedConn]
	var connecting atomic.Bool
	connecting.Store(true)
	pgCfg.DialFunc = func(dctx context.Context, network, addr string) (net.Conn, error) {
		var raw net.Conn
		var derr error
		if cfg.DialContext != nil {
			raw, derr = cfg.DialContext(dctx, network, addr)
		} else {
			d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
			raw, derr = d.DialContext(dctx, network, addr)
		}
		if derr != nil {
			return nil, derr
		}
		tc := driver.NewTrackedConn(raw)
		if connecting.Load() {
			tracked.Store(tc)
		}
		return tc, nil
	}

	conn, err := pgx.ConnectConfig(ctx, pgCfg)
	connecting.Store(false)
	if err != nil {
		// Authentication failures can arrive before PostgreSQL reports client_encoding.
		return nil, classifyPGError(err, "", cfg.ErrorCharset)
	}

	var serverVer, currentDB, currentSchema string
	var pid int32
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version'), current_database(), pg_backend_pid(), COALESCE(current_schema(), '')").
		Scan(&serverVer, &currentDB, &pid, &currentSchema); err != nil {
		reported, fallback := pgErrorCharsets(conn, cfg.ErrorCharset)
		appErr := classifyPGError(err, reported, fallback)
		_ = conn.Close(ctx)
		return nil, appErr
	}

	connCtx, connCancel := context.WithCancel(context.Background())

	return &Conn{
		conn:          conn,
		tracked:       tracked.Load(),
		connCtx:       connCtx,
		connCancel:    connCancel,
		version:       serverVer,
		currentDB:     currentDB,
		currentSchema: currentSchema,
		backendPID:    pid,
		readOnly:      cfg.ReadOnly,
		errorCharset:  cfg.ErrorCharset,
	}, nil
}

func configureTLS(pgCfg *pgx.ConnConfig, tlsCfg dto.TLSConfig, host string, port int) error {
	// Always clear any environment- or default-derived fallback candidates.
	pgCfg.Fallbacks = nil
	tc, err := driver.BuildTLSConfig(tlsCfg, host)
	if err != nil {
		return err
	}
	mode := driver.NormalizeTLSMode(tlsCfg.Mode)
	pgCfg.TLSConfig = tc
	if mode == "prefer" {
		// Allow plaintext fallback ONLY to the exact same target host and port.
		pgCfg.Fallbacks = []*pgconn.FallbackConfig{
			{
				Host:      host,
				Port:      uint16(port),
				TLSConfig: nil,
			},
		}
	}
	return nil
}

// Conn represents one dedicated physical PostgreSQL connection.
// Notably:
//   - Does NOT implement driver.DatabaseSwitcher (PG switches DB by reconnecting to target database).
//   - Implements optional driver.SchemaSwitcher (SET search_path) and driver.SequenceInspector.
type Conn struct {
	mu            sync.Mutex
	conn          *pgx.Conn
	tracked       *driver.TrackedConn
	connCtx       context.Context
	connCancel    context.CancelFunc
	version       string
	currentDB     string
	currentSchema string
	backendPID    int32
	readOnly      bool
	errorCharset  string
	closed        bool
}

func (c *Conn) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.connCancel()
	conn := c.conn
	c.mu.Unlock()

	if conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return conn.Close(ctx)
	}
	return nil
}

func (c *Conn) ServerFact(_ context.Context) (driver.ServerFact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return driver.ServerFact{
		DetectedType:  dto.EnginePostgres,
		ServerVersion: c.version,
		CurrentDB:     c.currentDB,
		CurrentSchema: c.currentSchema,
		ServerConnID:  strconv.Itoa(int(c.backendPID)),
	}, nil
}

func (c *Conn) Dialect() dto.EngineType { return dto.EnginePostgres }

func (c *Conn) Capabilities(_ string) dto.Capabilities {
	return dto.Capabilities{
		HasDatabases:               true,
		HasSchemas:                 true,
		SwitchDBReconnects:         true,
		SupportsCancel:             true,
		SupportsViews:              true,
		SupportsMatViews:           true,
		SupportsRoutines:           true,
		SupportsTriggers:           true,
		SupportsSequences:          true,
		SupportsSavepoint:          true,
		SupportsExplain:            true,
		DDLTransactional:           true,
		SupportsSingleStmtAlterAll: true,
	}
}

func (c *Conn) ResetWireTracker() {
	if c.tracked != nil {
		c.tracked.Reset()
	}
}

func (c *Conn) WriteSentWithoutResponse() bool {
	if c.tracked == nil {
		return false
	}
	return c.tracked.WriteSentWithoutResponse()
}

func (c *Conn) ClassifyError(err error, _ bool) *dto.AppError {
	return c.classifyError(err)
}

// SwitchSchema implements optional driver.SchemaSwitcher for PG search_path.
func (c *Conn) SwitchSchema(ctx context.Context, schema string) error {
	if schema == "" {
		return nil
	}
	q := fmt.Sprintf("SET search_path TO %s, public", QuoteIdent(schema))
	_, err := c.conn.Exec(ctx, q)
	if err != nil {
		return c.classifyError(err)
	}
	c.mu.Lock()
	c.currentSchema = schema
	c.mu.Unlock()
	return nil
}

// Query executes a row-returning statement using pgx SimpleProtocol so RawValues() yields exact PG text format.
func (c *Conn) Query(_ context.Context, stmt driver.BoundStatement) (driver.RowStream, error) {
	c.ResetWireTracker()
	rows, err := c.conn.Query(c.connCtx, stmt.SQL, stmt.Args...)
	if err != nil {
		return nil, err
	}
	fds := rows.FieldDescriptions()
	cols := make([]dto.ColumnMeta, len(fds))
	for i, fd := range fds {
		typeName, kind := pgOIDTypeInfo(fd.DataTypeOID)
		cols[i] = dto.ColumnMeta{
			Name:         fd.Name,
			DatabaseType: typeName,
			Kind:         kind,
			Nullable:     true,
		}
	}
	return &pgRowStream{rows: rows, columns: cols, fds: fds}, nil
}

func (c *Conn) Exec(_ context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	c.ResetWireTracker()
	tag, err := c.conn.Exec(c.connCtx, stmt.SQL, stmt.Args...)
	if err != nil {
		return driver.ExecFact{}, err
	}
	return driver.ExecFact{AffectedRows: tag.RowsAffected()}, nil
}

// CancelInFlight sends PostgreSQL's wire CancelRequest packet.
// Because PG CancelRequest is asynchronous without a server ACK, confirmed returns false:
// if the running query exits with SQLSTATE 57014, TabSession knows PG consumed the cancel and keeps the session alive;
// if the running query finished naturally, TabSession closes the connection and marks SessionLost so late cancel cannot hit B.
func (c *Conn) CancelInFlight(ctx context.Context, _ driver.Conn) (bool, error) {
	if err := c.conn.PgConn().CancelRequest(ctx); err != nil {
		return false, err
	}
	return false, nil
}

// --- MetaInspector & optional SequenceInspector ---

func (c *Conn) ListDatabases(ctx context.Context) ([]dto.DatabaseInfo, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT d.datname, pg_encoding_to_char(d.encoding), d.datcollate
		FROM pg_database d
		WHERE d.datistemplate = false
		ORDER BY d.datname`)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	var out []dto.DatabaseInfo
	for rows.Next() {
		var name, enc, coll string
		if err := rows.Scan(&name, &enc, &coll); err != nil {
			return nil, err
		}
		out = append(out, dto.DatabaseInfo{
			Name:      name,
			Charset:   enc,
			Collation: coll,
			IsSystem:  name == "postgres",
		})
	}
	return out, c.rowsError(rows)
}

func (c *Conn) ListSchemas(ctx context.Context, _ string) ([]string, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT nspname FROM pg_namespace
		WHERE nspname NOT LIKE 'pg_%' AND nspname <> 'information_schema'
		ORDER BY nspname`)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, c.rowsError(rows)
}

func (c *Conn) ListTables(ctx context.Context, db, schema string) ([]dto.TableSummary, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := c.conn.Query(ctx, `
		SELECT c.relname, COALESCE(c.reltuples::bigint, 0), COALESCE(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		ORDER BY c.relname`, pgx.QueryExecModeExec, schema)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	var out []dto.TableSummary
	for rows.Next() {
		var t dto.TableSummary
		t.Database = db
		t.Schema = schema
		t.Engine = "heap"
		if err := rows.Scan(&t.Name, &t.EstimatedRows, &t.Comment); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, c.rowsError(rows)
}

func (c *Conn) ListViews(ctx context.Context, db, schema string) ([]dto.ObjectSummary, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := c.conn.Query(ctx, `
		SELECT c.relname, CASE WHEN c.relkind = 'm' THEN 'matview' ELSE 'view' END
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('v', 'm')
		ORDER BY c.relname`, pgx.QueryExecModeExec, schema)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	var out []dto.ObjectSummary
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return nil, err
		}
		out = append(out, dto.ObjectSummary{Database: db, Schema: schema, Name: name, Kind: kind})
	}
	return out, c.rowsError(rows)
}

// ListSequences implements optional driver.SequenceInspector (PostgreSQL supports sequences).
func (c *Conn) ListSequences(ctx context.Context, db, schema string) ([]dto.ObjectSummary, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := c.conn.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'S'
		ORDER BY c.relname`, pgx.QueryExecModeExec, schema)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	var out []dto.ObjectSummary
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, dto.ObjectSummary{Database: db, Schema: schema, Name: name, Kind: "sequence"})
	}
	return out, c.rowsError(rows)
}

func (c *Conn) GetTableDetail(ctx context.Context, db, schema, table string) (*dto.TableDetail, error) {
	if schema == "" {
		schema = "public"
	}
	detail := &dto.TableDetail{Database: db, Schema: schema, Name: table, Engine: "heap"}

	pkCols, err := c.loadPrimaryKeyColumns(ctx, schema, table)
	if err != nil {
		return nil, err
	}
	pkSet := map[string]bool{}
	for _, k := range pkCols {
		pkSet[k] = true
	}

	rows, err := c.conn.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
		       COALESCE(pg_get_expr(ad.adbin, ad.adrelid), ''), ad.adbin IS NOT NULL,
		       COALESCE(col_description(c.oid, a.attnum), ''), a.attnum
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, pgx.QueryExecModeExec, schema, table)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	for rows.Next() {
		var col dto.ColumnDetail
		if err := rows.Scan(&col.Name, &col.ColumnType, &col.Nullable, &col.DefaultValue, &col.HasDefault, &col.Comment, &col.OrdinalPos); err != nil {
			return nil, err
		}
		col.DataType = col.ColumnType
		col.IsPrimaryKey = pkSet[col.Name]
		detail.Columns = append(detail.Columns, col)
	}
	if err := c.rowsError(rows); err != nil {
		return nil, err
	}

	edit, err := c.InspectTableEditability(ctx, db, schema, table, c.readOnly)
	if err == nil {
		detail.Editability = edit
	}
	return detail, nil
}

func (c *Conn) GetTableDDL(ctx context.Context, db, schema, table string) (string, error) {
	detail, err := c.GetTableDetail(ctx, db, schema, table)
	if err != nil {
		return "", err
	}
	var lines []string
	var pks []string
	for _, col := range detail.Columns {
		line := fmt.Sprintf("  %s %s", QuoteIdent(col.Name), col.ColumnType)
		if !col.Nullable {
			line += " NOT NULL"
		}
		if col.HasDefault {
			line += " DEFAULT " + col.DefaultValue
		}
		lines = append(lines, line)
		if col.IsPrimaryKey {
			pks = append(pks, QuoteIdent(col.Name))
		}
	}
	if len(pks) > 0 {
		lines = append(lines, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(pks, ", ")))
	}
	return fmt.Sprintf("CREATE TABLE %s.%s (\n%s\n);", QuoteIdent(detail.Schema), QuoteIdent(detail.Name), strings.Join(lines, ",\n")), nil
}

func (c *Conn) InspectTableEditability(ctx context.Context, _, schema, table string, clientReadOnly bool) (dto.TableEditability, error) {
	if schema == "" {
		schema = "public"
	}
	edit := dto.TableEditability{
		CanWrite:           !clientReadOnly,
		RowIdentity:        dto.RowIdentityNone,
		BatchTransactional: true,
		StorageEngine:      "heap",
	}
	if clientReadOnly {
		edit.Reasons = append(edit.Reasons, "read_only_connection")
	}
	pkCols, err := c.loadPrimaryKeyColumns(ctx, schema, table)
	if err != nil {
		return edit, err
	}
	if len(pkCols) > 0 {
		edit.RowIdentity = dto.RowIdentityPrimaryKey
		edit.KeyColumns = pkCols
	} else {
		edit.Reasons = append(edit.Reasons, "no_stable_unique_key")
	}
	return edit, nil
}

func (c *Conn) FetchCompletionCatalog(ctx context.Context, db, schema string) (*dto.CompletionCatalog, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := c.conn.Query(ctx, `
		SELECT c.relname, a.attname
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'v', 'm') AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY c.relname, a.attnum`, pgx.QueryExecModeExec, schema)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	cat := &dto.CompletionCatalog{Database: db, Schema: schema, Tables: map[string][]string{}}
	for rows.Next() {
		var t, col string
		if err := rows.Scan(&t, &col); err != nil {
			return nil, err
		}
		cat.Tables[t] = append(cat.Tables[t], col)
	}
	return cat, c.rowsError(rows)
}

func (c *Conn) BrowseTable(ctx context.Context, req dto.BrowseTableRequest, edit dto.TableEditability) (*dto.BrowseTableResponse, error) {
	schema := req.Schema
	if schema == "" {
		schema = "public"
	}
	page := req.Page
	if page < 1 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 200
	}
	if pageSize > 5000 {
		pageSize = 5000
	}

	pkSet := map[string]bool{}
	for _, k := range edit.KeyColumns {
		pkSet[k] = true
	}

	var orderParts []string
	var orderCols []string
	seen := map[string]bool{}
	if req.SortColumn != "" {
		dir := "ASC"
		if strings.EqualFold(req.SortDir, "desc") {
			dir = "DESC"
		}
		orderParts = append(orderParts, QuoteIdent(req.SortColumn)+" "+dir)
		orderCols = append(orderCols, req.SortColumn)
		seen[req.SortColumn] = true
	}
	for _, k := range edit.KeyColumns {
		if !seen[k] {
			orderParts = append(orderParts, QuoteIdent(k)+" ASC")
			orderCols = append(orderCols, k)
			seen[k] = true
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT * FROM %s.%s", QuoteIdent(schema), QuoteIdent(req.Table))
	if filter := strings.TrimSpace(req.FilterWhere); filter != "" {
		fmt.Fprintf(&b, " WHERE (%s)", filter)
	}
	if len(orderParts) > 0 {
		fmt.Fprintf(&b, " ORDER BY %s", strings.Join(orderParts, ", "))
	}
	fmt.Fprintf(&b, " LIMIT %d OFFSET %d", pageSize+1, int64(page-1)*int64(pageSize))
	querySQL := b.String()

	start := time.Now()
	rows, err := c.conn.Query(ctx, querySQL)
	if err != nil {
		return nil, c.classifyError(err)
	}
	fds := rows.FieldDescriptions()
	cols := make([]dto.ColumnMeta, len(fds))
	for i, fd := range fds {
		typeName, kind := pgOIDTypeInfo(fd.DataTypeOID)
		cols[i] = dto.ColumnMeta{
			Name:         fd.Name,
			DatabaseType: typeName,
			Kind:         kind,
			Nullable:     true,
			IsPrimaryKey: pkSet[fd.Name],
		}
	}

	stream := &pgRowStream{rows: rows, columns: cols, fds: fds}
	resultRows := make([][]dto.CellValue, 0, pageSize)
	hasMore := false
	for {
		row, ok, err := stream.Next(nil)
		if err != nil {
			rows.Close()
			return nil, c.classifyError(err)
		}
		if !ok {
			break
		}
		if len(resultRows) == pageSize {
			hasMore = true
			break
		}
		resultRows = append(resultRows, row)
	}
	rows.Close()

	var totalRows int64
	totalKnown := false
	var cntSQL strings.Builder
	fmt.Fprintf(&cntSQL, "SELECT COUNT(*) FROM %s.%s", QuoteIdent(schema), QuoteIdent(req.Table))
	if filter := strings.TrimSpace(req.FilterWhere); filter != "" {
		fmt.Fprintf(&cntSQL, " WHERE (%s)", filter)
	}
	if err := c.conn.QueryRow(ctx, cntSQL.String()).Scan(&totalRows); err == nil {
		totalKnown = true
	}

	return &dto.BrowseTableResponse{
		Columns:         cols,
		Rows:            resultRows,
		Page:            page,
		PageSize:        pageSize,
		HasMore:         hasMore,
		TotalKnown:      totalKnown,
		TotalRows:       totalRows,
		DurationMs:      time.Since(start).Milliseconds(),
		ExecutedSQL:     querySQL,
		StableOrderUsed: len(edit.KeyColumns) > 0,
		OrderColumns:    orderCols,
		Editability:     edit,
	}, nil
}

func (c *Conn) loadPrimaryKeyColumns(ctx context.Context, schema, table string) ([]string, error) {
	rows, err := c.conn.Query(ctx, `
		SELECT a.attname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE n.nspname = $1 AND c.relname = $2 AND i.indisprimary
		ORDER BY k.ord`, pgx.QueryExecModeExec, schema, table)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	return cols, c.rowsError(rows)
}

// --- RowStream & PG OID Decoding ---

type pgRowStream struct {
	rows    pgx.Rows
	columns []dto.ColumnMeta
	fds     []pgconn.FieldDescription
}

func (s *pgRowStream) Columns() []dto.ColumnMeta { return s.columns }
func (s *pgRowStream) Close() error {
	if s.rows != nil {
		s.rows.Close()
	}
	return nil
}

func (s *pgRowStream) Next(registerBlob driver.BlobRegistrar) ([]dto.CellValue, bool, error) {
	if !s.rows.Next() {
		if err := s.rows.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	rawVals := s.rows.RawValues()
	cells := make([]dto.CellValue, len(s.columns))
	for i, raw := range rawVals {
		cells[i] = decodePGCell(s.columns[i], raw, registerBlob)
	}
	return cells, true, nil
}

func decodePGCell(col dto.ColumnMeta, raw []byte, registerBlob driver.BlobRegistrar) dto.CellValue {
	if raw == nil {
		return dto.CellValue{Kind: dto.KindNull}
	}
	if col.Kind == dto.KindBinary {
		// PG SimpleProtocol encodes BYTEA in hex format: \xDEADBEEF
		decoded := raw
		if len(raw) >= 2 && raw[0] == '\\' && raw[1] == 'x' {
			if b, err := hex.DecodeString(string(raw[2:])); err == nil {
				decoded = b
			}
		}
		previewN := len(decoded)
		truncated := false
		if previewN > binPreviewLen {
			previewN = binPreviewLen
			truncated = true
		}
		var streamRef string
		if registerBlob != nil && len(decoded) > 0 {
			streamRef = registerBlob("application/octet-stream", col.Name+".bin", decoded)
		}
		return dto.CellValue{
			Kind:       dto.KindBinary,
			Text:       "0x" + strings.ToUpper(hex.EncodeToString(decoded[:previewN])),
			Truncated:  truncated,
			ByteLength: int64(len(decoded)),
			StreamRef:  streamRef,
		}
	}
	if len(raw) > textTruncLimit {
		var streamRef string
		if registerBlob != nil {
			streamRef = registerBlob("text/plain; charset=utf-8", col.Name+".txt", raw)
		}
		return dto.CellValue{
			Kind:       col.Kind,
			Text:       truncateUTF8(raw, textTruncLimit),
			Truncated:  true,
			ByteLength: int64(len(raw)),
			StreamRef:  streamRef,
		}
	}
	return dto.CellValue{
		Kind: col.Kind,
		Text: string(raw),
	}
}

func pgOIDTypeInfo(oid uint32) (string, dto.ValueKind) {
	switch oid {
	case pgtype.BoolOID:
		return "BOOL", dto.KindBool
	case pgtype.Int2OID, pgtype.Int4OID:
		return "INTEGER", dto.KindInt
	case pgtype.Int8OID:
		return "BIGINT", dto.KindBigInt
	case pgtype.NumericOID:
		return "NUMERIC", dto.KindDecimal
	case pgtype.Float4OID, pgtype.Float8OID:
		return "DOUBLE", dto.KindFloat
	case pgtype.JSONOID, pgtype.JSONBOID:
		return "JSONB", dto.KindJSON
	case pgtype.ByteaOID:
		return "BYTEA", dto.KindBinary
	case pgtype.DateOID, pgtype.TimeOID, pgtype.TimestampOID, pgtype.TimestamptzOID:
		return "TIMESTAMPTZ", dto.KindDateTime
	case pgtype.UUIDOID:
		return "UUID", dto.KindString
	default:
		return "TEXT", dto.KindString
	}
}

func truncateUTF8(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	b = b[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func QuoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func pgErrorCharsets(conn *pgx.Conn, fallback string) (reported, fallbackCharset string) {
	if conn != nil {
		cs := conn.PgConn().ParameterStatus("client_encoding")
		if cs != "" && !strings.EqualFold(cs, "SQL_ASCII") {
			return cs, fallback
		}
	}
	return "", fallback
}

func (c *Conn) classifyError(err error) *dto.AppError {
	reported, fallback := pgErrorCharsets(c.conn, c.errorCharset)
	return classifyPGError(err, reported, fallback)
}

func (c *Conn) rowsError(rows pgx.Rows) error {
	err := rows.Err()
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return c.classifyError(err)
	}
	return err
}

func decodePGError(pgErr *pgconn.PgError, reportedCharset, fallbackCharset string) *pgconn.PgError {
	decoded := *pgErr
	decoded.Message = driver.DecodeErrorText(pgErr.Message, reportedCharset, fallbackCharset)
	decoded.Detail = driver.DecodeErrorText(pgErr.Detail, reportedCharset, fallbackCharset)
	decoded.Severity = driver.DecodeErrorText(pgErr.Severity, reportedCharset, fallbackCharset)
	return &decoded
}

func classifyPGError(err error, reportedCharset, fallbackCharset string) *dto.AppError {
	if err == nil {
		return nil
	}
	var ae *dto.AppError
	if errors.As(err, &ae) {
		return ae
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		pgErr = decodePGError(pgErr, reportedCharset, fallbackCharset)
		switch pgErr.Code {
		case "57014": // query_canceled
			return &dto.AppError{
				Code:     dto.ErrStatementCanceled,
				Message:  "当前语句已取消",
				Detail:   pgErr.Message,
				SQLState: pgErr.Code,
			}
		case "25P02": // in_failed_sql_transaction
			return &dto.AppError{
				Code:     dto.ErrTxAborted,
				Message:  "当前事务内前序语句出错，事务已中止，请先执行 ROLLBACK",
				Detail:   pgErr.Message,
				SQLState: pgErr.Code,
			}
		case "25006", "42501": // read_only_sql_transaction / insufficient_privilege
			return &dto.AppError{
				Code:     dto.ErrReadOnlyDenied,
				Message:  "数据库拒绝写入操作 (只读权限或只读事务限制)",
				Detail:   pgErr.Message,
				SQLState: pgErr.Code,
			}
		case "28P01", "28000":
			return &dto.AppError{
				Code:     dto.ErrAuthFailed,
				Message:  "PostgreSQL 认证失败",
				Detail:   pgErr.Message,
				SQLState: pgErr.Code,
			}
		case "42601":
			return &dto.AppError{
				Code:     dto.ErrSQLSyntax,
				Message:  "SQL 语法错误",
				Detail:   pgErr.Message,
				SQLState: pgErr.Code,
				Position: int(pgErr.Position),
			}
		default:
			return &dto.AppError{
				Code:     dto.ErrInternal,
				Message:  fmt.Sprintf("PostgreSQL 错误 (%s): %s", pgErr.Code, pgErr.Message),
				Detail:   pgErr.Detail,
				SQLState: pgErr.Code,
			}
		}
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "eof") || strings.Contains(lower, "closed") || strings.Contains(lower, "reset by peer") || strings.Contains(lower, "broken pipe") {
		return &dto.AppError{
			Code:    dto.ErrSessionLost,
			Message: "PostgreSQL 连接已断开，原会话已结束",
			Detail:  err.Error(),
		}
	}
	return &dto.AppError{
		Code:    dto.ErrConnectionFailed,
		Message: "PostgreSQL 操作失败",
		Detail:  err.Error(),
	}
}
