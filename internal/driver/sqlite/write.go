package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	moderncsqlite "modernc.org/sqlite"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

const sqliteRollbackCleanupTimeout = 3 * time.Second

type sqliteQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// InspectWriteSchema reads the authoritative WriteSchema for PreviewGridChanges.
func (c *Conn) InspectWriteSchema(ctx context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.WriteSchema, error) {
	return c.inspectWriteSchema(ctx, c.conn, target, clientReadOnly)
}

// BeginTableWrite starts a SQLite write transaction with BEGIN IMMEDIATE (acquiring the RESERVED write lock
// immediately to prevent concurrent DDL or write races) and reads the authoritative WriteSchema inside that transaction.
func (c *Conn) BeginTableWrite(ctx context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.TableWriteTx, error) {
	dbName := target.Database
	if dbName == "" {
		dbName = "main"
	}
	if strings.TrimSpace(target.Table) == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "表名不能为空", "")
	}

	if _, err := c.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, classifySQLiteError(err)
	}

	ws, err := c.inspectWriteSchema(ctx, c.conn, driver.WriteTarget{
		Database: dbName,
		Table:    target.Table,
	}, clientReadOnly)
	if err != nil {
		rbCtx, rbCancel := context.WithTimeout(context.Background(), sqliteRollbackCleanupTimeout)
		_, _ = c.conn.ExecContext(rbCtx, "ROLLBACK")
		rbCancel()
		return nil, err
	}

	return &sqliteTableWriteTx{
		conn:   c,
		schema: ws,
	}, nil
}

type sqliteTableFlags struct {
	tableType    string
	withoutRowID bool
	strict       bool
}

func (c *Conn) readTableFlags(ctx context.Context, q sqliteQuerier, db, table string) (sqliteTableFlags, error) {
	// PRAGMA table_list(table) returns: (schema, name, type, ncol, wr, strict) in SQLite 3.37+.
	rows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA %s.table_list(%s)", QuoteIdent(db), QuoteIdent(table)))
	if err != nil {
		return sqliteTableFlags{}, classifySQLiteError(err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, name, tType string
		var ncol, wr, strict int
		if err := rows.Scan(&schema, &name, &tType, &ncol, &wr, &strict); err != nil {
			return sqliteTableFlags{}, err
		}
		if strings.EqualFold(schema, db) && strings.EqualFold(name, table) {
			return sqliteTableFlags{
				tableType:    tType,
				withoutRowID: wr != 0,
				strict:       strict != 0,
			}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return sqliteTableFlags{}, classifySQLiteError(err)
	}
	return sqliteTableFlags{}, dto.NewAppError(dto.ErrNotFound, fmt.Sprintf("表 %s.%s 不存在", db, table), "")
}

func (c *Conn) inspectWriteSchema(ctx context.Context, q sqliteQuerier, target driver.WriteTarget, clientReadOnly bool) (driver.WriteSchema, error) {
	dbName := target.Database
	if dbName == "" {
		dbName = "main"
	}

	flags, err := c.readTableFlags(ctx, q, dbName, target.Table)
	if err != nil {
		return driver.WriteSchema{}, err
	}
	isBaseTable := strings.EqualFold(flags.tableType, "table")

	writeCols, pkCols, err := c.readWriteColumns(ctx, q, dbName, target.Table, flags)
	if err != nil {
		return driver.WriteSchema{}, err
	}
	colByName := make(map[string]driver.WriteColumn, len(writeCols))
	for _, wc := range writeCols {
		colByName[wc.Detail.Name] = wc
	}

	_, idxMetas, err := c.readIndexesWith(ctx, q, dbName, target.Table)
	if err != nil {
		return driver.WriteSchema{}, err
	}

	pkIndexExists := false
	for _, idx := range idxMetas {
		if idx.origin == "pk" {
			pkIndexExists = true
			break
		}
	}

	edit := dto.TableEditability{
		CanWrite:           !clientReadOnly && !c.readOnly && isBaseTable,
		RowIdentity:        dto.RowIdentityNone,
		BatchTransactional: isBaseTable,
		StorageEngine:      "SQLite",
	}
	if clientReadOnly || c.readOnly {
		edit.Reasons = append(edit.Reasons, "连接已启用只读模式 (mode=ro / query_only)")
	}
	if !isBaseTable {
		edit.Reasons = append(edit.Reasons, "non_base_table:"+flags.tableType)
	}

	// CRITICAL RULE:
	// In SQLite, a non-STRICT rowid table PRIMARY KEY (such as TEXT PRIMARY KEY, INT PRIMARY KEY,
	// or composite PRIMARY KEY(a, b) without NOT NULL) allows duplicate NULL rows!
	// A PRIMARY KEY qualifies as RowIdentityPrimaryKey ONLY IF all key columns are KeyComparable AND:
	//   1. WITHOUT ROWID table (wr == 1), OR
	//   2. STRICT table (strict == 1, implicitly enforces NOT NULL on PK), OR
	//   3. Single-column INTEGER PRIMARY KEY (exact "INTEGER" type, wr == 0, no origin=="pk" autoindex -> rowid alias), OR
	//   4. Every PK column is explicitly NOT NULL (notnull == 1).
	pkQualifies := false
	if len(pkCols) > 0 && allSQLiteColsKeyComparable(pkCols, colByName) {
		if flags.withoutRowID || flags.strict {
			pkQualifies = true
		} else if len(pkCols) == 1 && !pkIndexExists &&
			strings.EqualFold(strings.TrimSpace(colByName[pkCols[0]].NativeType), "INTEGER") {
			pkQualifies = true
		} else {
			allNotNull := true
			for _, pkName := range pkCols {
				if colByName[pkName].Detail.Nullable {
					allNotNull = false
					break
				}
			}
			pkQualifies = allNotNull
		}
	}

	// Only a single-column rowid-alias INTEGER PRIMARY KEY (without WITHOUT ROWID or DESC pk autoindex)
	// auto-generates rowid values on omitted INSERT.
	isRowIDAlias := len(pkCols) == 1 && !flags.withoutRowID && !pkIndexExists &&
		strings.EqualFold(strings.TrimSpace(colByName[pkCols[0]].NativeType), "INTEGER")
	for i := range writeCols {
		writeCols[i].Detail.AutoIncrement = isRowIDAlias && writeCols[i].Detail.Name == pkCols[0]
	}

	if pkQualifies {
		edit.RowIdentity = dto.RowIdentityPrimaryKey
		edit.KeyColumns = pkCols
	} else {
		// Fall through to non-partial, non-expression UNIQUE indexes covering only NOT NULL & key-comparable columns.
		for _, idx := range idxMetas {
			if !idx.unique || idx.partial || len(idx.columns) == 0 || idx.hasExpression {
				continue
			}
			allNotNullAndComparable := true
			for _, colName := range idx.columns {
				wc, ok := colByName[colName]
				if !ok || wc.Detail.Nullable || !wc.KeyComparable {
					allNotNullAndComparable = false
					break
				}
			}
			if allNotNullAndComparable {
				edit.RowIdentity = dto.RowIdentityUniqueNotNull
				edit.KeyColumns = idx.columns
				break
			}
		}
	}

	if edit.RowIdentity == dto.RowIdentityNone {
		if len(pkCols) > 0 {
			edit.Reasons = append(edit.Reasons, "SQLite 主键列允许 NULL 或不可比较，无法安全唯一标识行")
		} else {
			edit.Reasons = append(edit.Reasons, "SQLite 表缺少显式主键或全列非空唯一键 (禁止使用隐式 rowid 延迟定位行，因 VACUUM 会重排 rowid)")
		}
	}

	return driver.WriteSchema{
		Dialect:       dto.EngineSQLite,
		Database:      dbName,
		Table:         target.Table,
		StorageEngine: "SQLite",
		WithoutRowID:  flags.withoutRowID,
		Strict:        flags.strict,
		Editability:   edit,
		Columns:       writeCols,
	}, nil
}

func allSQLiteColsKeyComparable(cols []string, colMap map[string]driver.WriteColumn) bool {
	if len(cols) == 0 {
		return false
	}
	for _, name := range cols {
		wc, ok := colMap[name]
		if !ok || !wc.KeyComparable {
			return false
		}
	}
	return true
}

// ComputeSQLiteAffinity computes a column's SQLite type affinity strictly per SQLite docs Section 3.1
// for standard (non-STRICT) tables.
func ComputeSQLiteAffinity(declaredType string) string {
	up := strings.ToUpper(strings.TrimSpace(declaredType))
	if up == "" {
		return "blob"
	}
	if strings.Contains(up, "INT") {
		return "integer"
	}
	if strings.Contains(up, "CHAR") || strings.Contains(up, "CLOB") || strings.Contains(up, "TEXT") {
		return "text"
	}
	if strings.Contains(up, "BLOB") {
		return "blob"
	}
	if strings.Contains(up, "REAL") || strings.Contains(up, "FLOA") || strings.Contains(up, "DOUB") {
		return "real"
	}
	return "numeric"
}

// ComputeSQLiteColumnAffinity computes effective SQLite column affinity accounting for STRICT tables:
// in STRICT tables, ANY has NO affinity (preserves verbatim TEXT such as "000123"), whereas in regular
// non-STRICT tables ANY falls through to NUMERIC affinity.
func ComputeSQLiteColumnAffinity(declaredType string, strict bool) string {
	if strict && strings.EqualFold(strings.TrimSpace(declaredType), "ANY") {
		return "any"
	}
	return ComputeSQLiteAffinity(declaredType)
}

func (c *Conn) readWriteColumns(ctx context.Context, q sqliteQuerier, db, table string, flags sqliteTableFlags) ([]driver.WriteColumn, []string, error) {
	// PRAGMA table_xinfo exposes generated columns (hidden=2 virtual, hidden=3 stored) that table_info hides.
	query := fmt.Sprintf("PRAGMA %s.table_xinfo(%s)", QuoteIdent(db), QuoteIdent(table))
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, classifySQLiteError(err)
	}
	defer rows.Close()

	var cols []driver.WriteColumn
	var pks []pkEntry
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk, hidden int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk, &hidden); err != nil {
			return nil, nil, err
		}
		// hidden == 1 represents internal hidden columns (e.g. virtual table hidden cols); skip.
		if hidden == 1 {
			continue
		}

		genKind := ""
		switch hidden {
		case 2:
			genKind = "virtual"
		case 3:
			genKind = "stored"
		}

		affinity := ComputeSQLiteColumnAffinity(colType, flags.strict)
		isDeclaredBlob := strings.Contains(strings.ToUpper(colType), "BLOB")

		strategy := driver.CompareSQLiteBinaryTypeof
		keyComparable := !isDeclaredBlob && affinity != "real"
		insertable := true
		updatable := true
		roReason := ""

		if isDeclaredBlob {
			strategy = driver.CompareUnsupported
			keyComparable = false
			insertable = false
			updatable = false
			roReason = "二进制 BLOB 列暂不支持行内编辑"
		}
		if genKind != "" {
			keyComparable = false
			insertable = false
			updatable = false
			roReason = "生成列由数据库自动计算，不可直接写入"
		}

		// In STRICT or WITHOUT ROWID tables, PK columns are implicitly non-null.
		nullable := notNull == 0
		if pk > 0 && (flags.withoutRowID || flags.strict) {
			nullable = false
		}
		autoInc := pk == 1 && !flags.withoutRowID && strings.EqualFold(strings.TrimSpace(colType), "INTEGER")

		wc := driver.WriteColumn{
			Detail: dto.ColumnDetail{
				Name:           name,
				DataType:       colType,
				ColumnType:     colType,
				Nullable:       nullable,
				IsPrimaryKey:   pk > 0,
				DefaultValue:   dflt.String,
				HasDefault:     dflt.Valid,
				AutoIncrement:  autoInc,
				GeneratedKind:  genKind,
				Insertable:     insertable,
				Updatable:      updatable,
				ReadOnlyReason: roReason,
				OrdinalPos:     len(cols) + 1,
			},
			NativeType:      colType,
			Deterministic:   true,
			SQLiteAffinity:  affinity,
			CompareStrategy: strategy,
			KeyComparable:   keyComparable,
		}
		if pk > 0 {
			pks = append(pks, pkEntry{name: name, order: pk})
		}
		cols = append(cols, wc)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, classifySQLiteError(err)
	}

	sort.Slice(pks, func(i, j int) bool { return pks[i].order < pks[j].order })
	pkNames := make([]string, len(pks))
	for i, p := range pks {
		pkNames[i] = p.name
	}
	return cols, pkNames, nil
}

// --- sqliteTableWriteTx ---

type sqliteTableWriteTx struct {
	conn            *Conn
	schema          driver.WriteSchema
	done            bool
	commitAttempted bool
}

func (t *sqliteTableWriteTx) Schema() driver.WriteSchema { return t.schema }

func (t *sqliteTableWriteTx) Exec(ctx context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	res, err := t.conn.conn.ExecContext(ctx, stmt.SQL, stmt.Args...)
	if err != nil {
		return driver.ExecFact{}, classifySQLiteError(err)
	}
	affected, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	return driver.ExecFact{
		AffectedRows: affected,
		LastInsertID: lastID,
	}, nil
}

func (t *sqliteTableWriteTx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		// Rollback did not execute; keep t.done == false so caller can retry with a valid context.
		return err
	}
	if _, err := t.conn.conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no transaction is active") {
			t.done = true
			return nil
		}
		return err
	}
	t.done = true
	return nil
}

func (t *sqliteTableWriteTx) Commit(ctx context.Context) driver.CommitResult {
	if t.done || t.commitAttempted {
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     dto.NewAppError(dto.ErrTxAborted, "事务已结束，无法重复提交", ""),
		}
	}
	t.commitAttempted = true
	if ctx != nil && ctx.Err() != nil {
		// Context already canceled BEFORE COMMIT was issued -> roll back with an independent
		// bounded context so the BEGIN IMMEDIATE write lock is released, then report NotCommitted.
		// Only mark t.done = true via t.Rollback when cleanup succeeds so a failed cleanup stays retryable.
		rbCtx, rbCancel := context.WithTimeout(context.Background(), sqliteRollbackCleanupTimeout)
		rbErr := t.Rollback(rbCtx)
		rbCancel()
		appErr := classifySQLiteError(ctx.Err())
		if rbErr != nil && appErr != nil {
			appErr.Detail = fmt.Sprintf("%s; cleanup rollback failed: %v", appErr.Detail, rbErr)
		}
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     appErr,
		}
	}
	t.done = true
	_, err := t.conn.conn.ExecContext(ctx, "COMMIT")
	return ClassifySQLiteCommitError(err)
}

// ClassifySQLiteCommitError classifies local SQLite file COMMIT errors WITHOUT relying on network WriteSent().
// Definitive non-commit SQLite error codes return NotCommitted; I/O, corrupt, protocol, or cancellation/interrupt
// errors return CommitOutcomeUnknown conservatively.
func ClassifySQLiteCommitError(err error) driver.CommitResult {
	if err == nil {
		return driver.CommitResult{Outcome: driver.CommitOutcomeCommitted}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeUnknown,
			Err: &dto.AppError{
				Code:      dto.ErrCommitOutcomeUnknown,
				Message:   "SQLite 提交过程中被取消或超时，无法确认事务是否已落盘，请先核对数据状态",
				Detail:    err.Error(),
				RetrySafe: false,
			},
		}
	}

	var se *moderncsqlite.Error
	if errors.As(err, &se) {
		primaryCode := se.Code() & 0xff
		switch primaryCode {
		case 1, // SQLITE_ERROR
			3,  // SQLITE_PERM
			4,  // SQLITE_ABORT
			5,  // SQLITE_BUSY
			6,  // SQLITE_LOCKED
			7,  // SQLITE_NOMEM
			8,  // SQLITE_READONLY
			13, // SQLITE_FULL
			18, // SQLITE_TOOBIG
			19, // SQLITE_CONSTRAINT
			20, // SQLITE_MISMATCH
			21, // SQLITE_MISUSE
			23, // SQLITE_AUTH
			25: // SQLITE_RANGE
			return driver.CommitResult{
				Outcome: driver.CommitOutcomeNotCommitted,
				Err:     classifySQLiteError(err),
			}
		}
		// SQLITE_INTERRUPT (9), SQLITE_IOERR (10), SQLITE_CORRUPT (11), SQLITE_PROTOCOL (15), etc. -> Unknown!
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeUnknown,
			Err: &dto.AppError{
				Code:      dto.ErrCommitOutcomeUnknown,
				Message:   "SQLite 提交发生底层 I/O 或中断异常，无法确认数据是否已写入磁盘，请先核对表数据",
				Detail:    err.Error(),
				RetrySafe: false,
			},
		}
	}

	return driver.CommitResult{
		Outcome: driver.CommitOutcomeUnknown,
		Err: &dto.AppError{
			Code:      dto.ErrCommitOutcomeUnknown,
			Message:   "SQLite 提交返回未预期错误，无法确认是否已生效，请先核对数据状态",
			Detail:    err.Error(),
			RetrySafe: false,
		},
	}
}
