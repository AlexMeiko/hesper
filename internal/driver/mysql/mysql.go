package mysql

import (
	"context"
	"database/sql"
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

	mysqldriver "github.com/go-sql-driver/mysql"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
)

const (
	trackedNetName = "sqlm-mysql-tracked"
	textTruncLimit = 64 * 1024
	binPreviewLen  = 64
)

type dialSlot struct {
	realAddr    string
	customDial  func(ctx context.Context, network, addr string) (net.Conn, error)
	trackedConn *driver.TrackedConn
}

var (
	registerDialOnce sync.Once
	dialSeq          atomic.Uint64
	dialSlots        sync.Map // token -> *dialSlot
)

func ensureTrackedDialerRegistered() {
	registerDialOnce.Do(func() {
		mysqldriver.RegisterDialContext(trackedNetName, func(ctx context.Context, token string) (net.Conn, error) {
			raw, ok := dialSlots.Load(token)
			if !ok {
				return nil, fmt.Errorf("mysql tracked dial slot missing for %s", token)
			}
			slot := raw.(*dialSlot)
			dialFn := slot.customDial
			if dialFn == nil {
				d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
				dialFn = d.DialContext
			}
			c, err := dialFn(ctx, "tcp", slot.realAddr)
			if err != nil {
				return nil, err
			}
			tc := driver.NewTrackedConn(c)
			slot.trackedConn = tc
			return tc, nil
		})
	})
}

// Opener opens physical MySQL / MariaDB connections using go-sql-driver/mysql.
type Opener struct {
	engine dto.EngineType
}

// NewOpener creates an Opener for EngineMySQL (or EngineMariaDB reusing MySQL protocol).
func NewOpener(engine dto.EngineType) *Opener {
	if engine == "" {
		engine = dto.EngineMySQL
	}
	return &Opener{engine: engine}
}

func (o *Opener) Type() dto.EngineType { return o.engine }

func (o *Opener) Open(ctx context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	ensureTrackedDialerRegistered()

	port := cfg.Port
	if port <= 0 {
		port = 3306
	}
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	token := fmt.Sprintf("m-%d", dialSeq.Add(1))
	slot := &dialSlot{
		realAddr:   net.JoinHostPort(host, strconv.Itoa(port)),
		customDial: cfg.DialContext,
	}
	dialSlots.Store(token, slot)
	defer dialSlots.Delete(token)

	mc := mysqldriver.NewConfig()
	mc.User = cfg.Username
	mc.Passwd = cfg.Password
	mc.Net = trackedNetName
	mc.Addr = token
	mc.DBName = cfg.Database
	mc.Timeout = timeout
	mc.ParseTime = false
	mc.ClientFoundRows = true
	mc.MultiStatements = false
	mc.AllowNativePasswords = true
	mc.CheckConnLiveness = true

	if err := configureTLS(mc, cfg.TLS, host); err != nil {
		return nil, err
	}

	connector, err := mysqldriver.NewConnector(mc)
	if err != nil {
		return nil, fmt.Errorf("build mysql connector: %w", err)
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Acquire the single dedicated physical connection immediately.
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, classifyMySQLError(err, false)
	}

	// Capture server identity and CONNECTION_ID() for bypass KILL QUERY.
	var versionStr, versionComment string
	var serverConnID uint64
	var currentDB sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT VERSION(), @@version_comment, CONNECTION_ID(), DATABASE()").
		Scan(&versionStr, &versionComment, &serverConnID, &currentDB); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, classifyMySQLError(err, false)
	}

	detected := dto.EngineMySQL
	if strings.Contains(strings.ToLower(versionStr), "mariadb") ||
		strings.Contains(strings.ToLower(versionComment), "mariadb") {
		detected = dto.EngineMariaDB
	}

	if cfg.ReadOnly {
		// Client read-only session guardrail (database privileges remain authoritative for strong read-only).
		if _, err := conn.ExecContext(ctx, "SET SESSION transaction_read_only = 1"); err != nil {
			// Fallback for older MySQL 5.7 builds using tx_read_only.
			_, _ = conn.ExecContext(ctx, "SET SESSION tx_read_only = 1")
		}
	}

	connCtx, connCancel := context.WithCancel(context.Background())

	return &Conn{
		db:           db,
		conn:         conn,
		tracked:      slot.trackedConn,
		connCtx:      connCtx,
		connCancel:   connCancel,
		detectedType: detected,
		version:      versionStr,
		serverConnID: serverConnID,
		currentDB:    currentDB.String,
		readOnly:     cfg.ReadOnly,
	}, nil
}

func configureTLS(mc *mysqldriver.Config, tlsCfg dto.TLSConfig, host string) error {
	tc, err := driver.BuildTLSConfig(tlsCfg, host)
	if err != nil {
		return err
	}
	mode := driver.NormalizeTLSMode(tlsCfg.Mode)
	mc.TLSConfig = ""
	mc.TLS = tc
	mc.AllowFallbackToPlaintext = (mode == "prefer")
	return nil
}

// Conn represents one dedicated physical MySQL / MariaDB connection.
// Implements driver.Conn, QueryExecutor, ExecutionCanceler, DatabaseSwitcher, MetaInspector, TableBrowser.
type Conn struct {
	mu           sync.Mutex
	db           *sql.DB
	conn         *sql.Conn
	tracked      *driver.TrackedConn
	connCtx      context.Context
	connCancel   context.CancelFunc
	detectedType dto.EngineType
	version      string
	serverConnID uint64
	currentDB    string
	readOnly     bool
	closed       bool
}

func (c *Conn) Ping(ctx context.Context) error {
	return c.conn.PingContext(ctx)
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
	db := c.db
	c.mu.Unlock()

	var err1, err2 error
	if conn != nil {
		err1 = conn.Close()
	}
	if db != nil {
		err2 = db.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}

func (c *Conn) ServerFact(_ context.Context) (driver.ServerFact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return driver.ServerFact{
		DetectedType:  c.detectedType,
		ServerVersion: c.version,
		CurrentDB:     c.currentDB,
		ServerConnID:  strconv.FormatUint(c.serverConnID, 10),
	}, nil
}

func (c *Conn) Dialect() dto.EngineType { return c.detectedType }

func (c *Conn) Capabilities(serverVersion string) dto.Capabilities {
	isMaria := c.detectedType == dto.EngineMariaDB
	majorVer := parseMajorVersion(serverVersion)
	isMySQL8Plus := !isMaria && majorVer >= 8
	return dto.Capabilities{
		HasDatabases:               true,
		HasSchemas:                 false,
		SwitchDBReconnects:         false,
		SupportsCancel:             true,
		SupportsViews:              true,
		SupportsMatViews:           false,
		SupportsRoutines:           true,
		SupportsTriggers:           true,
		SupportsSequences:          isMaria,
		SupportsSavepoint:          true,
		SupportsExplain:            true,
		DDLTransactional:           false,
		SupportsSingleStmtAlterAll: isMySQL8Plus,
	}
}

func parseMajorVersion(ver string) int {
	dot := strings.IndexByte(ver, '.')
	if dot <= 0 {
		return 0
	}
	n, err := strconv.Atoi(ver[:dot])
	if err != nil {
		return 0
	}
	return n
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

func (c *Conn) ClassifyError(err error, inTx bool) *dto.AppError {
	return classifyMySQLError(err, inTx)
}

// Query executes a row-returning statement using the connection-lifetime context so that
// graceful cancel goes via bypass KILL QUERY without closing the TCP socket (preserving tx & temp tables).
func (c *Conn) Query(_ context.Context, stmt driver.BoundStatement) (driver.RowStream, error) {
	c.ResetWireTracker()
	rows, err := c.conn.QueryContext(c.connCtx, stmt.SQL, stmt.Args...)
	if err != nil {
		return nil, err
	}
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	cols := make([]dto.ColumnMeta, len(colTypes))
	for i, ct := range colTypes {
		nullable, _ := ct.Nullable()
		dbType := strings.ToUpper(ct.DatabaseTypeName())
		cols[i] = dto.ColumnMeta{
			Name:         ct.Name(),
			DatabaseType: dbType,
			Kind:         classifyMySQLColumnKind(dbType),
			Nullable:     nullable,
		}
	}
	return &mysqlRowStream{
		rows:     rows,
		columns:  cols,
		colTypes: colTypes,
	}, nil
}

// Exec executes a non-row-returning statement using the connection-lifetime context.
func (c *Conn) Exec(_ context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	c.ResetWireTracker()
	res, err := c.conn.ExecContext(c.connCtx, stmt.SQL, stmt.Args...)
	if err != nil {
		return driver.ExecFact{}, err
	}
	aff, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	return driver.ExecFact{AffectedRows: aff, LastInsertID: lastID}, nil
}

// CancelInFlight sends KILL QUERY <serverConnID> on the separate bypass control connection.
// Because KILL QUERY is synchronous on the control connection, once MySQL returns OK,
// the server has already processed the kill signal — so it cannot arrive late to kill a subsequent queued query.
func (c *Conn) CancelInFlight(ctx context.Context, controlConn driver.Conn) (bool, error) {
	c.mu.Lock()
	id := c.serverConnID
	c.mu.Unlock()
	if id == 0 {
		return false, errors.New("mysql server connection id unavailable")
	}
	ctrl, ok := controlConn.(*Conn)
	if !ok || ctrl == nil {
		return false, errors.New("mysql bypass control connection unavailable")
	}
	killSQL := fmt.Sprintf("KILL QUERY %d", id)
	_, err := ctrl.conn.ExecContext(ctx, killSQL)
	if err != nil {
		var me *mysqldriver.MySQLError
		if errors.As(err, &me) && me.Number == 1094 {
			// Error 1094: Unknown thread id — original connection already closed on server.
			return false, err
		}
		return false, err
	}
	return true, nil
}

// SwitchDatabase switches the active database in-place via USE `db`.
func (c *Conn) SwitchDatabase(ctx context.Context, db string) error {
	if db == "" {
		return nil
	}
	if _, err := c.conn.ExecContext(ctx, "USE "+QuoteIdent(db)); err != nil {
		return classifyMySQLError(err, false)
	}
	c.mu.Lock()
	c.currentDB = db
	c.mu.Unlock()
	return nil
}

// --- MetaInspector implementation ---

func (c *Conn) ListDatabases(ctx context.Context) ([]dto.DatabaseInfo, error) {
	rows, err := c.conn.QueryContext(ctx, `
		SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME
		FROM information_schema.SCHEMATA
		ORDER BY SCHEMA_NAME`)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer rows.Close()

	sysDBs := map[string]bool{
		"information_schema": true,
		"mysql":              true,
		"performance_schema": true,
		"sys":                true,
	}
	var out []dto.DatabaseInfo
	for rows.Next() {
		var name, charset, collation string
		if err := rows.Scan(&name, &charset, &collation); err != nil {
			return nil, err
		}
		out = append(out, dto.DatabaseInfo{
			Name:      name,
			Charset:   charset,
			Collation: collation,
			IsSystem:  sysDBs[strings.ToLower(name)],
		})
	}
	return out, rows.Err()
}

func (c *Conn) ListSchemas(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (c *Conn) ListTables(ctx context.Context, db, _ string) ([]dto.TableSummary, error) {
	rows, err := c.conn.QueryContext(ctx, `
		SELECT TABLE_NAME, COALESCE(ENGINE, ''), COALESCE(TABLE_ROWS, 0), COALESCE(TABLE_COMMENT, '')
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`, db)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer rows.Close()

	var out []dto.TableSummary
	for rows.Next() {
		var t dto.TableSummary
		t.Database = db
		if err := rows.Scan(&t.Name, &t.Engine, &t.EstimatedRows, &t.Comment); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (c *Conn) ListViews(ctx context.Context, db, _ string) ([]dto.ObjectSummary, error) {
	rows, err := c.conn.QueryContext(ctx, `
		SELECT TABLE_NAME
		FROM information_schema.VIEWS
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME`, db)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer rows.Close()

	var out []dto.ObjectSummary
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, dto.ObjectSummary{
			Database: db,
			Name:     name,
			Kind:     "view",
		})
	}
	return out, rows.Err()
}

// ListSequences implements driver.SequenceInspector for MariaDB (where sequences are TABLE_TYPE = 'SEQUENCE').
// MySQL does not support native sequences and returns nil cleanly.
func (c *Conn) ListSequences(ctx context.Context, db, _ string) ([]dto.ObjectSummary, error) {
	if c.detectedType != dto.EngineMariaDB {
		return nil, nil
	}
	rows, err := c.conn.QueryContext(ctx, `
		SELECT TABLE_NAME, COALESCE(TABLE_COMMENT, '')
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'SEQUENCE'
		ORDER BY TABLE_NAME`, db)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer rows.Close()

	var out []dto.ObjectSummary
	for rows.Next() {
		var name, comment string
		if err := rows.Scan(&name, &comment); err != nil {
			return nil, err
		}
		out = append(out, dto.ObjectSummary{
			Database: db,
			Name:     name,
			Kind:     "sequence",
			Comment:  comment,
		})
	}
	return out, rows.Err()
}

func (c *Conn) GetTableDetail(ctx context.Context, db, _, table string) (*dto.TableDetail, error) {
	detail := &dto.TableDetail{
		Database: db,
		Name:     table,
	}

	// Table metadata (engine & comment)
	_ = c.conn.QueryRowContext(ctx, `
		SELECT COALESCE(ENGINE, ''), COALESCE(TABLE_COMMENT, '')
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, db, table).Scan(&detail.Engine, &detail.Comment)

	// Columns
	colRows, err := c.conn.QueryContext(ctx, `
		SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_KEY,
		       COLUMN_DEFAULT, EXTRA, COALESCE(COLUMN_COMMENT, ''), ORDINAL_POSITION
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, db, table)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer colRows.Close()

	for colRows.Next() {
		var col dto.ColumnDetail
		var isNullable, colKey, extra string
		var colDef sql.NullString
		if err := colRows.Scan(
			&col.Name, &col.DataType, &col.ColumnType, &isNullable, &colKey,
			&colDef, &extra, &col.Comment, &col.OrdinalPos,
		); err != nil {
			return nil, err
		}
		col.Nullable = strings.EqualFold(isNullable, "YES")
		col.IsPrimaryKey = strings.EqualFold(colKey, "PRI")
		col.HasDefault = colDef.Valid
		if colDef.Valid {
			col.DefaultValue = colDef.String
		}
		col.AutoIncrement = strings.Contains(strings.ToLower(extra), "auto_increment")
		detail.Columns = append(detail.Columns, col)
	}
	if err := colRows.Err(); err != nil {
		return nil, err
	}

	// Indexes
	idxRows, err := c.conn.QueryContext(ctx, `
		SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE, COALESCE(INDEX_TYPE, '')
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`, db, table)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer idxRows.Close()

	idxMap := map[string]*dto.IndexDetail{}
	var idxOrder []string
	for idxRows.Next() {
		var name, colName, idxType string
		var nonUnique int
		if err := idxRows.Scan(&name, &colName, &nonUnique, &idxType); err != nil {
			return nil, err
		}
		idx, exists := idxMap[name]
		if !exists {
			idx = &dto.IndexDetail{
				Name:      name,
				Unique:    nonUnique == 0,
				Primary:   strings.EqualFold(name, "PRIMARY"),
				IndexType: idxType,
			}
			idxMap[name] = idx
			idxOrder = append(idxOrder, name)
		}
		idx.Columns = append(idx.Columns, colName)
	}
	for _, name := range idxOrder {
		detail.Indexes = append(detail.Indexes, *idxMap[name])
	}

	edit, err := c.InspectTableEditability(ctx, db, "", table, c.readOnly)
	if err == nil {
		detail.Editability = edit
	}
	return detail, nil
}

func (c *Conn) GetTableDDL(ctx context.Context, db, _, table string) (string, error) {
	q := fmt.Sprintf("SHOW CREATE TABLE %s.%s", QuoteIdent(db), QuoteIdent(table))
	var tblName, ddl string
	if err := c.conn.QueryRowContext(ctx, q).Scan(&tblName, &ddl); err != nil {
		return "", classifyMySQLError(err, false)
	}
	return ddl, nil
}

// InspectTableEditability computes 3-dimension editability (CanWrite, RowIdentity, BatchTransactional)
// combining clientReadOnly flag, explicit primary/unique-not-null keys, and storage engine (InnoDB vs MyISAM).
func (c *Conn) InspectTableEditability(ctx context.Context, db, _, table string, clientReadOnly bool) (dto.TableEditability, error) {
	edit := dto.TableEditability{
		CanWrite:    !clientReadOnly,
		RowIdentity: dto.RowIdentityNone,
	}
	if clientReadOnly {
		edit.Reasons = append(edit.Reasons, "read_only_connection")
	}

	var engine string
	if err := c.conn.QueryRowContext(ctx, `
		SELECT COALESCE(ENGINE, '') FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, db, table).Scan(&engine); err == nil {
		edit.StorageEngine = engine
		edit.BatchTransactional = strings.EqualFold(engine, "InnoDB")
		if engine != "" && !edit.BatchTransactional {
			edit.Reasons = append(edit.Reasons, "non_transactional_engine:"+engine)
		}
	}

	// Load column nullability map
	nullableMap := map[string]bool{}
	colRows, err := c.conn.QueryContext(ctx, `
		SELECT COLUMN_NAME, IS_NULLABLE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, db, table)
	if err == nil {
		for colRows.Next() {
			var cn, nul string
			if colRows.Scan(&cn, &nul) == nil {
				nullableMap[cn] = strings.EqualFold(nul, "YES")
			}
		}
		_ = colRows.Close()
	}

	// Inspect PRIMARY and UNIQUE indexes
	idxRows, err := c.conn.QueryContext(ctx, `
		SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND NON_UNIQUE = 0
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`, db, table)
	if err != nil {
		return edit, classifyMySQLError(err, false)
	}
	defer idxRows.Close()

	uniqueIdx := map[string][]string{}
	var idxNames []string
	for idxRows.Next() {
		var idxName, colName string
		var nonUnique int
		if err := idxRows.Scan(&idxName, &colName, &nonUnique); err != nil {
			return edit, err
		}
		if _, exists := uniqueIdx[idxName]; !exists {
			idxNames = append(idxNames, idxName)
		}
		uniqueIdx[idxName] = append(uniqueIdx[idxName], colName)
	}

	if pkCols, ok := uniqueIdx["PRIMARY"]; ok && len(pkCols) > 0 {
		edit.RowIdentity = dto.RowIdentityPrimaryKey
		edit.KeyColumns = pkCols
	} else {
		for _, idxName := range idxNames {
			cols := uniqueIdx[idxName]
			allNotNull := len(cols) > 0
			for _, cn := range cols {
				if nullableMap[cn] {
					allNotNull = false
					break
				}
			}
			if allNotNull {
				edit.RowIdentity = dto.RowIdentityUniqueNotNull
				edit.KeyColumns = cols
				break
			}
		}
	}
	if edit.RowIdentity == dto.RowIdentityNone {
		edit.Reasons = append(edit.Reasons, "no_stable_unique_key")
	}
	return edit, nil
}

func (c *Conn) FetchCompletionCatalog(ctx context.Context, db, _ string) (*dto.CompletionCatalog, error) {
	rows, err := c.conn.QueryContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, db)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer rows.Close()

	cat := &dto.CompletionCatalog{
		Database: db,
		Tables:   make(map[string][]string),
	}
	for rows.Next() {
		var t, col string
		if err := rows.Scan(&t, &col); err != nil {
			return nil, err
		}
		cat.Tables[t] = append(cat.Tables[t], col)
	}
	return cat, rows.Err()
}

// --- TableBrowser (live table pagination, completely separate from Execute-Once SQL snapshots) ---

func (c *Conn) BrowseTable(ctx context.Context, req dto.BrowseTableRequest, edit dto.TableEditability) (*dto.BrowseTableResponse, error) {
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

	// Validate columns for safe ORDER BY and primary key marking.
	validCols := map[string]bool{}
	pkSet := map[string]bool{}
	for _, k := range edit.KeyColumns {
		pkSet[k] = true
	}
	colRows, err := c.conn.QueryContext(ctx, `
		SELECT COLUMN_NAME FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, req.Database, req.Table)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	for colRows.Next() {
		var cn string
		if colRows.Scan(&cn) == nil {
			validCols[cn] = true
		}
	}
	_ = colRows.Close()

	var orderParts []string
	var orderCols []string
	seenOrder := map[string]bool{}
	if req.SortColumn != "" && validCols[req.SortColumn] {
		dir := "ASC"
		if strings.EqualFold(req.SortDir, "desc") {
			dir = "DESC"
		}
		orderParts = append(orderParts, QuoteIdent(req.SortColumn)+" "+dir)
		orderCols = append(orderCols, req.SortColumn)
		seenOrder[req.SortColumn] = true
	}
	for _, k := range edit.KeyColumns {
		if validCols[k] && !seenOrder[k] {
			orderParts = append(orderParts, QuoteIdent(k)+" ASC")
			orderCols = append(orderCols, k)
			seenOrder[k] = true
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT * FROM %s.%s", QuoteIdent(req.Database), QuoteIdent(req.Table))
	if filter := strings.TrimSpace(req.FilterWhere); filter != "" {
		fmt.Fprintf(&b, " WHERE (%s)", filter)
	}
	if len(orderParts) > 0 {
		fmt.Fprintf(&b, " ORDER BY %s", strings.Join(orderParts, ", "))
	}
	offset := int64(page-1) * int64(pageSize)
	fmt.Fprintf(&b, " LIMIT %d OFFSET %d", pageSize+1, offset)
	querySQL := b.String()

	start := time.Now()
	rows, err := c.conn.QueryContext(ctx, querySQL)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}
	defer rows.Close()

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	cols := make([]dto.ColumnMeta, len(colTypes))
	for i, ct := range colTypes {
		nullable, _ := ct.Nullable()
		dbType := strings.ToUpper(ct.DatabaseTypeName())
		cols[i] = dto.ColumnMeta{
			Name:         ct.Name(),
			DatabaseType: dbType,
			Kind:         classifyMySQLColumnKind(dbType),
			Nullable:     nullable,
			IsPrimaryKey: pkSet[ct.Name()],
		}
	}

	stream := &mysqlRowStream{rows: rows, columns: cols, colTypes: colTypes}
	resultRows := make([][]dto.CellValue, 0, pageSize)
	hasMore := false
	for {
		row, ok, err := stream.Next(nil)
		if err != nil {
			_ = rows.Close()
			return nil, classifyMySQLError(err, false)
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
	// MUST close rows before issuing COUNT(*) on the same single physical *sql.Conn!
	_ = rows.Close()

	// Best-effort fast total count (exact when small or filtered).
	var totalRows int64
	totalKnown := false
	var countSQL strings.Builder
	fmt.Fprintf(&countSQL, "SELECT COUNT(*) FROM %s.%s", QuoteIdent(req.Database), QuoteIdent(req.Table))
	if filter := strings.TrimSpace(req.FilterWhere); filter != "" {
		fmt.Fprintf(&countSQL, " WHERE (%s)", filter)
	}
	if err := c.conn.QueryRowContext(ctx, countSQL.String()).Scan(&totalRows); err == nil {
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

// --- RowStream & Exact Type Fidelity Decoding ---

type mysqlRowStream struct {
	rows     *sql.Rows
	columns  []dto.ColumnMeta
	colTypes []*sql.ColumnType
}

func (s *mysqlRowStream) Columns() []dto.ColumnMeta { return s.columns }

func (s *mysqlRowStream) Close() error {
	if s.rows == nil {
		return nil
	}
	return s.rows.Close()
}

func (s *mysqlRowStream) Next(registerBlob driver.BlobRegistrar) ([]dto.CellValue, bool, error) {
	if !s.rows.Next() {
		if err := s.rows.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}

	n := len(s.columns)
	rawVals := make([]sql.RawBytes, n)
	scanArgs := make([]any, n)
	for i := range rawVals {
		scanArgs[i] = &rawVals[i]
	}
	if err := s.rows.Scan(scanArgs...); err != nil {
		return nil, false, err
	}

	cells := make([]dto.CellValue, n)
	for i, raw := range rawVals {
		cells[i] = decodeMySQLCell(s.columns[i], raw, registerBlob)
	}
	return cells, true, nil
}

func decodeMySQLCell(col dto.ColumnMeta, raw sql.RawBytes, registerBlob driver.BlobRegistrar) dto.CellValue {
	// Strict NULL vs empty string ("") distinction:
	// In database/sql, scanning SQL NULL into sql.RawBytes sets raw == nil,
	// whereas scanning SQL '' sets raw != nil with len(raw) == 0.
	if raw == nil {
		return dto.CellValue{Kind: dto.KindNull}
	}

	switch col.Kind {
	case dto.KindBinary:
		previewN := len(raw)
		truncated := false
		if previewN > binPreviewLen {
			previewN = binPreviewLen
			truncated = true
		}
		var streamRef string
		if registerBlob != nil && len(raw) > 0 {
			streamRef = registerBlob("application/octet-stream", col.Name+".bin", raw)
		}
		return dto.CellValue{
			Kind:       dto.KindBinary,
			Text:       "0x" + strings.ToUpper(hex.EncodeToString(raw[:previewN])),
			Truncated:  truncated,
			ByteLength: int64(len(raw)),
			StreamRef:  streamRef,
		}
	case dto.KindBigInt, dto.KindDecimal, dto.KindInt, dto.KindFloat, dto.KindDateTime, dto.KindBool:
		// Always exact text representation straight from MySQL wire bytes (zero float64 / JS Number precision loss).
		return dto.CellValue{
			Kind: col.Kind,
			Text: string(raw),
		}
	case dto.KindJSON:
		if len(raw) > textTruncLimit {
			var streamRef string
			if registerBlob != nil {
				streamRef = registerBlob("application/json", col.Name+".json", raw)
			}
			return dto.CellValue{
				Kind:       dto.KindJSON,
				Text:       truncateUTF8(raw, textTruncLimit),
				Truncated:  true,
				ByteLength: int64(len(raw)),
				StreamRef:  streamRef,
			}
		}
		return dto.CellValue{
			Kind: dto.KindJSON,
			Text: string(raw),
		}
	default:
		if len(raw) > textTruncLimit {
			var streamRef string
			if registerBlob != nil {
				streamRef = registerBlob("text/plain; charset=utf-8", col.Name+".txt", raw)
			}
			return dto.CellValue{
				Kind:       dto.KindString,
				Text:       truncateUTF8(raw, textTruncLimit),
				Truncated:  true,
				ByteLength: int64(len(raw)),
				StreamRef:  streamRef,
			}
		}
		return dto.CellValue{
			Kind: dto.KindString,
			Text: string(raw),
		}
	}
}

func classifyMySQLColumnKind(dbType string) dto.ValueKind {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	switch {
	case strings.Contains(t, "BIGINT"):
		return dto.KindBigInt
	case strings.Contains(t, "DECIMAL") || strings.Contains(t, "NUMERIC"):
		return dto.KindDecimal
	case strings.Contains(t, "FLOAT") || strings.Contains(t, "DOUBLE"):
		return dto.KindFloat
	case strings.Contains(t, "INT"):
		return dto.KindInt
	case strings.Contains(t, "JSON"):
		return dto.KindJSON
	case strings.Contains(t, "BLOB") || strings.Contains(t, "BINARY") || t == "BIT" || t == "GEOMETRY":
		return dto.KindBinary
	case strings.Contains(t, "DATE") || strings.Contains(t, "TIME") || t == "YEAR":
		return dto.KindDateTime
	default:
		return dto.KindString
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
	return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
}

func classifyMySQLError(err error, _ bool) *dto.AppError {
	if err == nil {
		return nil
	}
	var appErr *dto.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1317, 1028, 1969, 3024:
			return &dto.AppError{
				Code:     dto.ErrStatementCanceled,
				Message:  "当前语句已取消，原会话保持存活",
				Detail:   me.Message,
				SQLState: string(me.SQLState[:]),
			}
		case 1045, 1044:
			return &dto.AppError{
				Code:     dto.ErrAuthFailed,
				Message:  "数据库认证或访问权限失败",
				Detail:   me.Message,
				SQLState: string(me.SQLState[:]),
			}
		case 1142, 1143, 1227, 1290, 1792:
			return &dto.AppError{
				Code:     dto.ErrReadOnlyDenied,
				Message:  "数据库拒绝写入操作 (只读权限或只读事务限制)",
				Detail:   me.Message,
				SQLState: string(me.SQLState[:]),
			}
		case 1064:
			return &dto.AppError{
				Code:     dto.ErrSQLSyntax,
				Message:  "SQL 语法错误",
				Detail:   me.Message,
				SQLState: string(me.SQLState[:]),
			}
		default:
			return &dto.AppError{
				Code:     dto.ErrInternal,
				Message:  fmt.Sprintf("数据库错误 (%d): %s", me.Number, me.Message),
				Detail:   me.Message,
				SQLState: string(me.SQLState[:]),
			}
		}
	}
	lower := strings.ToLower(err.Error())
	if errors.Is(err, mysqldriver.ErrInvalidConn) || strings.Contains(lower, "bad connection") || strings.Contains(lower, "eof") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe") {
		return &dto.AppError{
			Code:    dto.ErrSessionLost,
			Message: "数据库连接已断开，原会话已结束 (临时表与未提交事务已失效)",
			Detail:  err.Error(),
		}
	}
	return &dto.AppError{
		Code:    dto.ErrConnectionFailed,
		Message: "数据库操作失败",
		Detail:  err.Error(),
	}
}
