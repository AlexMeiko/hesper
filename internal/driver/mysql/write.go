package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

const mysqlRollbackCleanupTimeout = 3 * time.Second

type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// InspectWriteSchema reads the authoritative WriteSchema for PreviewGridChanges.
func (c *Conn) InspectWriteSchema(ctx context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.WriteSchema, error) {
	return c.inspectWriteSchema(ctx, c.conn, target, clientReadOnly)
}

// BeginTableWrite starts a MySQL/MariaDB transaction, acquires a transaction-scoped SHARED_READ MDL
// on the target table (blocking concurrent ALTER TABLE ... ENGINE=MyISAM / DDL), and reads the
// authoritative WriteSchema inside that transaction.
func (c *Conn) BeginTableWrite(ctx context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.TableWriteTx, error) {
	dbName := c.resolveTargetDB(target.Database)
	if dbName == "" || strings.TrimSpace(target.Table) == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "数据库与表名不能为空", "")
	}

	if err := c.ensureWriteNoteDiagnostics(ctx); err != nil {
		return nil, err
	}

	tx, err := c.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, classifyMySQLError(err, false)
	}

	stopWatch := func() bool { return true }
	if c.tracked != nil && ctx != nil {
		stopWatch = context.AfterFunc(ctx, func() {
			_ = c.tracked.SetDeadline(time.Unix(1, 0))
		})
	}
	beginCtxDone := func() bool {
		return ctx != nil && ctx.Err() != nil
	}

	rollbackOnErr := func(cause error) (driver.TableWriteTx, error) {
		rbCtx, rbCancel := context.WithTimeout(context.Background(), mysqlRollbackCleanupTimeout)
		stopRB := watchConnDeadline(rbCtx, c.tracked, beginCtxDone)
		_ = tx.Rollback()
		stopRB()
		rbCancel()
		stopWatch()
		return nil, cause
	}

	// Acquire transaction-scoped SHARED_READ metadata lock (MDL) before reading information_schema,
	// closing the TOCTOU window where another session alters storage engine or column definitions.
	lockSQL := fmt.Sprintf("SELECT 1 FROM %s.%s WHERE 1 = 0", QuoteIdent(dbName), QuoteIdent(target.Table))
	rows, err := tx.QueryContext(ctx, lockSQL)
	if err != nil {
		return rollbackOnErr(classifyMySQLError(err, true))
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return rollbackOnErr(classifyMySQLError(err, true))
	}

	ws, err := c.inspectWriteSchema(ctx, tx, driver.WriteTarget{
		Database: dbName,
		Table:    target.Table,
	}, clientReadOnly)
	if err != nil {
		return rollbackOnErr(err)
	}

	return &mysqlTableWriteTx{
		conn:      c,
		tx:        tx,
		schema:    ws,
		beginCtx:  ctx,
		stopWatch: stopWatch,
	}, nil
}

// ensureWriteNoteDiagnostics enables Note-level diagnostics on this dedicated write connection
// so strict-mode fractional DECIMAL scale rounding (Note 1265) is always recorded in
// SHOW COUNT(*) WARNINGS even if the server defaults to sql_notes=0 or (on MariaDB 11+)
// note_verbosity=”. Runs before opening the write transaction and fails closed on any error
// except ER_UNKNOWN_SYSTEM_VARIABLE (1193) for older MariaDB releases without note_verbosity.
func (c *Conn) ensureWriteNoteDiagnostics(ctx context.Context) error {
	if _, err := c.conn.ExecContext(ctx, "SET SESSION sql_notes = 1"); err != nil {
		return classifyMySQLError(err, false)
	}
	if c.detectedType == dto.EngineMariaDB {
		if _, err := c.conn.ExecContext(ctx, "SET SESSION note_verbosity = 'basic'"); err != nil {
			if !isMySQLUnknownSystemVariable(err) {
				return classifyMySQLError(err, false)
			}
		}
	}
	return nil
}

func isMySQLUnknownSystemVariable(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1193
}

func (c *Conn) resolveTargetDB(db string) string {
	if db != "" {
		return db
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentDB
}

func (c *Conn) inspectWriteSchema(ctx context.Context, q sqlQuerier, target driver.WriteTarget, clientReadOnly bool) (driver.WriteSchema, error) {
	dbName := c.resolveTargetDB(target.Database)
	if dbName == "" || strings.TrimSpace(target.Table) == "" {
		return driver.WriteSchema{}, dto.NewAppError(dto.ErrInvalidArgument, "数据库与表名不能为空", "")
	}

	// 1. Fail-closed table existence & storage engine check.
	var engine, tableType string
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(ENGINE, ''), COALESCE(TABLE_TYPE, '')
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, dbName, target.Table).Scan(&engine, &tableType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return driver.WriteSchema{}, dto.NewAppError(dto.ErrNotFound, fmt.Sprintf("表 %s.%s 不存在", dbName, target.Table), "")
		}
		return driver.WriteSchema{}, classifyMySQLError(err, false)
	}

	isBaseTable := strings.EqualFold(tableType, "BASE TABLE")
	batchTx := isBaseTable && strings.EqualFold(engine, "InnoDB")

	edit := dto.TableEditability{
		CanWrite:           !clientReadOnly && !c.readOnly && isBaseTable,
		RowIdentity:        dto.RowIdentityNone,
		BatchTransactional: batchTx,
		StorageEngine:      engine,
	}
	if clientReadOnly || c.readOnly {
		edit.Reasons = append(edit.Reasons, "read_only_connection")
	}
	if !isBaseTable {
		edit.Reasons = append(edit.Reasons, "non_base_table:"+tableType)
	} else if !batchTx {
		edit.Reasons = append(edit.Reasons, "non_transactional_engine:"+engine)
	}

	// 2. Fail-closed columns read (including COLLATION_NAME, EXTRA, and GENERATION_EXPRESSION).
	colRows, err := q.QueryContext(ctx, `
		SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_KEY,
		       COLUMN_DEFAULT, EXTRA, COALESCE(COLLATION_NAME, ''), COALESCE(COLUMN_COMMENT, ''),
		       COALESCE(GENERATION_EXPRESSION, ''), ORDINAL_POSITION
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, dbName, target.Table)
	if err != nil {
		return driver.WriteSchema{}, classifyMySQLError(err, false)
	}
	defer colRows.Close()

	var writeCols []driver.WriteColumn
	colByName := make(map[string]driver.WriteColumn)
	// Explicit notNullMap (fail-closed): missing column defaults to false (NOT eligible as UNIQUE NOT NULL key).
	notNullMap := make(map[string]bool)

	for colRows.Next() {
		var (
			name, dataType, colType, isNullable, colKey, extra, collation, comment, genExpr string
			colDef                                                                          sql.NullString
			ordPos                                                                          int
		)
		if err := colRows.Scan(
			&name, &dataType, &colType, &isNullable, &colKey,
			&colDef, &extra, &collation, &comment, &genExpr, &ordPos,
		); err != nil {
			return driver.WriteSchema{}, err
		}

		nullable := !strings.EqualFold(isNullable, "NO")
		notNullMap[name] = !nullable

		wc := buildMySQLWriteColumn(c.detectedType, name, dataType, colType, nullable, strings.EqualFold(colKey, "PRI"), colDef, extra, collation, comment, genExpr, ordPos)
		writeCols = append(writeCols, wc)
		colByName[name] = wc
	}
	if err := colRows.Err(); err != nil {
		return driver.WriteSchema{}, classifyMySQLError(err, false)
	}
	if len(writeCols) == 0 {
		return driver.WriteSchema{}, dto.NewAppError(dto.ErrNotFound, fmt.Sprintf("无法读取表 %s.%s 的列定义", dbName, target.Table), "")
	}

	// 3. Fail-closed PRIMARY / UNIQUE index inspection using sql.NullString to safely skip functional/expression indexes.
	idxRows, err := q.QueryContext(ctx, `
		SELECT INDEX_NAME, COLUMN_NAME
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND NON_UNIQUE = 0
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`, dbName, target.Table)
	if err != nil {
		return driver.WriteSchema{}, classifyMySQLError(err, false)
	}
	defer idxRows.Close()

	type idxAccum struct {
		cols          []string
		hasExpression bool
	}
	uniqueIdx := make(map[string]*idxAccum)
	var idxNames []string
	for idxRows.Next() {
		var idxName string
		var colName sql.NullString
		if err := idxRows.Scan(&idxName, &colName); err != nil {
			return driver.WriteSchema{}, err
		}
		acc, exists := uniqueIdx[idxName]
		if !exists {
			acc = &idxAccum{}
			uniqueIdx[idxName] = acc
			idxNames = append(idxNames, idxName)
		}
		if !colName.Valid || colName.String == "" {
			acc.hasExpression = true
			continue
		}
		acc.cols = append(acc.cols, colName.String)
	}
	if err := idxRows.Err(); err != nil {
		return driver.WriteSchema{}, classifyMySQLError(err, false)
	}

	if pkAcc, ok := uniqueIdx["PRIMARY"]; ok && !pkAcc.hasExpression && len(pkAcc.cols) > 0 && allMySQLColsKeyComparable(pkAcc.cols, notNullMap, colByName) {
		edit.RowIdentity = dto.RowIdentityPrimaryKey
		edit.KeyColumns = pkAcc.cols
	} else {
		for _, idxName := range idxNames {
			if strings.EqualFold(idxName, "PRIMARY") {
				continue
			}
			acc := uniqueIdx[idxName]
			if acc.hasExpression || len(acc.cols) == 0 {
				continue
			}
			if allMySQLColsKeyComparable(acc.cols, notNullMap, colByName) {
				edit.RowIdentity = dto.RowIdentityUniqueNotNull
				edit.KeyColumns = acc.cols
				break
			}
		}
	}

	if edit.RowIdentity == dto.RowIdentityNone {
		if pkAcc, ok := uniqueIdx["PRIMARY"]; ok && len(pkAcc.cols) > 0 {
			edit.Reasons = append(edit.Reasons, "primary_key_not_comparable")
		} else {
			edit.Reasons = append(edit.Reasons, "no_stable_unique_key")
		}
	}

	return driver.WriteSchema{
		Dialect:       c.detectedType,
		Database:      dbName,
		Table:         target.Table,
		StorageEngine: engine,
		Editability:   edit,
		Columns:       writeCols,
	}, nil
}

func allMySQLColsKeyComparable(cols []string, notNullMap map[string]bool, colMap map[string]driver.WriteColumn) bool {
	if len(cols) == 0 {
		return false
	}
	for _, cn := range cols {
		if !notNullMap[cn] {
			return false
		}
		wc, ok := colMap[cn]
		if !ok || !wc.KeyComparable {
			return false
		}
	}
	return true
}

func buildMySQLWriteColumn(
	engine dto.EngineType,
	name, dataType, colType string,
	nullable, isPK bool,
	colDef sql.NullString,
	extra, collation, comment, genExpr string,
	ordPos int,
) driver.WriteColumn {
	extraLow := strings.ToLower(extra)
	genKind := ""
	if strings.Contains(extraLow, "virtual generated") {
		genKind = "virtual"
	} else if strings.Contains(extraLow, "stored generated") {
		genKind = "stored"
	}
	autoInc := strings.Contains(extraLow, "auto_increment")

	strategy, keyComparable, insertable, updatable, roReason := classifyMySQLType(engine, dataType, colType)

	if genKind != "" {
		insertable = false
		updatable = false
		roReason = "生成列由数据库自动计算，不可直接写入"
	}

	defVal := ""
	if colDef.Valid {
		defVal = colDef.String
	}

	return driver.WriteColumn{
		Detail: dto.ColumnDetail{
			Name:           name,
			DataType:       dataType,
			ColumnType:     colType,
			Collation:      collation,
			Nullable:       nullable,
			IsPrimaryKey:   isPK,
			DefaultValue:   defVal,
			HasDefault:     colDef.Valid,
			AutoIncrement:  autoInc,
			GeneratedKind:  genKind,
			Insertable:     insertable,
			Updatable:      updatable,
			ReadOnlyReason: roReason,
			Comment:        comment,
			OrdinalPos:     ordPos,
		},
		NativeType:           strings.ToLower(dataType),
		Collation:            collation,
		Deterministic:        true,
		GenerationExpression: genExpr,
		CompareStrategy:      strategy,
		KeyComparable:        keyComparable,
	}
}

func classifyMySQLType(engine dto.EngineType, dataType, colType string) (
	strategy driver.CompareStrategy,
	keyComparable bool,
	insertable bool,
	updatable bool,
	readOnlyReason string,
) {
	dt := strings.ToLower(strings.TrimSpace(dataType))
	ct := strings.ToLower(strings.TrimSpace(colType))

	// Check MariaDB JSON alias (stored as longtext with CHECK json_valid).
	if dt == "json" {
		if engine == dto.EngineMariaDB {
			return driver.CompareMySQLBinary, false, true, true, ""
		}
		return driver.CompareMySQLJSON, false, true, true, ""
	}

	switch dt {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		// ALL editable MySQL/MariaDB character columns use charset-normalized byte-wise comparison
		// (CAST(CONVERT(col USING utf8mb4) AS BINARY) = CAST(CONVERT(? USING utf8mb4) AS BINARY))
		// so non-utf8mb4 columns (e.g. latin1) match cleanly while still catching _ci and PAD SPACE conflicts.
		return driver.CompareMySQLBinary, true, true, true, ""
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric",
		"date", "datetime", "timestamp", "time", "year", "bool", "boolean":
		return driver.CompareExact, true, true, true, ""
	case "bit":
		// BIT values are binary-encoded in MySQL wire protocol; restrict inline editing in v1.
		return driver.CompareUnsupported, false, false, false, "BIT 列首版暂不支持行内编辑"
	case "float", "double", "real":
		return driver.CompareUnsupported, false, true, false, "浮点列首版暂不支持行内并发校验编辑，请通过 SQL 编辑器修改"
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return driver.CompareUnsupported, false, false, false, "二进制 BLOB 列暂不支持行内编辑"
	default:
		if strings.Contains(ct, "geometry") || strings.Contains(dt, "point") || strings.Contains(dt, "polygon") || strings.Contains(dt, "linestring") {
			return driver.CompareUnsupported, false, false, false, "空间几何列暂不支持行内编辑"
		}
		return driver.CompareUnsupported, false, true, false, "首版暂不支持该类型的行内并发校验编辑"
	}
}

// --- mysqlTableWriteTx ---

type mysqlTableWriteTx struct {
	conn            *Conn
	tx              *sql.Tx
	schema          driver.WriteSchema
	beginCtx        context.Context
	stopWatch       func() bool
	done            bool
	commitAttempted bool
}

func (t *mysqlTableWriteTx) beginCtxDone() bool {
	return t.beginCtx != nil && t.beginCtx.Err() != nil
}

func (t *mysqlTableWriteTx) stopTxWatch() {
	if t.stopWatch != nil {
		t.stopWatch()
	}
}

func (t *mysqlTableWriteTx) Schema() driver.WriteSchema { return t.schema }

func (t *mysqlTableWriteTx) Exec(ctx context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	t.conn.ResetWireTracker()
	res, err := t.tx.ExecContext(ctx, stmt.SQL, stmt.Args...)
	if err != nil {
		return driver.ExecFact{}, classifyMySQLError(err, true)
	}
	affected, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	if affected > 0 {
		if werr := t.checkDataLossWarnings(ctx); werr != nil {
			return driver.ExecFact{}, werr
		}
	}
	return driver.ExecFact{
		AffectedRows: affected,
		LastInsertID: lastID,
	}, nil
}

// checkDataLossWarnings inspects SHOW COUNT(*) WARNINGS (which does not clear diagnostics) and
// SHOW WARNINGS on the same transaction right after DML so non-strict sql_mode truncation
// (including sessions with max_error_count=0 where details are omitted) and strict-mode
// fractional DECIMAL scale truncation (Note 1265) are rejected before COMMIT.
func (t *mysqlTableWriteTx) checkDataLossWarnings(ctx context.Context) error {
	var warningCount int
	if err := t.tx.QueryRowContext(ctx, "SHOW COUNT(*) WARNINGS").Scan(&warningCount); err != nil {
		return classifyMySQLError(err, true)
	}
	if warningCount <= 0 {
		return nil
	}

	rows, err := t.tx.QueryContext(ctx, "SHOW WARNINGS")
	if err != nil {
		return classifyMySQLError(err, true)
	}
	defer rows.Close()

	seenRows := 0
	for rows.Next() {
		seenRows++
		var level string
		var code int
		var msg string
		if err := rows.Scan(&level, &code, &msg); err != nil {
			return err
		}
		if isMySQLDataLossWarning(level, code) {
			return dto.NewAppError(
				dto.ErrInvalidArgument,
				fmt.Sprintf("写入值发生截断或有损转换，已中止提交并回滚 (MySQL %d): %s", code, msg),
				fmt.Sprintf("SHOW WARNINGS %s %d: %s", level, code, msg),
			)
		}
	}
	if err := rows.Err(); err != nil {
		return classifyMySQLError(err, true)
	}
	if seenRows < warningCount {
		return dto.NewAppError(
			dto.ErrInvalidArgument,
			fmt.Sprintf("MySQL 产生了 %d 条写入告警，但仅返回了 %d 条诊断明细，无法确认未发生截断，已中止提交并回滚", warningCount, seenRows),
			fmt.Sprintf("SHOW COUNT(*) WARNINGS=%d, SHOW WARNINGS rows=%d", warningCount, seenRows),
		)
	}
	return nil
}

func isMySQLDataLossWarning(level string, code int) bool {
	if strings.EqualFold(level, "Error") {
		return true
	}
	if strings.EqualFold(level, "Note") {
		// MySQL 8.4 emits "Note 1265: Data truncated for column ..." even in strict mode when
		// silently rounding fractional DECIMAL values (e.g. inserting 2.5 into DECIMAL(10,0) -> 3).
		return code == 1265
	}
	if !strings.EqualFold(level, "Warning") {
		return false
	}
	switch code {
	case 1048, // ER_BAD_NULL_ERROR (NULL coerced to 0/empty in non-strict mode)
		1261, // ER_WARN_TOO_FEW_RECORDS
		1262, // ER_WARN_TOO_MANY_RECORDS
		1263, // ER_WARN_NULL_TO_NOTNULL
		1264, // ER_WARN_DATA_OUT_OF_RANGE
		1265, // WARN_DATA_TRUNCATED (overlength string, DECIMAL scale truncation, invalid ENUM/SET)
		1292, // ER_TRUNCATED_WRONG_VALUE (invalid date/time/numeric coerced)
		1299, // ER_INVALID_USE_OF_NULL
		1300, // ER_INVALID_CHARACTER_STRING
		1364, // ER_NO_DEFAULT_FOR_FIELD
		1365, // ER_DIVISION_BY_ZERO
		1366, // ER_TRUNCATED_WRONG_VALUE_FOR_FIELD (invalid charset/int coerced)
		1406, // ER_DATA_TOO_LONG
		1411, // ER_WRONG_VALUE_FOR_TYPE
		1690, // ER_DATA_OUT_OF_RANGE
		1918, // MariaDB out-of-range coercion warning
		3140, // ER_INVALID_JSON_TEXT
		3141, // ER_INVALID_JSON_TEXT_IN_PARAM
		3819, // ER_CHECK_CONSTRAINT_VIOLATED (MySQL 8+)
		4025: // ER_CONSTRAINT_FAILED (MariaDB)
		return true
	default:
		return false
	}
}

// watchConnDeadline binds ctx cancellation and deadlines to tracked's underlying net.Conn deadline
// so blocking database/sql *sql.Tx Commit/Rollback calls unblock promptly without racing Conn.Close().
func watchConnDeadline(ctx context.Context, tracked *driver.TrackedConn, keepExpired func() bool) func() {
	if tracked == nil || ctx == nil {
		return func() {}
	}
	if ctx.Err() != nil {
		_ = tracked.SetDeadline(time.Unix(1, 0))
		return func() {}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = tracked.SetDeadline(dl)
	}
	stopAfter := context.AfterFunc(ctx, func() {
		_ = tracked.SetDeadline(time.Unix(1, 0))
	})
	return func() {
		if stopAfter() && ctx.Err() == nil && (keepExpired == nil || !keepExpired()) {
			_ = tracked.SetDeadline(time.Time{})
		}
	}
}

func (t *mysqlTableWriteTx) Rollback(ctx context.Context) error {
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
	stop := watchConnDeadline(ctx, t.conn.tracked, t.beginCtxDone)
	err := t.tx.Rollback()
	stop()
	if err == nil || errors.Is(err, sql.ErrTxDone) {
		t.done = true
		t.stopTxWatch()
		return nil
	}
	if t.beginCtxDone() && t.conn.tracked != nil {
		_ = t.conn.tracked.SetDeadline(time.Unix(1, 0))
	}
	return err
}

func (t *mysqlTableWriteTx) Commit(ctx context.Context) driver.CommitResult {
	if t.done || t.commitAttempted {
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     dto.NewAppError(dto.ErrTxAborted, "事务已结束，无法重复提交", ""),
		}
	}
	t.commitAttempted = true
	rollbackCleanup := func() error {
		rbCtx, rbCancel := context.WithTimeout(context.Background(), mysqlRollbackCleanupTimeout)
		rbErr := t.Rollback(rbCtx)
		rbCancel()
		if rbErr == nil {
			t.done = true
			t.stopTxWatch()
		}
		return rbErr
	}
	if ctx != nil && ctx.Err() != nil {
		// Context already canceled BEFORE any COMMIT byte reached the wire -> roll back using an
		// independent bounded context so locks are released promptly, then return NotCommitted.
		// Only mark t.done = true when cleanup succeeds so a failed cleanup remains retryable via Rollback.
		rbErr := rollbackCleanup()
		appErr := classifyMySQLError(ctx.Err(), true)
		if rbErr != nil && appErr != nil {
			appErr.Detail = fmt.Sprintf("%s; cleanup rollback failed: %v", appErr.Detail, rbErr)
		}
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     appErr,
		}
	}
	t.conn.ResetWireTracker()
	stop := watchConnDeadline(ctx, t.conn.tracked, t.beginCtxDone)
	err := t.tx.Commit()
	stop()

	// 1. sql.ErrTxDone means database/sql rejected Commit() BEFORE entering driver txi.Commit()
	//    (tx.awaitDone() already flipped tx.done and initiated auto-ROLLBACK); any wire bytes
	//    belong to auto-ROLLBACK, never to COMMIT.
	if errors.Is(err, sql.ErrTxDone) {
		t.done = true
		t.stopTxWatch()
		return ClassifyMySQLCommitError(err, false)
	}

	// Capture WriteSent() BEFORE any cleanup rollback so cleanup bytes are never counted as COMMIT.
	writeSent := t.conn.tracked != nil && t.conn.tracked.WriteSent()

	// 2. If Commit() returned context.Canceled / context.DeadlineExceeded with ZERO bytes written
	//    (database/sql's select on tx.ctx.Done() short-circuited before entering driver txi.Commit()),
	//    clean up the open transaction via bounded rollback and return NotCommitted.
	//    If writeSent == true, bytes reached the wire and we cannot prove they were not COMMIT ->
	//    hand directly to ClassifyMySQLCommitError which conservatively returns CommitOutcomeUnknown!
	if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && !writeSent {
		rbErr := rollbackCleanup()
		res := ClassifyMySQLCommitError(err, false)
		if rbErr != nil && res.Err != nil {
			res.Err.Detail = fmt.Sprintf("%s; cleanup rollback failed: %v", res.Err.Detail, rbErr)
		}
		return res
	}

	t.done = true
	t.stopTxWatch()
	return ClassifyMySQLCommitError(err, writeSent)
}

// ClassifyMySQLCommitError implements the strict whitelist-based tri-state commit classification for MySQL/MariaDB.
// Exposed for direct unit testing (including sql.ErrTxDone -> NotCommitted and 40003 -> Unknown).
func ClassifyMySQLCommitError(err error, writeSent bool) driver.CommitResult {
	if err == nil {
		return driver.CommitResult{Outcome: driver.CommitOutcomeCommitted}
	}

	// 1. Go's database/sql returns sql.ErrTxDone from tx.Commit() ONLY when tx.done was already
	//    flipped BEFORE entering driver txi.Commit() (here, exclusively via tx.awaitDone() auto-rollback
	//    on context cancellation). Any wire bytes observed in that race came from ROLLBACK, not COMMIT.
	if errors.Is(err, sql.ErrTxDone) {
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     dto.NewAppError(dto.ErrTxAborted, "事务在提交前已被取消并回滚，未提交", err.Error()),
		}
	}

	appErr := classifyMySQLError(err, true)

	// 2. Zero bytes reached the wire before failure.
	if !writeSent {
		return driver.CommitResult{Outcome: driver.CommitOutcomeNotCommitted, Err: appErr}
	}

	// 3. Explicit MySQL/MariaDB server error: ONLY whitelisted rejection codes/SQLSTATEs are NotCommitted.
	//    40003 (statement_completion_unknown), shutdown/kill/lost-connection/replication-unknown codes
	//    MUST fall through to CommitOutcomeUnknown.
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) && isMySQLCommitDefinitiveNotCommitted(me) {
		return driver.CommitResult{Outcome: driver.CommitOutcomeNotCommitted, Err: appErr}
	}

	detail := err.Error()
	sqlState := ""
	if me != nil {
		sqlState = string(me.SQLState[:])
		detail = fmt.Sprintf("MySQL %d (%s): %s", me.Number, sqlState, me.Message)
	}
	return driver.CommitResult{
		Outcome: driver.CommitOutcomeUnknown,
		Err: &dto.AppError{
			Code:      dto.ErrCommitOutcomeUnknown,
			Message:   "提交请求已发出但未收到完整确认，请先核对目标表数据状态，切勿盲目重试",
			Detail:    detail,
			SQLState:  sqlState,
			RetrySafe: false,
		},
	}
}

func isMySQLCommitDefinitiveNotCommitted(me *mysqldriver.MySQLError) bool {
	if me == nil {
		return false
	}
	sqlState := string(me.SQLState[:])

	// Explicitly treat 40003 (statement_completion_unknown) and connection/shutdown/kill/GR-unknown codes as Unknown.
	if sqlState == "40003" || strings.HasPrefix(sqlState, "08") {
		return false
	}
	switch me.Number {
	case 1053, // ER_SERVER_SHUTDOWN
		1317, // ER_QUERY_INTERRUPTED (during COMMIT, completion uncertain)
		1927, // ER_CONNECTION_KILLED
		2006, // CR_SERVER_GONE_ERROR
		2013, // CR_SERVER_LOST
		3100, // ER_RUN_HOOK_ERROR (Group Replication commit uncertain)
		3101: // ER_TRANSACTION_ROLLBACK_DURING_COMMIT / plugin uncertain
		return false
	}

	// Concrete whitelisted server rejection error numbers.
	switch me.Number {
	case 1048, // ER_BAD_NULL_ERROR
		1062, // ER_DUP_ENTRY
		1142, // ER_TABLEACCESS_DENIED_ERROR
		1143, // ER_COLUMNACCESS_DENIED_ERROR
		1205, // ER_LOCK_WAIT_TIMEOUT
		1213, // ER_LOCK_DEADLOCK
		1216, // ER_NO_REFERENCED_ROW
		1217, // ER_ROW_IS_REFERENCED
		1227, // ER_SPECIFIC_ACCESS_DENIED_ERROR
		1264, // ER_WARN_DATA_OUT_OF_RANGE
		1265, // WARN_DATA_TRUNCATED
		1290, // ER_OPTION_PREVENTS_STATEMENT (read_only)
		1292, // ER_TRUNCATED_WRONG_VALUE
		1364, // ER_NO_DEFAULT_FOR_FIELD
		1366, // ER_TRUNCATED_WRONG_VALUE_FOR_FIELD
		1406, // ER_DATA_TOO_LONG
		1451, // ER_ROW_IS_REFERENCED_2
		1452, // ER_NO_REFERENCED_ROW_2
		1644, // ER_SIGNAL_EXCEPTION (trigger SIGNAL)
		1690, // ER_DATA_OUT_OF_RANGE
		1792, // ER_CANT_EXECUTE_IN_READ_ONLY_TRANSACTION
		3819, // ER_CHECK_CONSTRAINT_VIOLATED (MySQL 8+)
		4025: // ER_CONSTRAINT_FAILED (MariaDB)
		return true
	}

	// Concrete whitelisted SQLSTATE codes/classes (never matching 40003).
	switch sqlState {
	case "40000", "40001", "40002":
		return true
	}
	if len(sqlState) >= 2 {
		switch sqlState[:2] {
		case "22", "23", "25", "42", "44":
			return true
		}
	}
	return false
}
