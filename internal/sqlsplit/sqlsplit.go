package sqlsplit

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Dialect selects dialect-specific lexical rules (e.g., MySQL DELIMITER & #, PG $$ quoting).
type Dialect string

const (
	DialectMySQL    Dialect = "mysql"
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
)

// StatementKind is a coarse classification of a SQL statement used for session state,
// commit-outcome-unknown tracking, and client read-only guardrails.
type StatementKind string

const (
	KindQuery               StatementKind = "query"                 // SELECT, SHOW, DESCRIBE, EXPLAIN, VALUES, TABLE, WITH ... SELECT
	KindBegin               StatementKind = "begin"                 // BEGIN, START TRANSACTION
	KindCommit              StatementKind = "commit"                // COMMIT
	KindRollback            StatementKind = "rollback"              // ROLLBACK (full transaction rollback)
	KindRollbackToSavepoint StatementKind = "rollback_to_savepoint" // ROLLBACK [WORK|TRANSACTION] TO [SAVEPOINT] sp
	KindUse                 StatementKind = "use"                   // USE db (MySQL)
	KindSet                 StatementKind = "set"                   // SET ...
	KindWrite               StatementKind = "write"                 // INSERT, UPDATE, DELETE, REPLACE, MERGE, CALL, DO, LOAD
	KindDDL                 StatementKind = "ddl"                   // CREATE, ALTER, DROP, TRUNCATE, RENAME, GRANT, REVOKE
	KindOther               StatementKind = "other"
)

// Statement represents one lexically separated SQL statement with source coordinates.
type Statement struct {
	Text         string
	StartOffset  int
	EndOffset    int
	StartLine    int
	EndLine      int
	Kind         StatementKind
	FirstWord    string
	MutatesData  bool   // true for write/DDL/CALL/CTE-with-write statements
	ReturnsRows  bool   // true when the DB returns a result set (queries, EXPLAIN, MariaDB ANALYZE <stmt>, CALL, RETURNING, CTE-with-SELECT)
	StartsNextTx bool   // true for COMMIT/ROLLBACK/END ... AND CHAIN (immediately begins a chained tx on success)
	SplitError   string // non-empty when the splitter refused to split an unclosed or unsupported compound block
	tokens       []sqlToken
}

// Split splits SQL text into individual statements using a dialect-aware state machine.
// Handles single/double/backtick quotes, SQLite [...] identifiers, escaped quotes, line comments
// (-- and MySQL #), block comments (nested in PG), MySQL/MariaDB executable comments (/*!...*/ / /*M!...*/),
// PostgreSQL $tag$...$tag$ dollar quoting, MySQL DELIMITER, and dialect-specific compound blocks
// (SQLite CREATE TRIGGER ... ; END ;, MySQL/MariaDB routines/triggers/events, PG BEGIN ATOMIC ... END).
func Split(src string, dialect Dialect) []Statement {
	var stmts []Statement
	delimiter := ";"
	n := len(src)

	stmtStart := -1
	stmtStartLine := 1
	line := 1
	inExecComment := false
	tracker := newCompoundTracker(dialect)

	flush := func(end int, atEOF bool) {
		var splitErr string
		if delimiter == ";" {
			splitErr = tracker.finalizeError(atEOF)
		}
		tracker.reset()
		inExecComment = false

		if stmtStart < 0 || end <= stmtStart {
			stmtStart = -1
			return
		}
		raw := src[stmtStart:end]
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			stmtStart = -1
			return
		}
		tokens := tokenizeSQL(trimmed, dialect)
		if len(tokens) == 0 {
			stmtStart = -1
			return
		}
		leadTrim := strings.Index(raw, trimmed)
		realStart := stmtStart + leadTrim
		realEnd := realStart + len(trimmed)
		realStartLine := lineAtOffset(src, realStart)
		realEndLine := lineAtOffset(src, realEnd)
		kind, firstWord, mutates, returnsRows, startsNextTx := classifyTokensDetail(tokens, 0, trimmed, dialect)
		if dialect == DialectMySQL && splitErr == "" && isMySQLUnsupportedAnonBlockRaw(trimmed) {
			splitErr = errMariaDBAnonBlockUnsupported
		}
		if splitErr == errMariaDBAnonBlockUnsupported {
			kind = KindOther
			mutates = true
			returnsRows = false
			startsNextTx = false
		}
		stmts = append(stmts, Statement{
			Text:         trimmed,
			StartOffset:  realStart,
			EndOffset:    realEnd,
			StartLine:    realStartLine,
			EndLine:      realEndLine,
			Kind:         kind,
			FirstWord:    firstWord,
			MutatesData:  mutates,
			ReturnsRows:  returnsRows,
			StartsNextTx: startsNextTx,
			SplitError:   splitErr,
			tokens:       tokens,
		})
		stmtStart = -1
	}

	i := 0
	atLineStart := true
	for i < n {
		// Closing delimiter of an active MySQL/MariaDB executable comment (/*! ... */ / /*M! ... */).
		if inExecComment && src[i] == '*' && i+1 < n && src[i+1] == '/' {
			inExecComment = false
			i += 2
			atLineStart = false
			continue
		}

		// MySQL DELIMITER directive at line start (outside any open quote/comment)
		if dialect == DialectMySQL && !inExecComment && atLineStart && stmtStart < 0 {
			j := i
			for j < n && (src[j] == ' ' || src[j] == '\t') {
				j++
			}
			if hasPrefixFold(src[j:], "DELIMITER") && j+9 < n && (src[j+9] == ' ' || src[j+9] == '\t') {
				lineEnd := strings.IndexByte(src[j:], '\n')
				var rest string
				if lineEnd < 0 {
					rest = src[j+9:]
					i = n
				} else {
					rest = src[j+9 : j+lineEnd]
					i = j + lineEnd + 1
					line++
				}
				if newDelim := strings.TrimSpace(rest); newDelim != "" {
					delimiter = newDelim
				}
				tracker.reset()
				atLineStart = true
				continue
			}
		}

		// Match current delimiter
		if delimiter != "" && strings.HasPrefix(src[i:], delimiter) {
			if inExecComment {
				// Never split inside an open /*! ... */ executable comment; track whether ';' closes
				// a statement inside the comment so multi-statement executable comments fail closed.
				if delimiter == ";" && stmtStart >= 0 && tracker.shouldSplitOnSemicolon() {
					tracker.execCommentSawTrailingSemi = true
				}
				for k := 0; k < len(delimiter); k++ {
					if delimiter[k] == '\n' {
						line++
					}
				}
				i += len(delimiter)
				atLineStart = false
				continue
			}
			if delimiter == ";" && stmtStart >= 0 && !tracker.shouldSplitOnSemicolon() {
				i++
				atLineStart = false
				continue
			}
			flush(i, false)
			for k := 0; k < len(delimiter); k++ {
				if delimiter[k] == '\n' {
					line++
				}
			}
			i += len(delimiter)
			atLineStart = false
			continue
		}

		ch := src[i]

		// Track newlines and whitespace
		if ch == '\n' {
			line++
			i++
			atLineStart = true
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\f' || ch == '\v' {
			i++
			continue
		}

		// Single-line comment: -- ...
		if ch == '-' && i+1 < n && src[i+1] == '-' && (i+2 == n || src[i+2] == ' ' || src[i+2] == '\t' || src[i+2] == '\r' || src[i+2] == '\n' || src[i+2] == '\f' || src[i+2] == '\v' || dialect != DialectMySQL) {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}

		// MySQL # single-line comment
		if dialect == DialectMySQL && ch == '#' {
			i++
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment /* ... */ (nested in PostgreSQL; MySQL/MariaDB /*! ... */ & /*M! ... */ are expanded
		// using the same lexical rules as tokenizeSQL so tokens like /*!50020 DEFINER=...*/ stay visible).
		if ch == '/' && i+1 < n && src[i+1] == '*' {
			if !inExecComment {
				if prefixLen := mysqlExecCommentPrefixLen(src, i, dialect); prefixLen > 0 {
					if stmtStart < 0 {
						stmtStart = i
						stmtStartLine = line
						_ = stmtStartLine
					}
					inExecComment = true
					atLineStart = false
					i += prefixLen
					continue
				}
			}

			depth := 1
			i += 2
			for i < n && depth > 0 {
				if src[i] == '\n' {
					line++
					i++
					continue
				}
				if dialect == DialectPostgres && src[i] == '/' && i+1 < n && src[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			atLineStart = false
			continue
		}

		// Real statement content begins here
		if stmtStart < 0 {
			stmtStart = i
			stmtStartLine = line
			_ = stmtStartLine
		}
		if delimiter == ";" {
			tracker.noteStmtToken()
		}
		atLineStart = false

		// Single-quoted string (including PostgreSQL E'...' / e'...' escape string literals
		// and multiline PostgreSQL string continuations preserving escape mode).
		if next, nl, ok := scanSingleQuotedString(src, i, dialect, false); ok {
			if delimiter == ";" {
				tracker.onLiteralOrQuotedIdent()
			}
			line += nl
			i = next
			continue
		}

		// Double-quoted identifier or string
		if ch == '"' {
			if delimiter == ";" {
				tracker.onLiteralOrQuotedIdent()
			}
			i++
			for i < n {
				if src[i] == '\n' {
					line++
				}
				if dialect == DialectMySQL && src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == '"' {
					if i+1 < n && src[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		// Backtick identifier (MySQL / SQLite)
		if ch == '`' && dialect != DialectPostgres {
			if delimiter == ";" {
				tracker.onLiteralOrQuotedIdent()
			}
			i++
			for i < n {
				if src[i] == '\n' {
					line++
				}
				if src[i] == '`' {
					if i+1 < n && src[i+1] == '`' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		// SQLite bracket-quoted identifier: [...]
		if dialect == DialectSQLite && ch == '[' {
			if delimiter == ";" {
				tracker.onLiteralOrQuotedIdent()
			}
			i++
			for i < n {
				if src[i] == '\n' {
					line++
				}
				if src[i] == ']' {
					i++
					break
				}
				i++
			}
			continue
		}

		// PostgreSQL dollar-quoting: $$...$$ or $tag$...$tag$
		if dialect == DialectPostgres && ch == '$' {
			if tag, ok := parseDollarTag(src[i:]); ok {
				if delimiter == ";" {
					tracker.onLiteralOrQuotedIdent()
				}
				i += len(tag)
				endIdx := strings.Index(src[i:], tag)
				if endIdx < 0 {
					line += strings.Count(src[i:], "\n")
					i = n
				} else {
					line += strings.Count(src[i:i+endIdx], "\n")
					i += endIdx + len(tag)
				}
				continue
			}
		}

		if delimiter == ";" {
			switch ch {
			case '(':
				tracker.onLParen()
				i++
				continue
			case ')':
				tracker.onRParen()
				i++
				continue
			case ':':
				tracker.onColon()
				i++
				continue
			}
			if dialect == DialectMySQL && ch == '@' {
				if next, isSysVar, ok := scanMySQLAtVar(src, i); ok {
					tracker.onMySQLAtVar(isSysVar)
					i = next
					continue
				}
			}
			r, size := utf8.DecodeRuneInString(src[i:])
			if isDialectIdentStart(dialect, r, size) || (r >= '0' && r <= '9') {
				start := i
				i += size
				for i < n {
					nr, nsize := utf8.DecodeRuneInString(src[i:])
					if isDialectIdentPart(dialect, nr, nsize) || nr == '$' {
						i += nsize
						continue
					}
					break
				}
				tracker.onWord(strings.ToUpper(src[start:i]))
				continue
			}
			tracker.onSymbol(ch)
			i += size
			continue
		}

		i++
	}

	flush(n, true)
	return stmts
}

// SelectStatementAtCursor selects the statement containing cursorOffset (or the nearest preceding one)
// from a pre-split slice of statements.
func SelectStatementAtCursor(stmts []Statement, cursorOffset int) (Statement, bool) {
	if len(stmts) == 0 {
		return Statement{}, false
	}
	if cursorOffset < 0 {
		return stmts[0], true
	}
	for _, s := range stmts {
		if cursorOffset >= s.StartOffset && cursorOffset <= s.EndOffset {
			return s, true
		}
	}
	// If cursor sits on whitespace/semicolon right after a statement, pick the nearest preceding one.
	chosen := stmts[0]
	for _, s := range stmts {
		if s.StartOffset <= cursorOffset {
			chosen = s
		}
	}
	return chosen, true
}

// StatementAtCursor returns the statement containing cursorOffset, or the nearest preceding statement.
func StatementAtCursor(src string, cursorOffset int, dialect Dialect) (Statement, bool) {
	return SelectStatementAtCursor(Split(src, dialect), cursorOffset)
}

// mysqlExecCommentPrefixLen detects MySQL/MariaDB executable conditional comment openers:
//
//	/*!...*/ , /*!50000 ...*/ , /*M!...*/ , /*M!100301 ...*/
//
// Returns the byte length of the opener prefix (including optional version digits), or 0 if not one.
func mysqlExecCommentPrefixLen(s string, i int, dialect Dialect) int {
	if dialect != DialectMySQL || i+2 >= len(s) || s[i] != '/' || s[i+1] != '*' {
		return 0
	}
	j := 0
	if s[i+2] == '!' {
		j = i + 3
	} else if s[i+2] == 'M' && i+3 < len(s) && s[i+3] == '!' {
		j = i + 4
	} else {
		return 0
	}
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	return j - i
}

// HighRiskMatch describes one production high-risk SQL pattern detected by the backend analyzer.
type HighRiskMatch struct {
	Kind  string // "drop" | "truncate" | "delete_no_where" | "update_no_where"
	Label string
	SQL   string
}

type sqlTokKind uint8

const (
	tokWord sqlTokKind = iota
	tokLParen
	tokRParen
	tokComma
)

type sqlToken struct {
	kind  sqlTokKind
	upper string
	depth int
}

// tokenizeSQL extracts keywords, parentheses, and commas while skipping non-executable comments,
// quoted strings, quoted identifiers, and PostgreSQL $tag$...$tag$ dollar-quoted blocks.
// MySQL/MariaDB executable comments (/*! ... */ and /*M! ... */) are expanded so their tokens are inspected.
func tokenizeSQL(s string, dialect Dialect) []sqlToken {
	var out []sqlToken
	n := len(s)
	depth := 0
	inExecComment := false
	i := 0

	for i < n {
		ch := s[i]

		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == '\f' || ch == '\v' {
			i++
			continue
		}

		// Closing delimiter of an active MySQL/MariaDB executable comment
		if inExecComment && ch == '*' && i+1 < n && s[i+1] == '/' {
			inExecComment = false
			i += 2
			continue
		}

		// Single-line comment --
		if ch == '-' && i+1 < n && s[i+1] == '-' && (i+2 == n || s[i+2] == ' ' || s[i+2] == '\t' || s[i+2] == '\r' || s[i+2] == '\n' || s[i+2] == '\f' || s[i+2] == '\v' || dialect != DialectMySQL) {
			i += 2
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}

		// MySQL # line comment
		if dialect == DialectMySQL && ch == '#' {
			i++
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment /* ... */ (nested in PostgreSQL; MySQL/MariaDB /*! ... */ & /*M! ... */ are expanded)
		if ch == '/' && i+1 < n && s[i+1] == '*' {
			if !inExecComment {
				if prefixLen := mysqlExecCommentPrefixLen(s, i, dialect); prefixLen > 0 {
					inExecComment = true
					i += prefixLen
					continue
				}
			}
			cdepth := 1
			i += 2
			for i < n && cdepth > 0 {
				if dialect == DialectPostgres && s[i] == '/' && i+1 < n && s[i+1] == '*' {
					cdepth++
					i += 2
					continue
				}
				if s[i] == '*' && i+1 < n && s[i+1] == '/' {
					cdepth--
					i += 2
					continue
				}
				i++
			}
			continue
		}

		// Single-quoted string (including PostgreSQL E'...' / e'...' escape string literals
		// and multiline PostgreSQL string continuations preserving escape mode).
		if next, _, ok := scanSingleQuotedString(s, i, dialect, false); ok {
			i = next
			continue
		}

		// Double-quoted identifier or string (never treated as SQL keyword)
		if ch == '"' {
			i++
			for i < n {
				if dialect == DialectMySQL && s[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if s[i] == '"' {
					if i+1 < n && s[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		// Backtick identifier (MySQL / SQLite)
		if ch == '`' && dialect != DialectPostgres {
			i++
			for i < n {
				if s[i] == '`' {
					if i+1 < n && s[i+1] == '`' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		// SQLite bracket-quoted identifier [...] (never treated as SQL keyword)
		if dialect == DialectSQLite && ch == '[' {
			i++
			for i < n {
				if s[i] == ']' {
					i++
					break
				}
				i++
			}
			continue
		}

		// PostgreSQL dollar-quoted string
		if dialect == DialectPostgres && ch == '$' {
			if tag, ok := parseDollarTag(s[i:]); ok {
				i += len(tag)
				endIdx := strings.Index(s[i:], tag)
				if endIdx < 0 {
					i = n
				} else {
					i += endIdx + len(tag)
				}
				continue
			}
		}

		if ch == '(' {
			depth++
			out = append(out, sqlToken{kind: tokLParen, upper: "(", depth: depth})
			i++
			continue
		}
		if ch == ')' {
			out = append(out, sqlToken{kind: tokRParen, upper: ")", depth: depth})
			if depth > 0 {
				depth--
			}
			i++
			continue
		}
		if ch == ',' {
			out = append(out, sqlToken{kind: tokComma, upper: ",", depth: depth})
			i++
			continue
		}
		if dialect == DialectMySQL && ch == '@' {
			if next, _, ok := scanMySQLAtVar(s, i); ok {
				i = next
				continue
			}
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if isDialectIdentStart(dialect, r, size) || (r >= '0' && r <= '9') {
			start := i
			i += size
			for i < n {
				nr, nsize := utf8.DecodeRuneInString(s[i:])
				if isDialectIdentPart(dialect, nr, nsize) || nr == '$' {
					i += nsize
					continue
				}
				break
			}
			out = append(out, sqlToken{
				kind:  tokWord,
				upper: strings.ToUpper(s[start:i]),
				depth: depth,
			})
			continue
		}

		i += size
	}

	return out
}

// unwrapExplainOrMariaAnalyze checks whether tokens represent a wrapper command that ACTUALLY executes
// its inner statement and returns an execution plan:
//   - EXPLAIN ANALYZE [VERBOSE] [FORMAT=...] <stmt>
//   - PostgreSQL EXPLAIN (ANALYZE [true|on|1], ...) <stmt>
//   - MariaDB ANALYZE [FORMAT=JSON] <SELECT|WITH|UPDATE|DELETE|INSERT|REPLACE|VALUES|(...)>
//     (explicitly excluding statistics-gathering ANALYZE TABLE / ANALYZE LOCAL / ANALYZE NO_WRITE_TO_BINLOG)
func unwrapExplainOrMariaAnalyze(tokens []sqlToken, baseDepth int, dialect Dialect) ([]sqlToken, string, bool) {
	if len(tokens) < 2 || tokens[0].kind != tokWord || tokens[0].depth != baseDepth {
		return nil, "", false
	}

	if tokens[0].upper == "EXPLAIN" {
		// Form 1: PostgreSQL parenthesized options: EXPLAIN (ANALYZE [true|on|1], ...) <stmt>
		if tokens[1].kind == tokLParen && tokens[1].depth == baseDepth+1 {
			closeIdx := findMatchingRParen(tokens, 1, baseDepth+1)
			if closeIdx < 0 || closeIdx+1 >= len(tokens) {
				return nil, "", false
			}
			optTokens := tokens[2:closeIdx]
			analyzeEnabled := false
			for i := 0; i < len(optTokens); i++ {
				if optTokens[i].kind == tokWord && (optTokens[i].upper == "ANALYZE" || optTokens[i].upper == "ANALYSE") {
					analyzeEnabled = true
					if i+1 < len(optTokens) && optTokens[i+1].kind == tokWord {
						next := optTokens[i+1].upper
						if next == "FALSE" || next == "OFF" || next == "0" {
							analyzeEnabled = false
						}
					}
					break
				}
			}
			if !analyzeEnabled {
				return nil, "", false
			}
			return tokens[closeIdx+1:], "EXPLAIN", true
		}

		// Form 2: Unparenthesized EXPLAIN [ANALYZE | ANALYSE] [VERBOSE] [FORMAT=TREE|JSON] <stmt>
		if tokens[1].kind == tokWord && (tokens[1].upper == "ANALYZE" || tokens[1].upper == "ANALYSE") {
			idx := 2
			for idx < len(tokens) && tokens[idx].depth == baseDepth && tokens[idx].kind == tokWord {
				if tokens[idx].upper == "VERBOSE" {
					idx++
					continue
				}
				if tokens[idx].upper == "FORMAT" && idx+1 < len(tokens) && tokens[idx+1].depth == baseDepth && tokens[idx+1].kind == tokWord {
					idx += 2
					continue
				}
				break
			}
			if idx < len(tokens) {
				return tokens[idx:], "EXPLAIN", true
			}
		}
		return nil, "", false
	}

	// MariaDB ANALYZE [FORMAT=JSON] <statement> (runs the inner statement and returns an execution plan)
	if dialect == DialectMySQL && tokens[0].upper == "ANALYZE" {
		idx := 1
		if idx+1 < len(tokens) && tokens[idx].depth == baseDepth && tokens[idx].kind == tokWord && tokens[idx].upper == "FORMAT" && tokens[idx+1].depth == baseDepth && tokens[idx+1].kind == tokWord {
			idx += 2
		}
		if idx < len(tokens) {
			if tokens[idx].kind == tokLParen && tokens[idx].depth == baseDepth+1 {
				return tokens[idx:], "ANALYZE", true
			}
			if tokens[idx].kind == tokWord && tokens[idx].depth == baseDepth {
				switch tokens[idx].upper {
				case "SELECT", "WITH", "UPDATE", "DELETE", "INSERT", "REPLACE", "VALUES":
					return tokens[idx:], "ANALYZE", true
				}
			}
		}
	}

	return nil, "", false
}

func findMatchingRParen(tokens []sqlToken, lparenIdx int, parenDepth int) int {
	for i := lparenIdx + 1; i < len(tokens); i++ {
		if tokens[i].kind == tokRParen && tokens[i].depth == parenDepth {
			return i
		}
	}
	return -1
}

// parseCTEs extracts all CTE bodies (at baseDepth+1) and the trailing main statement tokens (at baseDepth)
// from a WITH [RECURSIVE] ... statement.
func parseCTEs(tokens []sqlToken, baseDepth int) (cteBodies [][]sqlToken, mainTokens []sqlToken, ok bool) {
	if len(tokens) < 2 || tokens[0].depth != baseDepth || tokens[0].kind != tokWord || tokens[0].upper != "WITH" {
		return nil, nil, false
	}
	i := 1
	if i < len(tokens) && tokens[i].depth == baseDepth && tokens[i].kind == tokWord && tokens[i].upper == "RECURSIVE" {
		i++
	}

	for i < len(tokens) {
		// Advance to CTE's AS keyword at baseDepth (skipping CTE name and optional column list "(c1, c2)")
		for i < len(tokens) && !(tokens[i].depth == baseDepth && tokens[i].kind == tokWord && tokens[i].upper == "AS") {
			i++
		}
		if i >= len(tokens) {
			return cteBodies, nil, len(cteBodies) > 0
		}
		i++ // consume AS

		// Optional PG MATERIALIZED / NOT MATERIALIZED
		if i < len(tokens) && tokens[i].depth == baseDepth && tokens[i].kind == tokWord && tokens[i].upper == "NOT" {
			i++
		}
		if i < len(tokens) && tokens[i].depth == baseDepth && tokens[i].kind == tokWord && tokens[i].upper == "MATERIALIZED" {
			i++
		}

		// Expect CTE body '(' at baseDepth + 1
		if i >= len(tokens) || tokens[i].kind != tokLParen || tokens[i].depth != baseDepth+1 {
			return cteBodies, nil, len(cteBodies) > 0
		}
		bodyStart := i + 1
		closeIdx := findMatchingRParen(tokens, i, baseDepth+1)
		if closeIdx < 0 {
			return cteBodies, nil, len(cteBodies) > 0
		}

		cteBodies = append(cteBodies, tokens[bodyStart:closeIdx])

		i = closeIdx + 1
		if i >= len(tokens) {
			return cteBodies, nil, true
		}

		// Skip optional PG SEARCH / CYCLE clauses before ',' or main statement
		if tokens[i].depth == baseDepth && tokens[i].kind == tokWord && (tokens[i].upper == "SEARCH" || tokens[i].upper == "CYCLE") {
			for i < len(tokens) {
				if tokens[i].depth == baseDepth {
					if tokens[i].kind == tokComma {
						break
					}
					if tokens[i].kind == tokWord {
						switch tokens[i].upper {
						case "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "VALUES", "TABLE", "WITH":
							goto afterClause
						}
					}
				} else if tokens[i].depth == baseDepth+1 && tokens[i].kind == tokLParen {
					// Could be parenthesized main query after SEARCH/CYCLE if previous token wasn't BY/SET/TO/DEFAULT
					prev := tokens[i-1]
					if prev.kind == tokWord && prev.upper != "BY" && prev.upper != "SET" && prev.upper != "TO" && prev.upper != "DEFAULT" && prev.upper != "USING" {
						break
					}
				}
				i++
			}
		}
	afterClause:
		if i >= len(tokens) {
			return cteBodies, nil, true
		}
		if tokens[i].depth == baseDepth && tokens[i].kind == tokComma {
			i++
			continue
		}

		return cteBodies, tokens[i:], true
	}

	return cteBodies, nil, len(cteBodies) > 0
}

// DetectHighRisk inspects one parsed Statement for production high-risk operations per docs/03 §4.2:
//   - DROP
//   - TRUNCATE
//   - DELETE without a WHERE clause at the command's own parenthesis level
//   - UPDATE without a WHERE clause at the command's own parenthesis level
//   - EXPLAIN ANALYZE and MariaDB ANALYZE <DML> wrapping any of the above
//   - Data-modifying CTEs (e.g. PG WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d)
func DetectHighRisk(stmt Statement, dialect Dialect) []HighRiskMatch {
	tokens := stmt.tokens
	if tokens == nil {
		tokens = tokenizeSQL(stmt.Text, dialect)
	}
	var matches []HighRiskMatch
	inspectHighRiskTokens(tokens, 0, stmt.Text, dialect, &matches)
	return matches
}

func inspectHighRiskTokens(tokens []sqlToken, baseDepth int, rawSQL string, dialect Dialect, out *[]HighRiskMatch) {
	if len(tokens) == 0 {
		return
	}

	// Unwrap outer parentheses enclosing the entire command (e.g. "(DELETE FROM t)")
	for len(tokens) > 0 && tokens[0].kind == tokLParen && tokens[0].depth == baseDepth+1 {
		closeIdx := findMatchingRParen(tokens, 0, baseDepth+1)
		if closeIdx == len(tokens)-1 {
			tokens = tokens[1:closeIdx]
			baseDepth++
			continue
		}
		break
	}
	if len(tokens) == 0 {
		return
	}

	if inner, _, ok := unwrapExplainOrMariaAnalyze(tokens, baseDepth, dialect); ok {
		inspectHighRiskTokens(inner, baseDepth, rawSQL, dialect, out)
		return
	}

	if tokens[0].kind != tokWord || tokens[0].depth != baseDepth {
		return
	}

	switch tokens[0].upper {
	case "DROP":
		*out = append(*out, HighRiskMatch{Kind: "drop", Label: "DROP (删除对象)", SQL: rawSQL})
		return
	case "TRUNCATE":
		*out = append(*out, HighRiskMatch{Kind: "truncate", Label: "TRUNCATE (清空整表)", SQL: rawSQL})
		return
	case "DELETE":
		if !hasWordAtDepth(tokens[1:], baseDepth, "WHERE") {
			*out = append(*out, HighRiskMatch{Kind: "delete_no_where", Label: "无 WHERE 条件的 DELETE (全表删除)", SQL: rawSQL})
		}
		return
	case "UPDATE":
		if !hasWordAtDepth(tokens[1:], baseDepth, "WHERE") {
			*out = append(*out, HighRiskMatch{Kind: "update_no_where", Label: "无 WHERE 条件的 UPDATE (全表更新)", SQL: rawSQL})
		}
		return
	case "WITH":
		cteBodies, mainTokens, ok := parseCTEs(tokens, baseDepth)
		if !ok || len(mainTokens) == 0 {
			inspectHighRiskFallback(tokens[1:], rawSQL, out)
			return
		}
		for _, body := range cteBodies {
			inspectHighRiskTokens(body, baseDepth+1, rawSQL, dialect, out)
		}
		inspectHighRiskTokens(mainTokens, baseDepth, rawSQL, dialect, out)
		return
	}
}

func isLockingUpdateToken(tokens []sqlToken, idx int) bool {
	if idx <= 0 || tokens[idx].kind != tokWord || tokens[idx].upper != "UPDATE" {
		return false
	}
	prev := tokens[idx-1]
	// Matches "FOR UPDATE" and PostgreSQL "FOR NO KEY UPDATE"
	return prev.kind == tokWord && (prev.upper == "FOR" || prev.upper == "KEY")
}

func containsWriteKeywordFallback(tokens []sqlToken) bool {
	for i, t := range tokens {
		if t.kind != tokWord {
			continue
		}
		switch t.upper {
		case "INSERT", "DELETE", "MERGE", "REPLACE", "DROP", "TRUNCATE":
			return true
		case "UPDATE":
			if !isLockingUpdateToken(tokens, i) {
				return true
			}
		}
	}
	return false
}

func inspectHighRiskFallback(tokens []sqlToken, rawSQL string, out *[]HighRiskMatch) {
	seen := make(map[string]bool)
	addOnce := func(m HighRiskMatch) {
		if !seen[m.Kind] {
			seen[m.Kind] = true
			*out = append(*out, m)
		}
	}
	for i, t := range tokens {
		if t.kind != tokWord {
			continue
		}
		switch t.upper {
		case "DROP":
			addOnce(HighRiskMatch{Kind: "drop", Label: "DROP (删除对象)", SQL: rawSQL})
		case "TRUNCATE":
			addOnce(HighRiskMatch{Kind: "truncate", Label: "TRUNCATE (清空整表)", SQL: rawSQL})
		case "DELETE":
			if !hasWordAtDepth(tokens[i+1:], t.depth, "WHERE") {
				addOnce(HighRiskMatch{Kind: "delete_no_where", Label: "无 WHERE 条件的 DELETE (全表删除)", SQL: rawSQL})
			}
		case "UPDATE":
			if !isLockingUpdateToken(tokens, i) && !hasWordAtDepth(tokens[i+1:], t.depth, "WHERE") {
				addOnce(HighRiskMatch{Kind: "update_no_where", Label: "无 WHERE 条件的 UPDATE (全表更新)", SQL: rawSQL})
			}
		}
	}
}

func hasWordAtDepth(tokens []sqlToken, depth int, word string) bool {
	for _, t := range tokens {
		if t.depth == depth && t.kind == tokWord && t.upper == word {
			return true
		}
	}
	return false
}

// Classify inspects leading keywords (skipping non-executable comments) to classify statement intent.
func Classify(stmt string, dialect Dialect) (StatementKind, string, bool) {
	tokens := tokenizeSQL(stmt, dialect)
	if len(tokens) == 0 {
		return KindOther, "", false
	}
	kind, firstWord, mutates, _, _ := classifyTokensDetail(tokens, 0, stmt, dialect)
	return kind, firstWord, mutates
}

func classifyTokensDetail(tokens []sqlToken, baseDepth int, stmt string, dialect Dialect) (StatementKind, string, bool, bool, bool) {
	if len(tokens) == 0 {
		return KindOther, "", false, false, false
	}

	// Parenthesized statements like "(SELECT 1) UNION (SELECT 2)" or "(SELECT * FROM t)":
	// classify by the inner leading statement inside the opening parentheses.
	if tokens[0].kind == tokLParen && tokens[0].depth == baseDepth+1 {
		closeIdx := findMatchingRParen(tokens, 0, baseDepth+1)
		if closeIdx < 0 {
			closeIdx = len(tokens)
		}
		return classifyTokensDetail(tokens[1:closeIdx], baseDepth+1, stmt, dialect)
	}

	// EXPLAIN ANALYZE and MariaDB ANALYZE <stmt> execute the wrapped statement AND return an execution plan result set.
	if inner, wrapperWord, ok := unwrapExplainOrMariaAnalyze(tokens, baseDepth, dialect); ok {
		kind, _, mutates, _, startsNextTx := classifyTokensDetail(inner, baseDepth, stmt, dialect)
		return kind, wrapperWord, mutates, true, startsNextTx
	}

	var topWords []string
	for _, t := range tokens {
		if t.depth == baseDepth && t.kind == tokWord {
			topWords = append(topWords, t.upper)
			if len(topWords) >= 8 {
				break
			}
		}
	}
	if len(topWords) == 0 {
		return KindOther, "", false, false, false
	}
	first := topWords[0]
	second := ""
	if len(topWords) > 1 {
		second = topWords[1]
	}

	switch first {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "VALUES", "TABLE", "PRAGMA":
		// PRAGMA with '=' in SQLite can mutate settings
		if first == "PRAGMA" && strings.Contains(stmt, "=") {
			return KindSet, first, false, false, false
		}
		return KindQuery, first, false, true, false
	case "WITH":
		// Inspect CTE bodies and trailing main statement structurally so "WITH ... SELECT ... FOR UPDATE"
		// stays KindQuery, while data-modifying CTEs (INSERT/UPDATE/DELETE/MERGE) are classified as KindWrite.
		// If CTE parsing fails on unrecognized syntax, fall back conservatively to keyword scanning.
		cteBodies, mainTokens, ok := parseCTEs(tokens, baseDepth)
		if !ok || len(mainTokens) == 0 {
			if containsWriteKeywordFallback(tokens[1:]) {
				return KindWrite, first, true, hasWordAtDepth(tokens[1:], baseDepth, "RETURNING"), false
			}
			return KindQuery, first, false, true, false
		}
		hasWrite := false
		for _, body := range cteBodies {
			bKind, _, bMutates, _, _ := classifyTokensDetail(body, baseDepth+1, stmt, dialect)
			if bMutates || bKind == KindWrite || bKind == KindDDL {
				hasWrite = true
			}
		}
		mKind, _, mMutates, mainReturnsRows, _ := classifyTokensDetail(mainTokens, baseDepth, stmt, dialect)
		if mMutates || mKind == KindWrite || mKind == KindDDL {
			hasWrite = true
		}
		if hasWrite {
			return KindWrite, first, true, mainReturnsRows, false
		}
		return KindQuery, first, false, true, false
	case "BEGIN":
		if baseDepth == 0 && dialect == DialectMySQL && isMySQLUnsupportedAnonBlockRaw(stmt) {
			return KindOther, first, true, false, false
		}
		return KindBegin, first, false, false, false
	case "START":
		if second == "TRANSACTION" {
			return KindBegin, first, false, false, false
		}
		return KindOther, first, false, false, false
	case "COMMIT":
		startsNextTx := dialect != DialectSQLite && hasAndChain(topWords)
		return KindCommit, first, false, false, startsNextTx
	case "END":
		switch dialect {
		case DialectSQLite:
			// SQLite parse.y: END | END TRANSACTION [name] (does not treat END WORK as commit).
			if len(tokens) == len(topWords) &&
				(len(topWords) == 1 || (second == "TRANSACTION" && (len(topWords) == 2 || len(topWords) == 3))) {
				return KindCommit, first, false, false, false
			}
			return KindOther, first, false, false, false
		case DialectPostgres:
			// PostgreSQL: END [WORK | TRANSACTION] [AND [NO] CHAIN].
			if len(tokens) == len(topWords) {
				if isCommit, startsNextTx := parsePGEndCommit(topWords); isCommit {
					return KindCommit, first, false, false, startsNextTx
				}
			}
			return KindOther, first, false, false, false
		default:
			// MySQL / MariaDB: standalone END is not a transaction commit; let server report syntax error.
			return KindOther, first, false, false, false
		}
	case "ROLLBACK":
		if second == "TO" || ((second == "WORK" || second == "TRANSACTION") && len(topWords) > 2 && topWords[2] == "TO") {
			return KindRollbackToSavepoint, first, false, false, false
		}
		if second == "PREPARED" {
			return KindOther, first, false, false, false
		}
		startsNextTx := dialect != DialectSQLite && hasAndChain(topWords)
		return KindRollback, first, false, false, startsNextTx
	case "USE":
		return KindUse, first, false, false, false
	case "SET":
		return KindSet, first, false, false, false
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "MERGE", "CALL", "DO", "LOAD", "COPY":
		returnsRows := first == "CALL" || hasWordAtDepth(tokens[1:], baseDepth, "RETURNING")
		return KindWrite, first, true, returnsRows, false
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE", "LOCK", "ANALYZE", "OPTIMIZE", "VACUUM":
		return KindDDL, first, true, false, false
	default:
		if baseDepth == 0 && dialect == DialectMySQL && isMySQLUnsupportedAnonBlockRaw(stmt) {
			return KindOther, first, true, false, false
		}
		return KindOther, first, false, false, false
	}
}

// hasAndChain reports whether top-level words contain "AND CHAIN" (and NOT "AND NO CHAIN").
func hasAndChain(words []string) bool {
	for i := 1; i+1 < len(words); i++ {
		if words[i] == "AND" && words[i+1] == "CHAIN" {
			return true
		}
	}
	return false
}

// parsePGEndCommit parses PostgreSQL's official END syntax:
//
//	END [ WORK | TRANSACTION ] [ AND [ NO ] CHAIN ]
func parsePGEndCommit(words []string) (isCommit bool, startsNextTx bool) {
	if len(words) == 0 || words[0] != "END" {
		return false, false
	}
	idx := 1
	if idx < len(words) && (words[idx] == "WORK" || words[idx] == "TRANSACTION") {
		idx++
	}
	if idx == len(words) {
		return true, false
	}
	if words[idx] != "AND" {
		return false, false
	}
	idx++
	if idx < len(words) && words[idx] == "NO" {
		idx++
		if idx == len(words)-1 && words[idx] == "CHAIN" {
			return true, false
		}
		return false, false
	}
	if idx == len(words)-1 && words[idx] == "CHAIN" {
		return true, true
	}
	return false, false
}

// IsReadOnlyEscapeSet detects SET or SQLite PRAGMA statements trying to disable session read-only mode.
func IsReadOnlyEscapeSet(stmt string, dialect Dialect) bool {
	upper := strings.Join(strings.Fields(strings.ToUpper(stripCommentsAndStrings(stmt, dialect))), " ")
	if dialect == DialectSQLite && strings.HasPrefix(upper, "PRAGMA") {
		return strings.Contains(upper, "QUERY_ONLY") || strings.Contains(upper, "WRITABLE_SCHEMA")
	}
	if !strings.HasPrefix(upper, "SET") {
		return false
	}
	return strings.Contains(upper, "TRANSACTION_READ_ONLY") ||
		strings.Contains(upper, "TX_READ_ONLY") ||
		strings.Contains(upper, "READ WRITE") ||
		strings.Contains(upper, "DEFAULT_TRANSACTION_READ_ONLY")
}

// ExtractUseDatabase parses "USE `dbname`" or "USE dbname" for MySQL session tracking.
func ExtractUseDatabase(stmt string) string {
	trimmed := strings.TrimSpace(stmt)
	trimmed = strings.TrimSuffix(trimmed, ";")
	fields := strings.Fields(trimmed)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "USE") {
		return ""
	}
	db := strings.Trim(fields[1], "`\"'")
	return db
}

func parseDollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	r, size := utf8.DecodeRuneInString(s[1:])
	if !isPGDollarTagStart(r, size) {
		return "", false
	}
	for i := 1 + size; i < len(s); {
		if s[i] == '$' {
			return s[:i+1], true
		}
		nr, nsize := utf8.DecodeRuneInString(s[i:])
		if !isPGDollarTagPart(nr, nsize) {
			return "", false
		}
		i += nsize
	}
	return "", false
}

// PostgreSQL scan.l: dolq_start = [A-Za-z\200-\377_], dolq_cont = [A-Za-z\200-\377_0-9].
func isPGDollarTagStart(r rune, size int) bool {
	if r == utf8.RuneError && size == 1 {
		return false
	}
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r >= 0x80
}

func isPGDollarTagPart(r rune, size int) bool {
	if r == utf8.RuneError && size == 1 {
		return false
	}
	return isPGDollarTagStart(r, size) || (r >= '0' && r <= '9')
}

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdentPart(r rune) bool  { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func isDialectIdentStart(dialect Dialect, r rune, size int) bool {
	if dialect == DialectPostgres {
		return isPGDollarTagStart(r, size)
	}
	return isIdentStart(r)
}

func isDialectIdentPart(dialect Dialect, r rune, size int) bool {
	if dialect == DialectPostgres {
		return isPGDollarTagPart(r, size)
	}
	return isIdentPart(r)
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func lineAtOffset(src string, offset int) int {
	if offset > len(src) {
		offset = len(src)
	}
	return 1 + strings.Count(src[:offset], "\n")
}

func stripCommentsAndStrings(s string, dialect Dialect) string {
	var b strings.Builder
	b.Grow(len(s))
	n := len(s)
	inExecComment := false
	prevWasIdent := false
	for i := 0; i < n; {
		ch := s[i]
		if inExecComment && ch == '*' && i+1 < n && s[i+1] == '/' {
			inExecComment = false
			i += 2
			b.WriteByte(' ')
			prevWasIdent = false
			continue
		}
		if ch == '-' && i+1 < n && s[i+1] == '-' && (i+2 == n || s[i+2] == ' ' || s[i+2] == '\t' || s[i+2] == '\r' || s[i+2] == '\n' || s[i+2] == '\f' || s[i+2] == '\v' || dialect != DialectMySQL) {
			for i < n && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			prevWasIdent = false
			continue
		}
		if dialect == DialectMySQL && ch == '#' {
			for i < n && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			prevWasIdent = false
			continue
		}
		if ch == '/' && i+1 < n && s[i+1] == '*' {
			if !inExecComment {
				if prefixLen := mysqlExecCommentPrefixLen(s, i, dialect); prefixLen > 0 {
					inExecComment = true
					i += prefixLen
					b.WriteByte(' ')
					prevWasIdent = false
					continue
				}
			}
			depth := 1
			i += 2
			for i < n && depth > 0 {
				if dialect == DialectPostgres && s[i] == '/' && i+1 < n && s[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if s[i] == '*' && i+1 < n && s[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			b.WriteByte(' ')
			prevWasIdent = false
			continue
		}
		// Preserve backtick-quoted identifiers verbatim (MySQL / SQLite) so comment markers
		// such as '--' or '#' inside quoted labels/identifiers are never stripped as comments.
		if ch == '`' && dialect != DialectPostgres {
			start := i
			i++
			for i < n {
				if s[i] == '`' {
					if i+1 < n && s[i+1] == '`' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString(s[start:i])
			prevWasIdent = true
			continue
		}
		if next, _, ok := scanSingleQuotedString(s, i, dialect, prevWasIdent); ok {
			i = next
			b.WriteByte(' ')
			prevWasIdent = false
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		prevWasIdent = isDialectIdentPart(dialect, r, size) || r == '$'
		i += size
	}
	return b.String()
}

// scanSingleQuotedString scans a single-quoted string literal starting at s[start], including
// PostgreSQL E'...' / e'...' escape string literals and PostgreSQL multiline continuations
// (where subsequent '...' segments separated by whitespace / '--' comments containing at least
// one newline inherit the initial escape-string mode).
// Returns the byte offset after the closing quote, the count of '\n' bytes consumed, and ok=true.
func scanSingleQuotedString(s string, start int, dialect Dialect, prevWasIdent bool) (next int, newlines int, ok bool) {
	n := len(s)
	if start >= n {
		return start, 0, false
	}
	ch := s[start]
	isPGEscape := dialect == DialectPostgres && !prevWasIdent && (ch == 'E' || ch == 'e') && start+1 < n && s[start+1] == '\''
	if ch != '\'' && !isPGEscape {
		return start, 0, false
	}

	i := start + 1
	if isPGEscape {
		i = start + 2
	}
	allowBackslashEscape := dialect == DialectMySQL || isPGEscape

	for {
		closed := false
		for i < n {
			if s[i] == '\n' {
				newlines++
			}
			if allowBackslashEscape && s[i] == '\\' && i+1 < n {
				if s[i+1] == '\n' {
					newlines++
				}
				i += 2
				continue
			}
			if s[i] == '\'' {
				if i+1 < n && s[i+1] == '\'' {
					i += 2
					continue
				}
				i++
				closed = true
				break
			}
			i++
		}
		if !closed || dialect != DialectPostgres {
			return i, newlines, true
		}

		// PostgreSQL continues a string constant across lines when separated by whitespace and/or
		// '--' comments containing at least one newline ('\n' or '\r'). The continuation segment
		// inherits allowBackslashEscape from the opening E'...' / '...' segment.
		j := i
		sepNewlines := 0
		sawNewline := false
		for j < n {
			switch s[j] {
			case '\n':
				sawNewline = true
				sepNewlines++
				j++
			case '\r':
				sawNewline = true
				j++
			case ' ', '\t', '\f', '\v':
				j++
			case '-':
				if j+1 < n && s[j+1] == '-' {
					j += 2
					for j < n && s[j] != '\n' && s[j] != '\r' {
						j++
					}
					continue
				}
				goto noContinuation
			default:
				goto noContinuation
			}
		}
	noContinuation:
		if sawNewline && j < n && s[j] == '\'' {
			newlines += sepNewlines
			i = j + 1
			continue
		}
		return i, newlines, true
	}
}

// scanMySQLAtVar scans an unquoted MySQL/MariaDB user or system variable token starting at '@'
// (e.g. @do, @a.do, @@session.var). Quoted forms (@'...', @"...", @`...`) are not matched here
// because their following quote is already scanned as a literal or quoted identifier token.
func scanMySQLAtVar(s string, start int) (next int, isSysVar bool, ok bool) {
	n := len(s)
	if start >= n || s[start] != '@' {
		return start, false, false
	}
	j := start + 1
	if j < n && s[j] == '@' {
		isSysVar = true
		j++
	}
	if j >= n {
		return start, false, false
	}
	r, size := utf8.DecodeRuneInString(s[j:])
	if !(isIdentStart(r) || (r >= '0' && r <= '9') || r == '$' || r == '.') {
		return start, false, false
	}
	j += size
	for j < n {
		nr, nsize := utf8.DecodeRuneInString(s[j:])
		if isIdentPart(nr) || nr == '$' || nr == '.' {
			j += nsize
			continue
		}
		break
	}
	return j, isSysVar, true
}
