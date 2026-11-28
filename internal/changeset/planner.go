package changeset

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

// BuildChangePlan validates a grid edit request against the authoritative WriteSchema and
// builds an immutable, parameterized ChangePlan in fixed order: Deletes -> Updates -> Inserts.
//
// Enforced contracts:
//   - Three-dimensional editability guards (CanWrite, BatchTransactional, RowIdentity)
//   - Stable-key completeness (no omitted/NULL/truncated/binary/non-comparable key column)
//   - Column Insertable / Updatable guards (generated columns, IDENTITY ALWAYS, unsupported types)
//   - No-op cell & row stripping (restored cells omitted; unchanged rows emit 0 UPDATE statements)
//   - NULL original predicates ALWAYS generate "<col> IS NULL" (never "= NULL" or "= ?")
//   - Primary-key updates target original key values in WHERE without duplicate predicates
func BuildChangePlan(
	source SourceRef,
	ws driver.WriteSchema,
	req dto.PreviewGridChangesRequest,
) (*ChangePlan, []dto.ChangeStepPreview, error) {
	// 1. Three-dimensional editability guards.
	if !ws.Editability.CanWrite {
		return nil, nil, dto.NewAppError(
			dto.ErrReadOnlyDenied,
			"当前连接处于只读模式或目标对象不可写入，已拒绝生成变更计划",
			strings.Join(ws.Editability.Reasons, "; "),
		)
	}
	if !ws.Editability.BatchTransactional {
		return nil, nil, dto.NewAppError(
			dto.ErrBatchNonTx,
			fmt.Sprintf("目标表存储引擎 (%s) 不支持整批原子事务回滚，已禁止网格写入", ws.StorageEngine),
			strings.Join(ws.Editability.Reasons, "; "),
		)
	}

	hasRowTargetingOps := len(req.Deletes) > 0 || len(req.Updates) > 0
	if hasRowTargetingOps && (ws.Editability.RowIdentity == dto.RowIdentityNone || len(ws.Editability.KeyColumns) == 0) {
		return nil, nil, dto.NewAppError(
			dto.ErrRowNotIdentifiable,
			"目标表缺少可靠的非空唯一稳定键，仅允许新增行，禁止修改或删除已有行",
			strings.Join(ws.Editability.Reasons, "; "),
		)
	}

	// Deterministic ordered column list and lookup map.
	orderedCols := make([]driver.WriteColumn, len(ws.Columns))
	copy(orderedCols, ws.Columns)
	sort.Slice(orderedCols, func(i, j int) bool {
		if orderedCols[i].Detail.OrdinalPos != orderedCols[j].Detail.OrdinalPos {
			return orderedCols[i].Detail.OrdinalPos < orderedCols[j].Detail.OrdinalPos
		}
		return orderedCols[i].Detail.Name < orderedCols[j].Detail.Name
	})

	colMap := make(map[string]driver.WriteColumn, len(orderedCols))
	for _, c := range orderedCols {
		colMap[c.Detail.Name] = c
	}

	keyCols := ws.Editability.KeyColumns
	keySet := make(map[string]bool, len(keyCols))
	for _, k := range keyCols {
		if _, ok := colMap[k]; !ok {
			return nil, nil, dto.NewAppError(dto.ErrRowNotIdentifiable, fmt.Sprintf("稳定键列 %s 不存在于表结构中", k), "")
		}
		keySet[k] = true
	}

	tableRef := qualifyTable(ws)
	var steps []ChangeStep
	deleteCount := 0
	updateCount := 0
	insertCount := 0

	// Helper to validate and build WHERE predicates for all stable key columns.
	buildKeyPredicates := func(keyValues map[string]dto.OriginalCellValue, nextParamIdx *int) ([]string, []string, []any, error) {
		for colName := range keyValues {
			if _, ok := colMap[colName]; !ok {
				return nil, nil, nil, dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("稳定键包含未知列: %s", colName), "")
			}
		}
		var sqlParts []string
		var previewParts []string
		var args []any
		for _, k := range keyCols {
			orig, ok := keyValues[k]
			if !ok {
				return nil, nil, nil, dto.NewAppError(
					dto.ErrRowNotIdentifiable,
					fmt.Sprintf("缺少稳定键列 %s 的完整原值，禁止修改或删除该行", k),
					"",
				)
			}
			pred, err := buildComparePredicate(ws.Dialect, colMap[k], orig, true, nextParamIdx)
			if err != nil {
				return nil, nil, nil, err
			}
			sqlParts = append(sqlParts, pred.SQL)
			previewParts = append(previewParts, pred.PreviewSQL)
			args = append(args, pred.Args...)
		}
		return sqlParts, previewParts, args, nil
	}

	// --- Phase 1: DELETES ---
	for i, del := range req.Deletes {
		rowKey := del.RowKey
		if rowKey == "" {
			rowKey = fmt.Sprintf("delete#%d", i)
		}
		paramIdx := 0
		whereSQL, wherePreview, args, err := buildKeyPredicates(del.KeyValues, &paramIdx)
		if err != nil {
			return nil, nil, err
		}
		sqlStr := fmt.Sprintf("DELETE FROM %s WHERE %s", tableRef, strings.Join(whereSQL, " AND "))
		prevStr := fmt.Sprintf("DELETE FROM %s WHERE %s", tableRef, strings.Join(wherePreview, " AND "))
		steps = append(steps, ChangeStep{
			Index:  len(steps),
			Op:     OpDelete,
			RowKey: rowKey,
			Stmt: driver.BoundStatement{
				SQL:  sqlStr,
				Args: args,
			},
			PreviewSQL: prevStr,
			Params:     formatParams(args),
		})
		deleteCount++
	}

	// --- Phase 2: UPDATES (with strict No-op stripping, NULL IS NULL, PK-update WHERE preservation) ---
	for i, upd := range req.Updates {
		rowKey := upd.RowKey
		if rowKey == "" {
			rowKey = fmt.Sprintf("update#%d", i)
		}

		// Validate all referenced column names exist in schema.
		for colName := range upd.NewValues {
			if _, ok := colMap[colName]; !ok {
				return nil, nil, dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("更新请求包含未知列: %s", colName), "")
			}
		}
		for colName := range upd.OriginalValues {
			if _, ok := colMap[colName]; !ok {
				return nil, nil, dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("原值校验包含未知列: %s", colName), "")
			}
		}

		// Validate stable keys upfront even if row ends up being stripped?
		// Wait: check stable keys on every requested update row so malformed/truncated key rows are rejected loudly.
		dummyParamIdx := 0
		if _, _, _, err := buildKeyPredicates(upd.KeyValues, &dummyParamIdx); err != nil {
			return nil, nil, err
		}

		type pendingSet struct {
			col      driver.WriteColumn
			boundVal any
			origVal  dto.OriginalCellValue
		}
		var effectiveSets []pendingSet

		for _, col := range orderedCols {
			newVal, hasNew := upd.NewValues[col.Detail.Name]
			if !hasNew {
				continue
			}

			// Resolve original value (allowing PK columns to come from KeyValues, rejecting mismatches).
			origVal, hasOrig := upd.OriginalValues[col.Detail.Name]
			if keyVal, hasKey := upd.KeyValues[col.Detail.Name]; hasKey {
				if hasOrig && origVal != keyVal {
					return nil, nil, dto.NewAppError(
						dto.ErrInvalidArgument,
						fmt.Sprintf("主键列 %s 在 keyValues 与 originalValues 中的原值不一致", col.Detail.Name),
						"",
					)
				}
				if !hasOrig {
					origVal = keyVal
					hasOrig = true
				}
			}
			if !hasOrig {
				return nil, nil, dto.NewAppError(
					dto.ErrInvalidArgument,
					fmt.Sprintf("列 %s 缺少修改前的原值，无法生成并发安全校验条件", col.Detail.Name),
					"",
				)
			}

			// Validate input mode before checking updatability / no-op.
			if newVal.Mode == dto.CellInputDefault {
				return nil, nil, dto.NewAppError(
					dto.ErrInvalidArgument,
					fmt.Sprintf("UPDATE 不支持对列 %s 使用 default 模式", col.Detail.Name),
					"",
				)
			}
			if !col.Detail.Updatable {
				reason := col.Detail.ReadOnlyReason
				if reason == "" {
					reason = fmt.Sprintf("列 %s 不可修改", col.Detail.Name)
				}
				return nil, nil, dto.NewAppError(dto.ErrInvalidArgument, reason, col.Detail.Name)
			}

			// Allow restoring an already-stored SQLite REAL value (e.g. 0.1) back to its unchanged text
			// to strip cleanly as a no-op without triggering strict new-value SetFloat64 precision errors.
			if isExistingSQLiteRealNoop(ws.Dialect, col, origVal, newVal) {
				continue
			}

			boundVal, targetSC, err := bindNewValue(ws.Dialect, col, newVal)
			if err != nil {
				return nil, nil, err
			}

			noop, err := isCellNoop(ws.Dialect, origVal, newVal, boundVal, targetSC)
			if err != nil {
				return nil, nil, err
			}
			if noop {
				continue
			}

			effectiveSets = append(effectiveSets, pendingSet{
				col:      col,
				boundVal: boundVal,
				origVal:  origVal,
			})
		}

		// Whole row restored to original values -> emit ZERO UPDATE statements (never trigger AFTER UPDATE).
		if len(effectiveSets) == 0 {
			continue
		}

		paramIdx := 0
		var setSQL []string
		var setPreview []string
		var args []any

		for _, s := range effectiveSets {
			paramIdx++
			qCol := quoteIdent(ws.Dialect, s.col.Detail.Name)
			ph := placeholder(ws.Dialect, paramIdx)
			setSQL = append(setSQL, fmt.Sprintf("%s = %s", qCol, ph))
			setPreview = append(setPreview, fmt.Sprintf("%s = %s", qCol, formatSQLPreviewLiteral(s.boundVal)))
			args = append(args, s.boundVal)
		}

		// WHERE starts with original stable key values (even when PK columns themselves are modified in SET).
		whereSQL, wherePreview, keyArgs, err := buildKeyPredicates(upd.KeyValues, &paramIdx)
		if err != nil {
			return nil, nil, err
		}
		args = append(args, keyArgs...)

		// Append optimistic-concurrency predicates for each actually modified non-key column.
		for _, s := range effectiveSets {
			if keySet[s.col.Detail.Name] {
				// Original PK value is already verified in key predicates; never duplicate it.
				continue
			}
			pred, err := buildComparePredicate(ws.Dialect, s.col, s.origVal, false, &paramIdx)
			if err != nil {
				return nil, nil, err
			}
			whereSQL = append(whereSQL, pred.SQL)
			wherePreview = append(wherePreview, pred.PreviewSQL)
			args = append(args, pred.Args...)
		}

		sqlStr := fmt.Sprintf("UPDATE %s SET %s WHERE %s", tableRef, strings.Join(setSQL, ", "), strings.Join(whereSQL, " AND "))
		prevStr := fmt.Sprintf("UPDATE %s SET %s WHERE %s", tableRef, strings.Join(setPreview, ", "), strings.Join(wherePreview, " AND "))

		steps = append(steps, ChangeStep{
			Index:  len(steps),
			Op:     OpUpdate,
			RowKey: rowKey,
			Stmt: driver.BoundStatement{
				SQL:  sqlStr,
				Args: args,
			},
			PreviewSQL: prevStr,
			Params:     formatParams(args),
		})
		updateCount++
	}

	// --- Phase 3: INSERTS ---
	for i, ins := range req.Inserts {
		rowKey := ins.RowKey
		if rowKey == "" {
			rowKey = fmt.Sprintf("insert#%d", i)
		}
		for colName := range ins.Values {
			if _, ok := colMap[colName]; !ok {
				return nil, nil, dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("新增行包含未知列: %s", colName), "")
			}
		}

		paramIdx := 0
		var colNamesSQL []string
		var placeholdersSQL []string
		var previewVals []string
		var args []any

		for _, col := range orderedCols {
			val, provided := ins.Values[col.Detail.Name]
			if !provided || val.Mode == dto.CellInputDefault {
				// Omitted or explicit CellInputDefault lets database apply DEFAULT / AUTO_INCREMENT / GENERATED / IDENTITY.
				continue
			}
			if !col.Detail.Insertable {
				reason := col.Detail.ReadOnlyReason
				if reason == "" {
					reason = fmt.Sprintf("列 %s 不允许在新增时显式写入", col.Detail.Name)
				}
				return nil, nil, dto.NewAppError(dto.ErrInvalidArgument, reason, col.Detail.Name)
			}
			boundVal, _, err := bindNewValue(ws.Dialect, col, val)
			if err != nil {
				return nil, nil, err
			}
			paramIdx++
			colNamesSQL = append(colNamesSQL, quoteIdent(ws.Dialect, col.Detail.Name))
			placeholdersSQL = append(placeholdersSQL, placeholder(ws.Dialect, paramIdx))
			previewVals = append(previewVals, formatSQLPreviewLiteral(boundVal))
			args = append(args, boundVal)
		}

		var sqlStr, prevStr string
		if len(colNamesSQL) == 0 {
			if ws.Dialect == dto.EngineMySQL || ws.Dialect == dto.EngineMariaDB {
				sqlStr = fmt.Sprintf("INSERT INTO %s () VALUES ()", tableRef)
				prevStr = sqlStr
			} else {
				sqlStr = fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", tableRef)
				prevStr = sqlStr
			}
		} else {
			colsJoined := strings.Join(colNamesSQL, ", ")
			sqlStr = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", tableRef, colsJoined, strings.Join(placeholdersSQL, ", "))
			prevStr = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", tableRef, colsJoined, strings.Join(previewVals, ", "))
		}

		steps = append(steps, ChangeStep{
			Index:  len(steps),
			Op:     OpInsert,
			RowKey: rowKey,
			Stmt: driver.BoundStatement{
				SQL:  sqlStr,
				Args: args,
			},
			PreviewSQL: prevStr,
			Params:     formatParams(args),
		})
		insertCount++
	}

	plan := &ChangePlan{
		Source:            source,
		SchemaFingerprint: ComputeSchemaFingerprint(ws),
		Steps:             steps,
		DeleteCount:       deleteCount,
		UpdateCount:       updateCount,
		InsertCount:       insertCount,
	}

	previews := make([]dto.ChangeStepPreview, len(steps))
	for i, st := range steps {
		previews[i] = dto.ChangeStepPreview{
			Index:      st.Index,
			Op:         string(st.Op),
			RowKey:     st.RowKey,
			SQL:        st.Stmt.SQL,
			PreviewSQL: st.PreviewSQL,
			Params:     st.Params,
		}
	}

	return plan, previews, nil
}

// isCellNoop reports whether a requested cell update equals the original read-time fact,
// distinguishing NULL vs empty string "" and SQLite storage classes accurately.
func isCellNoop(dialect dto.EngineType, orig dto.OriginalCellValue, newVal dto.NewCellValue, boundVal any, sqliteTargetSC string) (bool, error) {
	if newVal.Mode == dto.CellInputNull {
		if orig.Kind != dto.KindNull {
			return false, nil
		}
		if dialect == dto.EngineSQLite {
			sc := strings.ToLower(strings.TrimSpace(orig.StorageClass))
			if sc != "" && sc != "null" {
				return false, dto.NewAppError(dto.ErrInvalidArgument, "原值类型与 SQLite 存储类型不一致", "")
			}
		}
		return true, nil
	}

	// CellInputLiteral:
	if orig.Kind == dto.KindNull {
		return false, nil
	}
	if orig.Truncated || orig.Kind == dto.KindBinary {
		return false, nil
	}
	if dialect == dto.EngineSQLite {
		origSC, err := normalizeSQLiteOriginalStorageClass(orig)
		if err != nil {
			return false, err
		}
		if origSC != sqliteTargetSC {
			return false, nil
		}
		if origSC == "integer" {
			if origInt, err := strconv.ParseInt(trimSQLiteWhitespace(orig.Text), 10, 64); err == nil {
				if newInt, ok := boundVal.(int64); ok {
					return origInt == newInt, nil
				}
			}
		}
		return orig.Text == newVal.Text, nil
	}
	return orig.Text == newVal.Text, nil
}

// isExistingSQLiteRealNoop checks whether the user restored an unchanged SQLite REAL cell
// (such as an existing 0.1 in a REAL, NUMERIC, or non-STRICT INTEGER column) so it is cleanly
// stripped as a no-op before strict new-value int64/float64 precision checks.
func isExistingSQLiteRealNoop(dialect dto.EngineType, col driver.WriteColumn, orig dto.OriginalCellValue, newVal dto.NewCellValue) bool {
	if dialect != dto.EngineSQLite || newVal.Mode != dto.CellInputLiteral {
		return false
	}
	if orig.Kind == dto.KindNull || orig.Truncated || orig.Kind == dto.KindBinary {
		return false
	}
	if orig.Text != newVal.Text {
		return false
	}
	origSC, err := normalizeSQLiteOriginalStorageClass(orig)
	if err != nil || origSC != "real" {
		return false
	}
	switch col.SQLiteAffinity {
	case "real", "numeric", "integer":
		return isSQLiteNumericLiteral(newVal.Text)
	default:
		return false
	}
}

func formatParams(args []any) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = formatParamDisplay(a)
	}
	return out
}
