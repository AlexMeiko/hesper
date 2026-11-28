package changeset

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"hesper/internal/driver"
	"hesper/internal/dto"
)

// quoteIdent safely quotes one SQL identifier according to the target database dialect.
func quoteIdent(dialect dto.EngineType, ident string) string {
	switch dialect {
	case dto.EngineMySQL, dto.EngineMariaDB:
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
	default:
		return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
	}
}

// qualifyTable produces the fully qualified table reference for DML statements.
func qualifyTable(ws driver.WriteSchema) string {
	switch ws.Dialect {
	case dto.EngineMySQL, dto.EngineMariaDB:
		if ws.Database != "" {
			return quoteIdent(ws.Dialect, ws.Database) + "." + quoteIdent(ws.Dialect, ws.Table)
		}
		return quoteIdent(ws.Dialect, ws.Table)
	case dto.EnginePostgres:
		schema := ws.Schema
		if schema == "" {
			schema = "public"
		}
		return quoteIdent(ws.Dialect, schema) + "." + quoteIdent(ws.Dialect, ws.Table)
	default:
		if ws.Database != "" {
			return quoteIdent(ws.Dialect, ws.Database) + "." + quoteIdent(ws.Dialect, ws.Table)
		}
		return quoteIdent(ws.Dialect, ws.Table)
	}
}

func placeholder(dialect dto.EngineType, paramIndex int) string {
	if dialect == dto.EnginePostgres {
		return fmt.Sprintf("$%d", paramIndex)
	}
	return "?"
}

// formatSQLPreviewLiteral formats one bound Go parameter strictly for human-readable SQL preview.
// Actual execution ALWAYS uses BoundStatement.SQL + BoundStatement.Args (never interpolated SQL).
func formatSQLPreviewLiteral(v any) string {
	if v == nil {
		return "NULL"
	}
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		s := fmt.Sprint(x)
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
}

func formatParamDisplay(v any) string {
	if v == nil {
		return "NULL"
	}
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// isSQLiteNumericLiteral reports whether s matches SQLite's well-formed numeric literal grammar
// that triggers affinity conversion on INTEGER / NUMERIC / REAL columns:
// optional ASCII whitespace, optional +/-, digits with optional fractional part, optional exponent.
// Non-numeric text such as ISO timestamps ("2026-11-27T12:00:00Z") or JSON ("{\"a\":1}") returns false.
func isSQLiteNumericLiteral(s string) bool {
	s = strings.Trim(s, " \t\r\n")
	if s == "" {
		return false
	}
	i := 0
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return false
	}

	digitsBeforeDot := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		digitsBeforeDot++
		i++
	}
	digitsAfterDot := 0
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			digitsAfterDot++
			i++
		}
	}
	if digitsBeforeDot == 0 && digitsAfterDot == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		expDigits := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			expDigits++
			i++
		}
		if expDigits == 0 {
			return false
		}
	}
	return i == len(s)
}

// parseLosslessSQLiteFloat64 verifies that a numeric literal string is represented with ZERO
// IEEE 754 binary64 rounding error by comparing big.Rat.SetFloat64(f) directly against the exact
// rational value of the input (rejecting rounded values such as 1e23 -> 99999999999999991611392).
// Reading and rebinding existing REAL values for WHERE checks uses bindOriginalSQLiteValue instead.
func parseLosslessSQLiteFloat64(trimmed string) (float64, bool) {
	if len(trimmed) > 400 {
		return 0, false
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	rIn, okIn := new(big.Rat).SetString(trimmed)
	if !okIn {
		return 0, false
	}
	rFloat := new(big.Rat).SetFloat64(f)
	if rFloat == nil || rIn.Cmp(rFloat) != 0 {
		return 0, false
	}
	return f, true
}

// bindNewValue validates and converts one user-supplied NewCellValue into a bound parameter
// strictly separated from bindOriginalValue.
// Also returns the resulting SQLite storage class ("null"|"integer"|"real"|"text") for no-op detection.
func bindNewValue(dialect dto.EngineType, col driver.WriteColumn, val dto.NewCellValue) (any, string, error) {
	switch val.Mode {
	case dto.CellInputNull:
		return nil, "null", nil
	case dto.CellInputLiteral:
		// handled below
	case dto.CellInputDefault:
		return nil, "", dto.NewAppError(
			dto.ErrInvalidArgument,
			fmt.Sprintf("列 %s 不允许以 default 模式直接绑定值", col.Detail.Name),
			"",
		)
	default:
		return nil, "", dto.NewAppError(
			dto.ErrInvalidArgument,
			fmt.Sprintf("列 %s 的输入模式无效: %q", col.Detail.Name, val.Mode),
			"",
		)
	}

	if dialect != dto.EngineSQLite {
		// PostgreSQL (QueryExecModeExec text format) and MySQL/MariaDB (prepared-statement string)
		// parse exact decimal / bigint / date / json / enum text directly on the server with zero float64 rounding.
		return val.Text, "", nil
	}

	// SQLite affinity rules:
	switch col.SQLiteAffinity {
	case "text", "blob", "any":
		// TEXT, untyped (blob), and STRICT table ANY columns have no numeric coercion;
		// preserve exact input text (including leading zeros such as "000123" and arbitrary precision strings).
		return val.Text, "text", nil
	case "integer":
		if !isSQLiteNumericLiteral(val.Text) {
			// Non-numeric text is stored verbatim as TEXT in non-STRICT SQLite (and rejected natively by STRICT).
			return val.Text, "text", nil
		}
		trimmed := strings.Trim(val.Text, " \t\r\n")
		i64, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil, "", dto.NewAppError(
				dto.ErrPrecisionUnsupported,
				fmt.Sprintf("SQLite INTEGER 亲和性列 %s 仅支持精确 64 位整数，值 %q 将溢出或发生精度转换，已拒绝写入", col.Detail.Name, val.Text),
				"",
			)
		}
		return i64, "integer", nil
	case "real":
		if !isSQLiteNumericLiteral(val.Text) {
			return val.Text, "text", nil
		}
		trimmed := strings.Trim(val.Text, " \t\r\n")
		f, ok := parseLosslessSQLiteFloat64(trimmed)
		if !ok {
			return nil, "", dto.NewAppError(
				dto.ErrPrecisionUnsupported,
				fmt.Sprintf("SQLite REAL 列 %s 无法无损保存数值 %q (超出 IEEE-754 float64 精度)，请改用 TEXT 列保存高精度十进制", col.Detail.Name, val.Text),
				"",
			)
		}
		return f, "real", nil
	default: // "numeric" (covers DECIMAL(38,10), DATETIME, TIMESTAMP, JSON, BOOLEAN, etc.)
		if !isSQLiteNumericLiteral(val.Text) {
			// SQLite NUMERIC affinity leaves non-numeric text (e.g. ISO datetime, JSON objects) untouched as TEXT.
			return val.Text, "text", nil
		}
		trimmed := strings.Trim(val.Text, " \t\r\n")
		if i64, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return i64, "integer", nil
		}
		f, ok := parseLosslessSQLiteFloat64(trimmed)
		if !ok {
			return nil, "", dto.NewAppError(
				dto.ErrPrecisionUnsupported,
				fmt.Sprintf("SQLite NUMERIC 亲和性列 %s 无法无损保存高精度数字 %q (SQLite 会转为 IEEE-754 浮点舍入)，请改用 TEXT 列存储", col.Detail.Name, val.Text),
				"",
			)
		}
		return f, "real", nil
	}
}

// normalizeSQLiteOriginalStorageClass resolves the authoritative SQLite typeof() storage class
// for an OriginalCellValue (using StorageClass from BrowseTable, with safe fallback for Kind).
func normalizeSQLiteOriginalStorageClass(orig dto.OriginalCellValue) (string, error) {
	sc := strings.ToLower(strings.TrimSpace(orig.StorageClass))
	if orig.Kind == dto.KindNull {
		if sc != "" && sc != "null" {
			return "", dto.NewAppError(dto.ErrInvalidArgument, "原值类型与 SQLite 存储类型不一致", "")
		}
		return "null", nil
	}
	if sc == "null" {
		return "", dto.NewAppError(dto.ErrInvalidArgument, "原值类型与 SQLite 存储类型不一致", "")
	}
	if sc != "" {
		switch sc {
		case "integer", "real", "text", "blob":
			return sc, nil
		default:
			return "", dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("无效的 SQLite 存储类型: %q", orig.StorageClass), "")
		}
	}
	switch orig.Kind {
	case dto.KindInt:
		return "integer", nil
	case dto.KindFloat:
		return "real", nil
	case dto.KindBinary:
		return "blob", nil
	default:
		return "text", nil
	}
}

// bindOriginalSQLiteValue faithfully restores an existing SQLite cell value by its StorageClass
// WITHOUT applying new-value precision limits (e.g. existing REAL 0.1 parses cleanly to float64).
func bindOriginalSQLiteValue(colName string, orig dto.OriginalCellValue, isKey bool) (any, string, error) {
	sc, err := normalizeSQLiteOriginalStorageClass(orig)
	if err != nil {
		return nil, "", err
	}
	switch sc {
	case "integer":
		v, perr := strconv.ParseInt(strings.TrimSpace(orig.Text), 10, 64)
		if perr != nil {
			return nil, "", dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("列 %s 的整数原值无法解析: %q", colName, orig.Text), perr.Error())
		}
		return v, "integer", nil
	case "real":
		v, perr := strconv.ParseFloat(strings.TrimSpace(orig.Text), 64)
		if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, "", dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("列 %s 的浮点原值无法解析: %q", colName, orig.Text), "")
		}
		return v, "real", nil
	case "text":
		return orig.Text, "text", nil
	default:
		if isKey {
			return nil, "", dto.NewAppError(dto.ErrRowNotIdentifiable, fmt.Sprintf("稳定键列 %s 的原值为二进制或不支持比较，无法安全定位行", colName), "")
		}
		return nil, "", dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("列 %s 的原值为二进制，不支持并发校验", colName), "")
	}
}

type builtPredicate struct {
	SQL        string
	PreviewSQL string
	Args       []any
}

// buildComparePredicate constructs a dialect-accurate WHERE predicate for either a stable key
// (isKey=true) or a modified column's optimistic concurrency check (isKey=false).
// Original NULL values ALWAYS generate "<col> IS NULL" with zero bound parameters.
func buildComparePredicate(
	dialect dto.EngineType,
	col driver.WriteColumn,
	orig dto.OriginalCellValue,
	isKey bool,
	nextParamIdx *int,
) (builtPredicate, error) {
	qCol := quoteIdent(dialect, col.Detail.Name)

	// 1. NULL original check (forbidden on stable keys; always IS NULL on modified columns).
	if orig.Kind == dto.KindNull {
		if isKey {
			return builtPredicate{}, dto.NewAppError(
				dto.ErrRowNotIdentifiable,
				fmt.Sprintf("稳定键列 %s 的值为 NULL，无法安全定位行", col.Detail.Name),
				"",
			)
		}
		return builtPredicate{
			SQL:        qCol + " IS NULL",
			PreviewSQL: qCol + " IS NULL",
		}, nil
	}

	// 2. Truncated or binary originals cannot form a trustworthy equality predicate.
	if orig.Truncated || orig.Kind == dto.KindBinary {
		if isKey {
			return builtPredicate{}, dto.NewAppError(
				dto.ErrRowNotIdentifiable,
				fmt.Sprintf("稳定键列 %s 的值被截断或为二进制，禁止修改或删除该行", col.Detail.Name),
				"",
			)
		}
		return builtPredicate{}, dto.NewAppError(
			dto.ErrInvalidArgument,
			fmt.Sprintf("列 %s 的原值被截断或为二进制，暂不支持行内并发校验修改", col.Detail.Name),
			"",
		)
	}

	if isKey && !col.KeyComparable {
		return builtPredicate{}, dto.NewAppError(
			dto.ErrRowNotIdentifiable,
			fmt.Sprintf("列 %s 不支持作为稳定键参与可靠定位", col.Detail.Name),
			"",
		)
	}

	allocPh := func() string {
		*nextParamIdx++
		return placeholder(dialect, *nextParamIdx)
	}

	switch col.CompareStrategy {
	case driver.CompareExact:
		ph := allocPh()
		val := orig.Text
		return builtPredicate{
			SQL:        fmt.Sprintf("%s = %s", qCol, ph),
			PreviewSQL: fmt.Sprintf("%s = %s", qCol, formatSQLPreviewLiteral(val)),
			Args:       []any{val},
		}, nil

	case driver.CompareMySQLBinary:
		val := orig.Text
		lit := formatSQLPreviewLiteral(val)
		if isKey {
			// Combine index-friendly equality with charset-normalized byte-wise comparison:
			// CONVERT(... USING utf8mb4) prevents false conflicts on non-utf8mb4 columns (e.g. latin1 'é')
			// while CAST(... AS BINARY) preserves exact case and PAD SPACE trailing-space detection.
			ph1 := allocPh()
			ph2 := allocPh()
			return builtPredicate{
				SQL:        fmt.Sprintf("(%s = %s AND CAST(CONVERT(%s USING utf8mb4) AS BINARY) = CAST(CONVERT(%s USING utf8mb4) AS BINARY))", qCol, ph1, qCol, ph2),
				PreviewSQL: fmt.Sprintf("(%s = %s AND CAST(CONVERT(%s USING utf8mb4) AS BINARY) = CAST(CONVERT(%s USING utf8mb4) AS BINARY))", qCol, lit, qCol, lit),
				Args:       []any{val, val},
			}, nil
		}
		ph := allocPh()
		return builtPredicate{
			SQL:        fmt.Sprintf("CAST(CONVERT(%s USING utf8mb4) AS BINARY) = CAST(CONVERT(%s USING utf8mb4) AS BINARY)", qCol, ph),
			PreviewSQL: fmt.Sprintf("CAST(CONVERT(%s USING utf8mb4) AS BINARY) = CAST(CONVERT(%s USING utf8mb4) AS BINARY)", qCol, lit),
			Args:       []any{val},
		}, nil

	case driver.CompareMySQLJSON:
		ph := allocPh()
		val := orig.Text
		return builtPredicate{
			SQL:        fmt.Sprintf("CAST(%s AS JSON) = CAST(%s AS JSON)", qCol, ph),
			PreviewSQL: fmt.Sprintf("CAST(%s AS JSON) = CAST(%s AS JSON)", qCol, formatSQLPreviewLiteral(val)),
			Args:       []any{val},
		}, nil

	case driver.ComparePGJSONText:
		ph := allocPh()
		val := orig.Text
		return builtPredicate{
			SQL:        fmt.Sprintf("%s::text = %s", qCol, ph),
			PreviewSQL: fmt.Sprintf("%s::text = %s", qCol, formatSQLPreviewLiteral(val)),
			Args:       []any{val},
		}, nil

	case driver.ComparePGJSONB:
		ph := allocPh()
		val := orig.Text
		return builtPredicate{
			SQL:        fmt.Sprintf("%s = %s::jsonb", qCol, ph),
			PreviewSQL: fmt.Sprintf("%s = %s::jsonb", qCol, formatSQLPreviewLiteral(val)),
			Args:       []any{val},
		}, nil

	case driver.ComparePGCCollation:
		val := orig.Text
		lit := formatSQLPreviewLiteral(val)
		if isKey {
			ph1 := allocPh()
			ph2 := allocPh()
			return builtPredicate{
				SQL:        fmt.Sprintf(`(%s = %s AND %s::text COLLATE "C" = %s)`, qCol, ph1, qCol, ph2),
				PreviewSQL: fmt.Sprintf(`(%s = %s AND %s::text COLLATE "C" = %s)`, qCol, lit, qCol, lit),
				Args:       []any{val, val},
			}, nil
		}
		ph := allocPh()
		return builtPredicate{
			SQL:        fmt.Sprintf(`%s::text COLLATE "C" = %s`, qCol, ph),
			PreviewSQL: fmt.Sprintf(`%s::text COLLATE "C" = %s`, qCol, lit),
			Args:       []any{val},
		}, nil

	case driver.CompareSQLiteBinaryTypeof:
		val, sc, err := bindOriginalSQLiteValue(col.Detail.Name, orig, isKey)
		if err != nil {
			return builtPredicate{}, err
		}
		ph1 := allocPh()
		ph2 := allocPh()
		return builtPredicate{
			SQL:        fmt.Sprintf("(%s COLLATE BINARY = %s AND typeof(%s) = %s)", qCol, ph1, qCol, ph2),
			PreviewSQL: fmt.Sprintf("(%s COLLATE BINARY = %s AND typeof(%s) = %s)", qCol, formatSQLPreviewLiteral(val), qCol, formatSQLPreviewLiteral(sc)),
			Args:       []any{val, sc},
		}, nil

	default:
		if isKey {
			return builtPredicate{}, dto.NewAppError(
				dto.ErrRowNotIdentifiable,
				fmt.Sprintf("稳定键列 %s 的类型不支持可靠比较", col.Detail.Name),
				"",
			)
		}
		return builtPredicate{}, dto.NewAppError(
			dto.ErrInvalidArgument,
			fmt.Sprintf("列 %s 的类型首版暂不支持并发校验修改", col.Detail.Name),
			"",
		)
	}
}
