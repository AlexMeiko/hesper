package sqlsplit

import (
	"strings"
	"unicode"
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
	KindQuery    StatementKind = "query"    // SELECT, SHOW, DESCRIBE, EXPLAIN, VALUES, TABLE, WITH ... SELECT
	KindBegin    StatementKind = "begin"    // BEGIN, START TRANSACTION
	KindCommit   StatementKind = "commit"   // COMMIT
	KindRollback StatementKind = "rollback" // ROLLBACK
	KindUse      StatementKind = "use"      // USE db (MySQL)
	KindSet      StatementKind = "set"      // SET ...
	KindWrite    StatementKind = "write"    // INSERT, UPDATE, DELETE, REPLACE, MERGE, CALL, DO, LOAD
	KindDDL      StatementKind = "ddl"      // CREATE, ALTER, DROP, TRUNCATE, RENAME, GRANT, REVOKE
	KindOther    StatementKind = "other"
)

// Statement represents one lexically separated SQL statement with source coordinates.
type Statement struct {
	Text        string
	StartOffset int
	EndOffset   int
	StartLine   int
	EndLine     int
	Kind        StatementKind
	FirstWord   string
	MutatesData bool // true for write/DDL/CALL/CTE-with-write statements
}

// Split splits SQL text into individual statements using a dialect-aware state machine.
// Handles single/double/backtick quotes, escaped quotes, line comments (-- and MySQL #),
// block comments (nested in PG), PostgreSQL $tag$...$tag$ dollar quoting, and MySQL DELIMITER.
func Split(src string, dialect Dialect) []Statement {
	var stmts []Statement
	delimiter := ";"
	n := len(src)

	stmtStart := -1
	stmtStartLine := 1
	line := 1

	flush := func(end int) {
		if stmtStart < 0 || end <= stmtStart {
			stmtStart = -1
			return
		}
		raw := src[stmtStart:end]
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || isOnlyComments(trimmed, dialect) {
			stmtStart = -1
			return
		}
		leadTrim := strings.Index(raw, trimmed)
		realStart := stmtStart + leadTrim
		realEnd := realStart + len(trimmed)
		realStartLine := lineAtOffset(src, realStart)
		realEndLine := lineAtOffset(src, realEnd)
		kind, firstWord, mutates := Classify(trimmed, dialect)
		stmts = append(stmts, Statement{
			Text:        trimmed,
			StartOffset: realStart,
			EndOffset:   realEnd,
			StartLine:   realStartLine,
			EndLine:     realEndLine,
			Kind:        kind,
			FirstWord:   firstWord,
			MutatesData: mutates,
		})
		stmtStart = -1
	}

	i := 0
	atLineStart := true
	for i < n {
		// MySQL DELIMITER directive at line start (outside any open quote/comment)
		if dialect == DialectMySQL && atLineStart && stmtStart < 0 {
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
				atLineStart = true
				continue
			}
		}

		// Match current delimiter
		if delimiter != "" && strings.HasPrefix(src[i:], delimiter) {
			flush(i)
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
		if ch == ' ' || ch == '\t' || ch == '\r' {
			i++
			continue
		}

		// Single-line comment: -- ...
		if ch == '-' && i+1 < n && src[i+1] == '-' && (i+2 == n || src[i+2] == ' ' || src[i+2] == '\t' || src[i+2] == '\r' || src[i+2] == '\n' || dialect != DialectMySQL) {
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

		// Block comment /* ... */ (nested in PostgreSQL)
		if ch == '/' && i+1 < n && src[i+1] == '*' {
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
		atLineStart = false

		// Single-quoted string
		if ch == '\'' {
			i++
			for i < n {
				if src[i] == '\n' {
					line++
				}
				if dialect == DialectMySQL && src[i] == '\\' && i+1 < n {
					if src[i+1] == '\n' {
						line++
					}
					i += 2
					continue
				}
				if src[i] == '\'' {
					if i+1 < n && src[i+1] == '\'' {
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

		// Double-quoted identifier or string
		if ch == '"' {
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

		// PostgreSQL dollar-quoting: $$...$$ or $tag$...$tag$
		if dialect == DialectPostgres && ch == '$' {
			if tag, ok := parseDollarTag(src[i:]); ok {
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

		i++
	}

	flush(n)
	return stmts
}

// StatementAtCursor returns the statement containing cursorOffset, or the nearest preceding statement.
func StatementAtCursor(src string, cursorOffset int, dialect Dialect) (Statement, bool) {
	stmts := Split(src, dialect)
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

// Classify inspects leading keywords (skipping comments) to classify statement intent.
func Classify(stmt string, dialect Dialect) (StatementKind, string, bool) {
	tokens := extractLeadingWords(stmt, dialect, 8)
	if len(tokens) == 0 {
		return KindOther, "", false
	}
	first := tokens[0]
	second := ""
	if len(tokens) > 1 {
		second = tokens[1]
	}

	switch first {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "VALUES", "TABLE", "PRAGMA":
		// PRAGMA with '=' in SQLite can mutate settings
		if first == "PRAGMA" && strings.Contains(stmt, "=") {
			return KindSet, first, false
		}
		return KindQuery, first, false
	case "WITH":
		// CTE could lead to INSERT/UPDATE/DELETE (data-modifying CTE in PG/MySQL)
		upper := strings.ToUpper(stripCommentsAndStrings(stmt, dialect))
		if containsWord(upper, "INSERT") || containsWord(upper, "UPDATE") || containsWord(upper, "DELETE") || containsWord(upper, "MERGE") {
			return KindWrite, first, true
		}
		return KindQuery, first, false
	case "BEGIN":
		return KindBegin, first, false
	case "START":
		if second == "TRANSACTION" {
			return KindBegin, first, false
		}
		return KindOther, first, false
	case "COMMIT", "END":
		return KindCommit, first, false
	case "ROLLBACK":
		return KindRollback, first, false
	case "USE":
		return KindUse, first, false
	case "SET":
		return KindSet, first, false
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "MERGE", "CALL", "DO", "LOAD", "COPY":
		return KindWrite, first, true
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE", "LOCK", "ANALYZE", "OPTIMIZE", "VACUUM":
		return KindDDL, first, true
	default:
		return KindOther, first, false
	}
}

// IsReadOnlyEscapeSet detects SET or SQLite PRAGMA statements trying to disable session read-only mode.
func IsReadOnlyEscapeSet(stmt string, dialect Dialect) bool {
	upper := strings.TrimSpace(strings.ToUpper(stripCommentsAndStrings(stmt, dialect)))
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
	if !isIdentStart(rune(s[1])) {
		return "", false
	}
	for i := 2; i < len(s); i++ {
		if s[i] == '$' {
			return s[:i+1], true
		}
		if !isIdentPart(rune(s[i])) {
			return "", false
		}
	}
	return "", false
}

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdentPart(r rune) bool  { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func lineAtOffset(src string, offset int) int {
	if offset > len(src) {
		offset = len(src)
	}
	return 1 + strings.Count(src[:offset], "\n")
}

func isOnlyComments(s string, dialect Dialect) bool {
	return len(extractLeadingWords(s, dialect, 1)) == 0
}

func stripCommentsAndStrings(s string, dialect Dialect) string {
	var b strings.Builder
	b.Grow(len(s))
	n := len(s)
	for i := 0; i < n; {
		ch := s[i]
		if ch == '-' && i+1 < n && s[i+1] == '-' {
			for i < n && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			continue
		}
		if dialect == DialectMySQL && ch == '#' {
			for i < n && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			continue
		}
		if ch == '/' && i+1 < n && s[i+1] == '*' {
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
			continue
		}
		if ch == '\'' {
			i++
			for i < n {
				if dialect == DialectMySQL && s[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if s[i] == '\'' {
					if i+1 < n && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(ch)
		i++
	}
	return b.String()
}

func extractLeadingWords(s string, dialect Dialect, maxWords int) []string {
	clean := stripCommentsAndStrings(s, dialect)
	var words []string
	var cur strings.Builder
	for _, r := range clean {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(unicode.ToUpper(r))
		} else if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
			if len(words) >= maxWords {
				return words
			}
		}
	}
	if cur.Len() > 0 && len(words) < maxWords {
		words = append(words, cur.String())
	}
	return words
}

func containsWord(upper, word string) bool {
	idx := 0
	for {
		pos := strings.Index(upper[idx:], word)
		if pos < 0 {
			return false
		}
		start := idx + pos
		end := start + len(word)
		leftOK := start == 0 || !isIdentPart(rune(upper[start-1]))
		rightOK := end == len(upper) || !isIdentPart(rune(upper[end]))
		if leftOK && rightOK {
			return true
		}
		idx = start + 1
	}
}
