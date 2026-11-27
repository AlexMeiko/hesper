package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
)

const (
	textTruncLimit = 64 * 1024
	binPreviewLen  = 64
)

var connIDSeq atomic.Uint64

// Opener opens physical SQLite target database connections via pure-Go modernc.org/sqlite (no CGO).
type Opener struct{}

func NewOpener() *Opener { return &Opener{} }

func (o *Opener) Type() dto.EngineType { return dto.EngineSQLite }

func (o *Opener) Open(ctx context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	filePath := strings.TrimSpace(cfg.FilePath)
	if filePath == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "SQLite 数据库文件路径不能为空", "")
	}

	isMemory := filePath == ":memory:" || strings.HasPrefix(filePath, "file::memory:")
	if !isMemory {
		info, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, dto.NewAppError(dto.ErrConnectionFailed, "SQLite 数据库文件不存在", filePath)
			}
			return nil, dto.NewAppError(dto.ErrConnectionFailed, "无法访问 SQLite 数据库文件", err.Error())
		}
		if info.IsDir() {
			return nil, dto.NewAppError(dto.ErrInvalidArgument, "目标路径是目录而非 SQLite 数据库文件", filePath)
		}
	}

	dsn := buildDSN(filePath, isMemory, cfg.ReadOnly)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, classifySQLiteError(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, classifySQLiteError(err)
	}

	var versionStr string
	if err := conn.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&versionStr); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, classifySQLiteError(err)
	}

	if cfg.ReadOnly {
		// Double-lock read-only at engine level (in addition to URI mode=ro for files).
		_, _ = conn.ExecContext(ctx, "PRAGMA query_only = ON")
	}

	id := connIDSeq.Add(1)
	return &Conn{
		db:           db,
		conn:         conn,
		filePath:     filePath,
		version:      versionStr,
		serverConnID: fmt.Sprintf("sqlite-%d", id),
		readOnly:     cfg.ReadOnly,
	}, nil
}

func buildDSN(filePath string, isMemory, readOnly bool) string {
	if isMemory {
		if readOnly {
			return "file::memory:?_pragma=query_only(ON)&_pragma=busy_timeout(5000)"
		}
		return "file::memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(ON)")
		q.Add("_pragma", "busy_timeout(5000)")
	} else {
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "foreign_keys(ON)")
	}
	return buildFileURI(filepath.ToSlash(absPath), q)
}

// buildFileURI constructs a cross-platform SQLite file:/// URI with an empty authority.
// On Windows, filepath.ToSlash(absPath) returns "C:/..." without a leading "/"; passing that
// directly to url.URL{Scheme: "file", Path: "C:/..."} yields "file://C:/...", which SQLite rejects
// with "invalid uri authority: C:". Prepending "/" yields "file:///C:/...", matching SQLite's URI spec.
func buildFileURI(slashPath string, q url.Values) string {
	if strings.HasPrefix(slashPath, "//?/") && isWindowsDrivePath(slashPath[4:]) {
		slashPath = "/" + slashPath[4:]
	} else if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := url.URL{
		Scheme:   "file",
		Path:     slashPath,
		RawQuery: q.Encode(),
	}
	return u.String()
}

func isWindowsDrivePath(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// Conn represents one dedicated SQLite connection.
// Implements driver.Conn, QueryExecutor, ExecutionCanceler, MetaInspector, TableBrowser.
type Conn struct {
	mu           sync.Mutex
	db           *sql.DB
	conn         *sql.Conn
	filePath     string
	version      string
	serverConnID string
	readOnly     bool
	closed       bool

	cancelMu        sync.Mutex
	activeCancel    context.CancelFunc
	activeCancelSeq uint64
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
	conn := c.conn
	db := c.db
	c.mu.Unlock()

	c.cancelMu.Lock()
	if c.activeCancel != nil {
		c.activeCancel()
		c.activeCancel = nil
	}
	c.cancelMu.Unlock()

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
	return driver.ServerFact{
		DetectedType:  dto.EngineSQLite,
		ServerVersion: c.version,
		CurrentDB:     "main",
		ServerConnID:  c.serverConnID,
	}, nil
}

func (c *Conn) Dialect() dto.EngineType { return dto.EngineSQLite }

func (c *Conn) Capabilities(_ string) dto.Capabilities {
	return dto.Capabilities{
		HasDatabases:               false,
		HasSchemas:                 false,
		SwitchDBReconnects:         false,
		SupportsCancel:             true,
		SupportsViews:              true,
		SupportsMatViews:           false,
		SupportsRoutines:           false,
		SupportsTriggers:           true,
		SupportsSequences:          false,
		SupportsSavepoint:          true,
		SupportsExplain:            true,
		DDLTransactional:           true,
		SupportsSingleStmtAlterAll: false,
	}
}

func (c *Conn) ResetWireTracker()              {}
func (c *Conn) WriteSentWithoutResponse() bool { return false }

func (c *Conn) ClassifyError(err error, _ bool) *dto.AppError {
	return classifySQLiteError(err)
}

// --- QueryExecutor ---

func (c *Conn) Query(ctx context.Context, stmt driver.BoundStatement) (driver.RowStream, error) {
	qCtx, qCancel := context.WithCancel(ctx)
	seq := c.setActiveCancel(qCancel)

	rows, err := c.conn.QueryContext(qCtx, stmt.SQL, stmt.Args...)
	if err != nil {
		c.clearActiveCancel(seq)
		qCancel()
		return nil, classifySQLiteError(err)
	}

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		c.clearActiveCancel(seq)
		qCancel()
		return nil, classifySQLiteError(err)
	}

	cols := make([]dto.ColumnMeta, len(colTypes))
	for i, ct := range colTypes {
		dbType := ct.DatabaseTypeName()
		nullable, ok := ct.Nullable()
		if !ok {
			nullable = true
		}
		cols[i] = dto.ColumnMeta{
			Name:         ct.Name(),
			DatabaseType: dbType,
			Kind:         classifySQLiteColumnKind(dbType),
			Nullable:     nullable,
		}
	}

	return &sqliteRowStream{
		conn:      c,
		rows:      rows,
		cols:      cols,
		qCancel:   qCancel,
		cancelSeq: seq,
	}, nil
}

func (c *Conn) Exec(ctx context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	qCtx, qCancel := context.WithCancel(ctx)
	seq := c.setActiveCancel(qCancel)
	defer func() {
		c.clearActiveCancel(seq)
		qCancel()
	}()

	res, err := c.conn.ExecContext(qCtx, stmt.SQL, stmt.Args...)
	if err != nil {
		return driver.ExecFact{}, classifySQLiteError(err)
	}
	affected, _ := res.RowsAffected()
	return driver.ExecFact{AffectedRows: affected}, nil
}

// --- ExecutionCanceler ---

// CancelInFlight interrupts the currently executing in-process SQLite statement via context cancel
// (which modernc.org/sqlite hooks to sqlite3_interrupt). Each statement holds its own isolated qCancel,
// so canceling statement A can NEVER disturb a queued statement B.
func (c *Conn) CancelInFlight(_ context.Context, _ driver.Conn) (bool, error) {
	c.cancelMu.Lock()
	cancelFn := c.activeCancel
	c.activeCancel = nil
	c.cancelMu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
	return true, nil
}

func (c *Conn) setActiveCancel(fn context.CancelFunc) uint64 {
	c.cancelMu.Lock()
	c.activeCancelSeq++
	seq := c.activeCancelSeq
	c.activeCancel = fn
	c.cancelMu.Unlock()
	return seq
}

func (c *Conn) clearActiveCancel(seq uint64) {
	c.cancelMu.Lock()
	if c.activeCancelSeq == seq {
		c.activeCancel = nil
	}
	c.cancelMu.Unlock()
}

// --- MetaInspector ---

func (c *Conn) ListDatabases(ctx context.Context) ([]dto.DatabaseInfo, error) {
	rows, err := c.conn.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return []dto.DatabaseInfo{{Name: "main", IsSystem: false}}, nil
	}
	defer rows.Close()

	var out []dto.DatabaseInfo
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return nil, err
		}
		out = append(out, dto.DatabaseInfo{
			Name:     name,
			IsSystem: strings.EqualFold(name, "temp"),
		})
	}
	if len(out) == 0 {
		return []dto.DatabaseInfo{{Name: "main", IsSystem: false}}, nil
	}
	return out, rows.Err()
}

func (c *Conn) ListSchemas(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (c *Conn) ListTables(ctx context.Context, db, _ string) ([]dto.TableSummary, error) {
	if db == "" {
		db = "main"
	}
	master := QuoteIdent(db) + ".sqlite_master"
	rows, err := c.conn.QueryContext(ctx, fmt.Sprintf(`
		SELECT name FROM %s
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%%'
		ORDER BY name`, master))
	if err != nil {
		return nil, classifySQLiteError(err)
	}
	defer rows.Close()

	var out []dto.TableSummary
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, dto.TableSummary{
			Database: db,
			Name:     name,
			Engine:   "SQLite",
		})
	}
	return out, rows.Err()
}

func (c *Conn) ListViews(ctx context.Context, db, _ string) ([]dto.ObjectSummary, error) {
	if db == "" {
		db = "main"
	}
	master := QuoteIdent(db) + ".sqlite_master"
	rows, err := c.conn.QueryContext(ctx, fmt.Sprintf(`
		SELECT name FROM %s
		WHERE type = 'view'
		ORDER BY name`, master))
	if err != nil {
		return nil, classifySQLiteError(err)
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

func (c *Conn) GetTableDetail(ctx context.Context, db, _, table string) (*dto.TableDetail, error) {
	if db == "" {
		db = "main"
	}
	detail := &dto.TableDetail{
		Database: db,
		Name:     table,
		Engine:   "SQLite",
	}

	cols, _, err := c.readColumns(ctx, db, table)
	if err != nil {
		return nil, err
	}
	detail.Columns = cols

	idxs, _, err := c.readIndexes(ctx, db, table)
	if err != nil {
		return nil, err
	}
	detail.Indexes = idxs

	edit, err := c.InspectTableEditability(ctx, db, "", table, c.readOnly)
	if err != nil {
		return nil, err
	}
	detail.Editability = edit
	return detail, nil
}

func (c *Conn) GetTableDDL(ctx context.Context, db, _, table string) (string, error) {
	if db == "" {
		db = "main"
	}
	master := QuoteIdent(db) + ".sqlite_master"
	var mainDDL sql.NullString
	err := c.conn.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT sql FROM %s
		WHERE type IN ('table', 'view') AND name = ?`, master), table).Scan(&mainDDL)
	if err != nil {
		return "", classifySQLiteError(err)
	}

	var parts []string
	if mainDDL.Valid && strings.TrimSpace(mainDDL.String) != "" {
		parts = append(parts, strings.TrimRight(strings.TrimSpace(mainDDL.String), ";")+";")
	}

	// Include secondary indexes & triggers defined on this table for faithful DDL inspection.
	rows, err := c.conn.QueryContext(ctx, fmt.Sprintf(`
		SELECT sql FROM %s
		WHERE type IN ('index', 'trigger') AND tbl_name = ? AND sql IS NOT NULL
		ORDER BY type, name`, master), table)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimRight(strings.TrimSpace(s), ";")+";")
			}
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// InspectTableEditability computes 3-dimension editability for SQLite.
// CRITICAL RULE (Section 3.2 VACUUM rowid regression):
// Only EXPLICIT PRIMARY KEY (including INTEGER PRIMARY KEY) or non-partial UNIQUE index
// over all NOT NULL columns qualifies for row identity. Implicit rowid is STRICTLY REJECTED.
func (c *Conn) InspectTableEditability(ctx context.Context, db, _, table string, clientReadOnly bool) (dto.TableEditability, error) {
	if db == "" {
		db = "main"
	}
	edit := dto.TableEditability{
		CanWrite:           !clientReadOnly && !c.readOnly,
		RowIdentity:        dto.RowIdentityNone,
		BatchTransactional: true,
		StorageEngine:      "SQLite",
	}
	if clientReadOnly || c.readOnly {
		edit.Reasons = append(edit.Reasons, "连接已启用只读模式 (mode=ro / query_only)")
	}

	cols, pkCols, err := c.readColumns(ctx, db, table)
	if err != nil {
		return edit, err
	}
	notNullByCol := make(map[string]bool, len(cols))
	for _, col := range cols {
		notNullByCol[col.Name] = !col.Nullable
	}

	if len(pkCols) > 0 {
		edit.RowIdentity = dto.RowIdentityPrimaryKey
		edit.KeyColumns = pkCols
		return edit, nil
	}

	// No explicit primary key: check for a non-partial UNIQUE index covering only NOT NULL columns.
	_, idxMeta, err := c.readIndexes(ctx, db, table)
	if err != nil {
		return edit, err
	}
	for _, idx := range idxMeta {
		if !idx.unique || idx.partial || len(idx.columns) == 0 || idx.hasExpression {
			continue
		}
		allNotNull := true
		for _, colName := range idx.columns {
			if !notNullByCol[colName] {
				allNotNull = false
				break
			}
		}
		if allNotNull {
			edit.RowIdentity = dto.RowIdentityUniqueNotNull
			edit.KeyColumns = idx.columns
			return edit, nil
		}
	}

	edit.Reasons = append(edit.Reasons, "SQLite 表缺少显式主键或全列非空唯一键 (禁止使用隐式 rowid 延迟定位行，因 VACUUM 会重排 rowid)")
	return edit, nil
}

func (c *Conn) FetchCompletionCatalog(ctx context.Context, db, _ string) (*dto.CompletionCatalog, error) {
	if db == "" {
		db = "main"
	}
	tables, err := c.ListTables(ctx, db, "")
	if err != nil {
		return nil, err
	}
	views, _ := c.ListViews(ctx, db, "")

	cat := &dto.CompletionCatalog{
		Database: db,
		Tables:   make(map[string][]string, len(tables)+len(views)),
	}
	for _, t := range tables {
		cols, _, cerr := c.readColumns(ctx, db, t.Name)
		if cerr != nil {
			continue
		}
		names := make([]string, len(cols))
		for i, col := range cols {
			names[i] = col.Name
		}
		cat.Tables[t.Name] = names
	}
	for _, v := range views {
		cols, _, cerr := c.readColumns(ctx, db, v.Name)
		if cerr != nil {
			continue
		}
		names := make([]string, len(cols))
		for i, col := range cols {
			names[i] = col.Name
		}
		cat.Tables[v.Name] = names
	}
	return cat, nil
}

// --- TableBrowser ---

func (c *Conn) BrowseTable(ctx context.Context, req dto.BrowseTableRequest, edit dto.TableEditability) (*dto.BrowseTableResponse, error) {
	db := req.Database
	if db == "" {
		db = "main"
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

	colsInfo, _, err := c.readColumns(ctx, db, req.Table)
	if err != nil {
		return nil, err
	}
	validCols := make(map[string]bool, len(colsInfo))
	pkSet := make(map[string]bool, len(edit.KeyColumns))
	for _, col := range colsInfo {
		validCols[col.Name] = true
		if col.IsPrimaryKey {
			pkSet[col.Name] = true
		}
	}
	for _, k := range edit.KeyColumns {
		pkSet[k] = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT * FROM %s.%s", QuoteIdent(db), QuoteIdent(req.Table))
	if filter := strings.TrimSpace(req.FilterWhere); filter != "" {
		fmt.Fprintf(&b, " WHERE %s", filter)
	}

	var orderCols []string
	stableOrder := false
	if req.SortColumn != "" && validCols[req.SortColumn] {
		dir := "ASC"
		if strings.EqualFold(req.SortDir, "desc") {
			dir = "DESC"
		}
		fmt.Fprintf(&b, " ORDER BY %s %s", QuoteIdent(req.SortColumn), dir)
		orderCols = append(orderCols, req.SortColumn)
		for _, k := range edit.KeyColumns {
			if k != req.SortColumn {
				fmt.Fprintf(&b, ", %s ASC", QuoteIdent(k))
				orderCols = append(orderCols, k)
			}
		}
		stableOrder = len(edit.KeyColumns) > 0
	} else if len(edit.KeyColumns) > 0 {
		b.WriteString(" ORDER BY ")
		for i, k := range edit.KeyColumns {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s ASC", QuoteIdent(k))
			orderCols = append(orderCols, k)
		}
		stableOrder = true
	}

	offset := (page - 1) * pageSize
	fmt.Fprintf(&b, " LIMIT %d OFFSET %d", pageSize+1, offset)
	execSQL := b.String()

	start := time.Now()
	rows, err := c.conn.QueryContext(ctx, execSQL)
	if err != nil {
		return nil, classifySQLiteError(err)
	}

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		return nil, classifySQLiteError(err)
	}

	cols := make([]dto.ColumnMeta, len(colTypes))
	for i, ct := range colTypes {
		dbType := ct.DatabaseTypeName()
		cols[i] = dto.ColumnMeta{
			Name:         ct.Name(),
			DatabaseType: dbType,
			Kind:         classifySQLiteColumnKind(dbType),
			Nullable:     true,
			IsPrimaryKey: pkSet[ct.Name()],
		}
	}

	var data [][]dto.CellValue
	hasMore := false
	scanVals := make([]any, len(cols))
	scanPtrs := make([]any, len(cols))
	for i := range scanVals {
		scanPtrs[i] = &scanVals[i]
	}

	for rows.Next() {
		if len(data) == pageSize {
			hasMore = true
			break
		}
		if err := rows.Scan(scanPtrs...); err != nil {
			_ = rows.Close()
			return nil, classifySQLiteError(err)
		}
		row := make([]dto.CellValue, len(cols))
		for i := range cols {
			row[i] = convertSQLiteCell(scanVals[i], cols[i], nil)
		}
		data = append(data, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, classifySQLiteError(err)
	}
	// Explicitly close open cursor BEFORE issuing COUNT(*) on the single pinned connection.
	_ = rows.Close()

	var totalRows int64
	totalKnown := false
	var countSQL strings.Builder
	fmt.Fprintf(&countSQL, "SELECT COUNT(*) FROM %s.%s", QuoteIdent(db), QuoteIdent(req.Table))
	if filter := strings.TrimSpace(req.FilterWhere); filter != "" {
		fmt.Fprintf(&countSQL, " WHERE %s", filter)
	}
	if err := c.conn.QueryRowContext(ctx, countSQL.String()).Scan(&totalRows); err == nil {
		totalKnown = true
	}

	return &dto.BrowseTableResponse{
		Columns:         cols,
		Rows:            data,
		Page:            page,
		PageSize:        pageSize,
		HasMore:         hasMore,
		TotalKnown:      totalKnown,
		TotalRows:       totalRows,
		DurationMs:      time.Since(start).Milliseconds(),
		ExecutedSQL:     execSQL,
		StableOrderUsed: stableOrder,
		OrderColumns:    orderCols,
		Editability:     edit,
	}, nil
}

// --- Internal Metadata Helpers ---

type pkEntry struct {
	name  string
	order int
}

func (c *Conn) readColumns(ctx context.Context, db, table string) ([]dto.ColumnDetail, []string, error) {
	q := fmt.Sprintf("PRAGMA %s.table_info(%s)", QuoteIdent(db), QuoteIdent(table))
	rows, err := c.conn.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, classifySQLiteError(err)
	}
	defer rows.Close()

	var cols []dto.ColumnDetail
	var pks []pkEntry
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, nil, err
		}
		col := dto.ColumnDetail{
			Name:         name,
			DataType:     colType,
			ColumnType:   colType,
			Nullable:     notNull == 0,
			IsPrimaryKey: pk > 0,
			HasDefault:   dflt.Valid,
			DefaultValue: dflt.String,
			OrdinalPos:   cid + 1,
		}
		if pk > 0 {
			pks = append(pks, pkEntry{name: name, order: pk})
		}
		cols = append(cols, col)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].order < pks[j].order })
	pkNames := make([]string, len(pks))
	for i, p := range pks {
		pkNames[i] = p.name
	}
	return cols, pkNames, nil
}

type sqliteIndexMeta struct {
	name          string
	unique        bool
	origin        string
	partial       bool
	columns       []string
	hasExpression bool
}

func (c *Conn) readIndexes(ctx context.Context, db, table string) ([]dto.IndexDetail, []sqliteIndexMeta, error) {
	q := fmt.Sprintf("PRAGMA %s.index_list(%s)", QuoteIdent(db), QuoteIdent(table))
	rows, err := c.conn.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, classifySQLiteError(err)
	}

	type rawIdx struct {
		name    string
		unique  bool
		origin  string
		partial bool
	}
	var rawList []rawIdx
	for rows.Next() {
		var seq, uniq, part int
		var name, origin string
		if err := rows.Scan(&seq, &name, &uniq, &origin, &part); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		rawList = append(rawList, rawIdx{
			name:    name,
			unique:  uniq != 0,
			origin:  origin,
			partial: part != 0,
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	_ = rows.Close()

	details := make([]dto.IndexDetail, 0, len(rawList))
	metas := make([]sqliteIndexMeta, 0, len(rawList))
	for _, r := range rawList {
		iq := fmt.Sprintf("PRAGMA %s.index_info(%s)", QuoteIdent(db), QuoteIdent(r.name))
		irows, ierr := c.conn.QueryContext(ctx, iq)
		if ierr != nil {
			continue
		}
		var colNames []string
		hasExpr := false
		for irows.Next() {
			var seqno, cid int
			var cname sql.NullString
			if irows.Scan(&seqno, &cid, &cname) == nil {
				if cid < 0 || !cname.Valid {
					hasExpr = true
				} else {
					colNames = append(colNames, cname.String)
				}
			}
		}
		_ = irows.Close()

		details = append(details, dto.IndexDetail{
			Name:      r.name,
			Columns:   colNames,
			Unique:    r.unique,
			Primary:   r.origin == "pk",
			IndexType: "BTREE",
		})
		metas = append(metas, sqliteIndexMeta{
			name:          r.name,
			unique:        r.unique,
			origin:        r.origin,
			partial:       r.partial,
			columns:       colNames,
			hasExpression: hasExpr,
		})
	}
	return details, metas, nil
}

// --- Row Stream ---

type sqliteRowStream struct {
	conn      *Conn
	rows      *sql.Rows
	cols      []dto.ColumnMeta
	qCancel   context.CancelFunc
	cancelSeq uint64
	scanVals  []any
	scanPtrs  []any
	closed    bool
}

func (s *sqliteRowStream) Columns() []dto.ColumnMeta {
	return s.cols
}

func (s *sqliteRowStream) Next(regBlob driver.BlobRegistrar) ([]dto.CellValue, bool, error) {
	if !s.rows.Next() {
		if err := s.rows.Err(); err != nil {
			return nil, false, classifySQLiteError(err)
		}
		return nil, false, nil
	}
	if s.scanVals == nil {
		s.scanVals = make([]any, len(s.cols))
		s.scanPtrs = make([]any, len(s.cols))
		for i := range s.scanVals {
			s.scanPtrs[i] = &s.scanVals[i]
		}
	}
	if err := s.rows.Scan(s.scanPtrs...); err != nil {
		return nil, false, classifySQLiteError(err)
	}
	out := make([]dto.CellValue, len(s.cols))
	for i := range s.cols {
		out[i] = convertSQLiteCell(s.scanVals[i], s.cols[i], regBlob)
	}
	return out, true, nil
}

func (s *sqliteRowStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.rows.Close()
	s.conn.clearActiveCancel(s.cancelSeq)
	s.qCancel()
	if err != nil {
		return classifySQLiteError(err)
	}
	return nil
}

// --- Value Fidelity Conversion ---

func convertSQLiteCell(raw any, col dto.ColumnMeta, regBlob driver.BlobRegistrar) dto.CellValue {
	if raw == nil {
		return dto.CellValue{Kind: dto.KindNull}
	}

	switch v := raw.(type) {
	case int64:
		kind := dto.KindInt
		if col.Kind == dto.KindBigInt || v > 9007199254740991 || v < -9007199254740991 {
			kind = dto.KindBigInt
		}
		return dto.CellValue{
			Kind: kind,
			Text: strconv.FormatInt(v, 10),
		}
	case float64:
		return dto.CellValue{
			Kind: dto.KindFloat,
			Text: strconv.FormatFloat(v, 'g', -1, 64),
		}
	case bool:
		if v {
			return dto.CellValue{Kind: dto.KindBool, Text: "true"}
		}
		return dto.CellValue{Kind: dto.KindBool, Text: "false"}
	case []byte:
		cell := dto.CellValue{
			Kind:       dto.KindBinary,
			ByteLength: int64(len(v)),
		}
		if len(v) > binPreviewLen {
			cell.Text = "0x" + hex.EncodeToString(v[:binPreviewLen]) + "..."
			cell.Truncated = true
		} else {
			cell.Text = "0x" + hex.EncodeToString(v)
		}
		if regBlob != nil && len(v) > 0 {
			cp := make([]byte, len(v))
			copy(cp, v)
			cell.StreamRef = regBlob("application/octet-stream", col.Name+".bin", cp)
		}
		return cell
	case time.Time:
		return dto.CellValue{
			Kind: dto.KindDateTime,
			Text: v.Format("2006-01-02 15:04:05.999999999"),
		}
	case string:
		kind := dto.KindString
		switch col.Kind {
		case dto.KindDecimal, dto.KindJSON, dto.KindDateTime, dto.KindBigInt:
			kind = col.Kind
		}
		if len(v) > textTruncLimit {
			cell := dto.CellValue{
				Kind:       kind,
				Text:       truncateUTF8String(v, textTruncLimit),
				Truncated:  true,
				ByteLength: int64(len(v)),
			}
			if regBlob != nil {
				cell.StreamRef = regBlob("text/plain; charset=utf-8", col.Name+".txt", []byte(v))
			}
			return cell
		}
		return dto.CellValue{
			Kind: kind,
			Text: v,
		}
	default:
		return dto.CellValue{
			Kind: dto.KindString,
			Text: fmt.Sprint(v),
		}
	}
}

func classifySQLiteColumnKind(dbType string) dto.ValueKind {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	switch {
	case strings.Contains(t, "BIGINT"):
		return dto.KindBigInt
	case strings.Contains(t, "DECIMAL") || strings.Contains(t, "NUMERIC"):
		return dto.KindDecimal
	case strings.Contains(t, "INT"):
		return dto.KindInt
	case strings.Contains(t, "REAL") || strings.Contains(t, "FLOA") || strings.Contains(t, "DOUB"):
		return dto.KindFloat
	case strings.Contains(t, "BLOB"):
		return dto.KindBinary
	case strings.Contains(t, "JSON"):
		return dto.KindJSON
	case strings.Contains(t, "DATE") || strings.Contains(t, "TIME"):
		return dto.KindDateTime
	case strings.Contains(t, "BOOL"):
		return dto.KindBool
	default:
		return dto.KindString
	}
}

func truncateUTF8String(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	b := []byte(s[:limit])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func QuoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func classifySQLiteError(err error) *dto.AppError {
	if err == nil {
		return nil
	}
	var appErr *dto.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if errors.Is(err, context.Canceled) || strings.Contains(lower, "interrupted") || strings.Contains(lower, "sqlite_interrupt") {
		return &dto.AppError{
			Code:    dto.ErrStatementCanceled,
			Message: "当前语句已取消，原会话保持存活",
			Detail:  msg,
		}
	}
	if strings.Contains(lower, "readonly") || strings.Contains(lower, "read-only") {
		return &dto.AppError{
			Code:    dto.ErrReadOnlyDenied,
			Message: "SQLite 数据库处于只读模式，拒绝写入操作",
			Detail:  msg,
		}
	}
	if strings.Contains(lower, "syntax error") || strings.Contains(lower, "near ") {
		return &dto.AppError{
			Code:    dto.ErrSQLSyntax,
			Message: "SQL 语法错误",
			Detail:  msg,
		}
	}
	return &dto.AppError{
		Code:    dto.ErrInternal,
		Message: "SQLite 执行失败",
		Detail:  msg,
	}
}
