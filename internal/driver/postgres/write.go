package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

const pgRollbackCleanupTimeout = 3 * time.Second

type pgQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// InspectWriteSchema reads the authoritative WriteSchema for PreviewGridChanges.
func (c *Conn) InspectWriteSchema(ctx context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.WriteSchema, error) {
	return c.inspectWriteSchema(ctx, c.conn, target, clientReadOnly)
}

// BeginTableWrite starts a PostgreSQL transaction, acquires an ACCESS SHARE lock on the target table
// to prevent concurrent ALTER/DROP TABLE, and reads the authoritative WriteSchema inside that transaction.
func (c *Conn) BeginTableWrite(ctx context.Context, target driver.WriteTarget, clientReadOnly bool) (driver.TableWriteTx, error) {
	schemaName := target.Schema
	if schemaName == "" {
		schemaName = "public"
	}
	if strings.TrimSpace(target.Table) == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "表名不能为空", "")
	}

	tx, err := c.conn.Begin(ctx)
	if err != nil {
		return nil, c.classifyError(err)
	}

	rollbackOnErr := func(cause error) (driver.TableWriteTx, error) {
		rbCtx, rbCancel := context.WithTimeout(context.Background(), pgRollbackCleanupTimeout)
		_ = tx.Rollback(rbCtx)
		rbCancel()
		return nil, cause
	}

	// Acquire transaction-scoped ACCESS SHARE table lock before reading metadata so concurrent DDL cannot
	// alter column types, keys, or drop the table between schema verification and row writes.
	lockSQL := fmt.Sprintf("SELECT 1 FROM %s.%s WHERE false", QuoteIdent(schemaName), QuoteIdent(target.Table))
	rows, err := tx.Query(ctx, lockSQL)
	if err != nil {
		return rollbackOnErr(c.classifyError(err))
	}
	rows.Close()
	if err := c.rowsError(rows); err != nil {
		return rollbackOnErr(err)
	}

	ws, err := c.inspectWriteSchema(ctx, tx, driver.WriteTarget{
		Database: target.Database,
		Schema:   schemaName,
		Table:    target.Table,
	}, clientReadOnly)
	if err != nil {
		return rollbackOnErr(err)
	}

	return &pgTableWriteTx{
		conn:   c,
		tx:     tx,
		schema: ws,
	}, nil
}

func (c *Conn) inspectWriteSchema(ctx context.Context, q pgQuerier, target driver.WriteTarget, clientReadOnly bool) (driver.WriteSchema, error) {
	schemaName := target.Schema
	if schemaName == "" {
		schemaName = "public"
	}
	dbName := target.Database
	if dbName == "" {
		c.mu.Lock()
		dbName = c.currentDB
		c.mu.Unlock()
	}

	// Verify target relation exists and inspect its relkind (fail-closed).
	var relKind string
	err := q.QueryRow(ctx, `
		SELECT c.relkind::text
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`,
		pgx.QueryExecModeExec, schemaName, target.Table).Scan(&relKind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return driver.WriteSchema{}, dto.NewAppError(dto.ErrNotFound, fmt.Sprintf("表 %s.%s 不存在", schemaName, target.Table), "")
		}
		return driver.WriteSchema{}, c.classifyError(err)
	}

	isBaseTable := relKind == "r" || relKind == "p"

	pkCols, err := c.loadPrimaryKeyColumnsWith(ctx, q, schemaName, target.Table)
	if err != nil {
		return driver.WriteSchema{}, err
	}
	pkSet := make(map[string]bool, len(pkCols))
	for _, k := range pkCols {
		pkSet[k] = true
	}

	// Query column definitions including generated/identity flags, collation, and collisdeterministic.
	rows, err := q.Query(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE(pg_get_expr(ad.adbin, ad.adrelid), ''),
		       ad.adbin IS NOT NULL,
		       COALESCE(col_description(c.oid, a.attnum), ''),
		       a.attnum,
		       COALESCE(a.attgenerated::text, ''),
		       COALESCE(a.attidentity::text, ''),
		       COALESCE(coll.collname, ''),
		       COALESCE(coll.collisdeterministic, true),
		       t.typtype::text,
		       t.typname
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		LEFT JOIN pg_collation coll ON coll.oid = a.attcollation
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, pgx.QueryExecModeExec, schemaName, target.Table)
	if err != nil {
		return driver.WriteSchema{}, c.classifyError(err)
	}
	defer rows.Close()

	var writeCols []driver.WriteColumn
	colByName := make(map[string]driver.WriteColumn)
	for rows.Next() {
		var (
			name, formatType, defaultVal, comment string
			nullable, hasDefault, deterministic   bool
			attNum                                int
			attGenerated, attIdentity, collName   string
			typType, typName                      string
		)
		if err := rows.Scan(
			&name, &formatType, &nullable, &defaultVal, &hasDefault, &comment, &attNum,
			&attGenerated, &attIdentity, &collName, &deterministic, &typType, &typName,
		); err != nil {
			return driver.WriteSchema{}, err
		}

		wc := buildPGWriteColumn(name, formatType, nullable, pkSet[name], defaultVal, hasDefault, comment, attNum, attGenerated, attIdentity, collName, deterministic, typType, typName)
		writeCols = append(writeCols, wc)
		colByName[name] = wc
	}
	if err := c.rowsError(rows); err != nil {
		return driver.WriteSchema{}, err
	}

	edit := dto.TableEditability{
		CanWrite:           !clientReadOnly && !c.readOnly && isBaseTable,
		RowIdentity:        dto.RowIdentityNone,
		BatchTransactional: isBaseTable,
		StorageEngine:      "heap",
	}
	if clientReadOnly || c.readOnly {
		edit.Reasons = append(edit.Reasons, "read_only_connection")
	}
	if !isBaseTable {
		edit.Reasons = append(edit.Reasons, "non_base_table:"+relKind)
	}

	// 1. Check primary key (excluding INCLUDE columns via indnkeyatts; verify all key columns are comparable).
	if len(pkCols) > 0 && allPGColsKeyComparable(pkCols, colByName) {
		edit.RowIdentity = dto.RowIdentityPrimaryKey
		edit.KeyColumns = pkCols
	} else {
		// 2. Check non-partial, non-expression UNIQUE indexes covering only NOT NULL & key-comparable columns.
		uniqueCandidates, uerr := c.loadUniqueNotNullIndexesWith(ctx, q, schemaName, target.Table)
		if uerr != nil {
			return driver.WriteSchema{}, uerr
		}
		for _, cand := range uniqueCandidates {
			if allPGColsKeyComparable(cand, colByName) {
				edit.RowIdentity = dto.RowIdentityUniqueNotNull
				edit.KeyColumns = cand
				break
			}
		}
	}

	if edit.RowIdentity == dto.RowIdentityNone {
		if len(pkCols) > 0 {
			edit.Reasons = append(edit.Reasons, "primary_key_not_comparable")
		} else {
			edit.Reasons = append(edit.Reasons, "no_stable_unique_key")
		}
	}

	return driver.WriteSchema{
		Dialect:       dto.EnginePostgres,
		Database:      dbName,
		Schema:        schemaName,
		Table:         target.Table,
		StorageEngine: "heap",
		Editability:   edit,
		Columns:       writeCols,
	}, nil
}

func allPGColsKeyComparable(cols []string, colMap map[string]driver.WriteColumn) bool {
	if len(cols) == 0 {
		return false
	}
	for _, name := range cols {
		wc, ok := colMap[name]
		if !ok || !wc.KeyComparable || wc.Detail.Nullable {
			return false
		}
	}
	return true
}

func buildPGWriteColumn(
	name, formatType string,
	nullable, isPK bool,
	defaultVal string,
	hasDefault bool,
	comment string,
	attNum int,
	attGenerated, attIdentity, collName string,
	deterministic bool,
	typType, typName string,
) driver.WriteColumn {
	genKind := ""
	if attGenerated == "s" {
		genKind = "stored"
	}
	identKind := ""
	autoInc := strings.Contains(strings.ToLower(defaultVal), "nextval(")
	switch attIdentity {
	case "a":
		identKind = "always"
		autoInc = true
	case "d":
		identKind = "by_default"
		autoInc = true
	}

	strategy, keyComparable, insertable, updatable, roReason := classifyPGType(formatType, typType, typName, deterministic)

	if genKind != "" {
		insertable = false
		updatable = false
		roReason = "生成列由数据库自动计算，不可直接写入"
	} else if identKind == "always" {
		insertable = false
		updatable = false
		roReason = "GENERATED ALWAYS AS IDENTITY 列由数据库自动生成，不可直接写入"
	}

	return driver.WriteColumn{
		Detail: dto.ColumnDetail{
			Name:           name,
			DataType:       formatType,
			ColumnType:     formatType,
			Collation:      collName,
			Nullable:       nullable,
			IsPrimaryKey:   isPK,
			DefaultValue:   defaultVal,
			HasDefault:     hasDefault,
			AutoIncrement:  autoInc,
			GeneratedKind:  genKind,
			IdentityKind:   identKind,
			Insertable:     insertable,
			Updatable:      updatable,
			ReadOnlyReason: roReason,
			Comment:        comment,
			OrdinalPos:     attNum,
		},
		NativeType:      formatType,
		Collation:       collName,
		Deterministic:   deterministic,
		CompareStrategy: strategy,
		KeyComparable:   keyComparable,
	}
}

func classifyPGType(formatType, typType, typName string, deterministic bool) (
	strategy driver.CompareStrategy,
	keyComparable bool,
	insertable bool,
	updatable bool,
	readOnlyReason string,
) {
	lowType := strings.ToLower(strings.TrimSpace(typName))
	lowFmt := strings.ToLower(strings.TrimSpace(formatType))

	// Arrays, composites, ranges, multiranges: unsupported in v1 inline editing.
	if strings.HasPrefix(lowType, "_") || strings.HasSuffix(lowFmt, "[]") ||
		typType == "c" || typType == "r" || typType == "m" {
		return driver.CompareUnsupported, false, true, false, "首版暂不支持复合/数组/范围类型的行内并发校验编辑"
	}

	// Enums support exact equality and stable keys.
	if typType == "e" {
		return driver.CompareExact, true, true, true, ""
	}

	switch lowType {
	case "bytea":
		return driver.CompareUnsupported, false, false, false, "二进制 BYTEA 列暂不支持行内编辑"
	case "float4", "float8":
		return driver.CompareUnsupported, false, true, false, "浮点列首版暂不支持行内并发校验编辑，请通过 SQL 编辑器修改"
	case "xml", "point", "line", "lseg", "box", "path", "polygon", "circle", "tsvector", "tsquery", "pg_lsn", "pg_snapshot", "txid_snapshot":
		return driver.CompareUnsupported, false, true, false, "首版暂不支持该类型的行内并发校验编辑"
	case "json":
		// PostgreSQL json has no '=' operator; compare raw stored text via col::text = $n.
		return driver.ComparePGJSONText, false, true, true, ""
	case "jsonb":
		return driver.ComparePGJSONB, true, true, true, ""
	case "citext":
		// citext is case-insensitive even when collisdeterministic is true; compare with COLLATE "C".
		return driver.ComparePGCCollation, true, true, true, ""
	case "text", "varchar", "bpchar", "char", "name":
		if !deterministic {
			return driver.ComparePGCCollation, true, true, true, ""
		}
		return driver.CompareExact, true, true, true, ""
	case "int2", "int4", "int8", "numeric", "bool", "uuid",
		"date", "time", "timetz", "timestamp", "timestamptz", "interval",
		"inet", "cidr", "macaddr", "macaddr8", "bit", "varbit", "money", "oid":
		return driver.CompareExact, true, true, true, ""
	default:
		// Conservative fallback for custom/domain/extension types.
		if typType == "d" {
			return driver.CompareUnsupported, false, true, false, "首版暂不支持自定义 Domain 类型的行内并发校验编辑"
		}
		return driver.CompareUnsupported, false, true, false, "首版暂不支持该扩展类型的行内并发校验编辑"
	}
}

func (c *Conn) loadPrimaryKeyColumnsWith(ctx context.Context, q pgQuerier, schema, table string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT a.attname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON k.ord <= i.indnkeyatts
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum AND a.attnum > 0 AND NOT a.attisdropped
		WHERE n.nspname = $1 AND c.relname = $2
		  AND i.indisprimary AND i.indisvalid AND i.indisready
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

func (c *Conn) loadUniqueNotNullIndexesWith(ctx context.Context, q pgQuerier, schema, table string) ([][]string, error) {
	rows, err := q.Query(ctx, `
		SELECT i.indexrelid,
		       k.attnum,
		       COALESCE(a.attname, ''),
		       COALESCE(a.attnotnull, false)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON k.ord <= i.indnkeyatts
		LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum AND a.attnum > 0 AND NOT a.attisdropped
		WHERE n.nspname = $1 AND c.relname = $2
		  AND i.indisunique AND NOT i.indisprimary
		  AND i.indisvalid AND i.indisready
		  AND i.indpred IS NULL
		ORDER BY i.indexrelid, k.ord`, pgx.QueryExecModeExec, schema, table)
	if err != nil {
		return nil, c.classifyError(err)
	}
	defer rows.Close()

	type idxAccum struct {
		cols  []string
		valid bool
	}
	var order []uint32
	byID := make(map[uint32]*idxAccum)
	for rows.Next() {
		var idxOID uint32
		var attNum int
		var colName string
		var notNull bool
		if err := rows.Scan(&idxOID, &attNum, &colName, &notNull); err != nil {
			return nil, err
		}
		acc, ok := byID[idxOID]
		if !ok {
			acc = &idxAccum{valid: true}
			byID[idxOID] = acc
			order = append(order, idxOID)
		}
		// attnum == 0 means an expression index column; nullable column disqualifies unique-not-null key.
		if attNum <= 0 || colName == "" || !notNull {
			acc.valid = false
			continue
		}
		acc.cols = append(acc.cols, colName)
	}
	if err := c.rowsError(rows); err != nil {
		return nil, err
	}

	var out [][]string
	for _, oid := range order {
		acc := byID[oid]
		if acc.valid && len(acc.cols) > 0 {
			out = append(out, acc.cols)
		}
	}
	return out, nil
}

// --- pgTableWriteTx ---

type pgTableWriteTx struct {
	conn            *Conn
	tx              pgx.Tx
	schema          driver.WriteSchema
	done            bool
	commitAttempted bool
}

func (t *pgTableWriteTx) Schema() driver.WriteSchema { return t.schema }

func (t *pgTableWriteTx) Exec(ctx context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	t.conn.ResetWireTracker()
	var (
		tag pgconn.CommandTag
		err error
	)
	if len(stmt.Args) > 0 {
		args := make([]any, 0, len(stmt.Args)+1)
		args = append(args, pgx.QueryExecModeExec)
		args = append(args, stmt.Args...)
		tag, err = t.tx.Exec(ctx, stmt.SQL, args...)
	} else {
		tag, err = t.tx.Exec(ctx, stmt.SQL)
	}
	if err != nil {
		return driver.ExecFact{}, t.conn.classifyError(err)
	}
	return driver.ExecFact{AffectedRows: tag.RowsAffected()}, nil
}

func (t *pgTableWriteTx) Rollback(ctx context.Context) error {
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
	err := t.tx.Rollback(ctx)
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		t.done = true
		return nil
	}
	return err
}

func (t *pgTableWriteTx) Commit(ctx context.Context) driver.CommitResult {
	if t.done || t.commitAttempted {
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     dto.NewAppError(dto.ErrTxAborted, "事务已结束，无法重复提交", ""),
		}
	}
	t.commitAttempted = true
	if ctx != nil && ctx.Err() != nil {
		// Context already canceled BEFORE COMMIT was sent -> roll back with an independent bounded
		// context so the ACCESS SHARE lock is released promptly, then return NotCommitted.
		// Only mark t.done = true via t.Rollback when cleanup succeeds so a failed cleanup stays retryable.
		rbCtx, rbCancel := context.WithTimeout(context.Background(), pgRollbackCleanupTimeout)
		rbErr := t.Rollback(rbCtx)
		rbCancel()
		appErr := t.conn.classifyError(ctx.Err())
		if rbErr != nil && appErr != nil {
			appErr.Detail = fmt.Sprintf("%s; cleanup rollback failed: %v", appErr.Detail, rbErr)
		}
		return driver.CommitResult{
			Outcome: driver.CommitOutcomeNotCommitted,
			Err:     appErr,
		}
	}
	t.done = true
	t.conn.ResetWireTracker()
	err := t.tx.Commit(ctx)
	writeSent := t.conn.tracked != nil && t.conn.tracked.WriteSent()
	return ClassifyPGCommitError(err, writeSent, t.conn.classifyError)
}

// ClassifyPGCommitError implements the strict whitelist-based tri-state commit classification for PostgreSQL.
// Exposed for direct unit testing across SQLSTATE classes (including 40003 -> Unknown and pgx.ErrTxCommitRollback -> NotCommitted).
func ClassifyPGCommitError(err error, writeSent bool, classify func(error) *dto.AppError) driver.CommitResult {
	if err == nil {
		return driver.CommitResult{Outcome: driver.CommitOutcomeCommitted}
	}

	appErr := classify(err)

	// 1. pgx explicitly observed a ROLLBACK response from PostgreSQL.
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		if appErr == nil {
			appErr = dto.NewAppError(dto.ErrTxAborted, "事务已被 PostgreSQL 回滚，未提交", err.Error())
		}
		return driver.CommitResult{Outcome: driver.CommitOutcomeNotCommitted, Err: appErr}
	}

	// 2. Zero bytes reached the wire before failure (e.g. context already canceled before send).
	if !writeSent {
		return driver.CommitResult{Outcome: driver.CommitOutcomeNotCommitted, Err: appErr}
	}

	// 3. Explicit PostgreSQL server protocol error: ONLY whitelisted rejection SQLSTATEs are NotCommitted.
	//    Critically, 40003 (statement_completion_unknown), 08* (connection), 57P01-03 (shutdown), XX* (internal)
	//    MUST fall through to CommitOutcomeUnknown!
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && isPGCommitDefinitiveNotCommitted(pgErr.Code) {
		return driver.CommitResult{Outcome: driver.CommitOutcomeNotCommitted, Err: appErr}
	}

	// 4. Conservative fail-safe: COMMIT bytes were sent and server completion is unconfirmed.
	detail := err.Error()
	sqlState := ""
	if pgErr != nil {
		sqlState = pgErr.Code
		detail = fmt.Sprintf("SQLSTATE %s: %s", pgErr.Code, pgErr.Message)
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

func isPGCommitDefinitiveNotCommitted(code string) bool {
	if len(code) < 2 {
		return false
	}
	// Explicitly exclude 40003 (statement_completion_unknown) by whitelisting concrete Class 40 rollback codes.
	switch code {
	case "40000", "40001", "40002", "40P01":
		return true
	case "40003":
		return false
	}
	switch code[:2] {
	case "22", // data_exception
		"23", // integrity_constraint_violation (incl. deferred constraints on COMMIT)
		"25", // invalid_transaction_state
		"42", // syntax_error_or_access_rule_violation
		"44", // with_check_option_violation
		"53", // insufficient_resources
		"55": // object_not_in_prerequisite_state
		return true
	}
	return false
}
