package sqlsplit

import (
	"strings"
	"unicode/utf8"
)

// DDLScope classifies which Explorer metadata level a DDL statement mutates.
type DDLScope string

const (
	DDLScopeNone     DDLScope = ""
	DDLScopeDatabase DDLScope = "database"
	DDLScopeSchema   DDLScope = "schema"
	DDLScopeObject   DDLScope = "object"
)

// DDLObjectTarget identifies an additional affected (database, schema) container,
// such as the destination database of a MySQL cross-database RENAME TABLE statement.
type DDLObjectTarget struct {
	DB     string
	Schema string
}

// DDLAction describes a parsed structural DDL statement so the session manager and UI
// can release target-database metadata pools before DROP DATABASE and refresh affected Explorer nodes.
//
// SAFETY INVARIANT: For DROP DATABASE (IsDropDB == true), TargetName is populated ONLY when the
// database identifier is unambiguously parsed under the active dialect's lexical rules. When a
// statement uses constructs this lightweight scanner cannot safely decode (e.g. PostgreSQL U&"..."
// Unicode escapes, unquoted non-ASCII identifiers, unclosed quotes, or unexpected trailing tokens),
// TargetName is intentionally left empty so callers NEVER guess a database name to evict a metadata pool.
type DDLAction struct {
	Scope        DDLScope
	Verb         string // CREATE | DROP | ALTER | RENAME
	TargetName   string // unambiguously normalized database / schema / object name (empty if ambiguous)
	TargetDB     string // explicit database qualifier when present
	TargetSchema string // explicit schema qualifier when present
	ExtraObjects []DDLObjectTarget
	IsDropDB     bool
	HasForce     bool // PostgreSQL DROP DATABASE ... WITH (FORCE)
}

type ddlTokKind int

const (
	ddlTokWord ddlTokKind = iota
	ddlTokQuotedIdent
	ddlTokDot
	ddlTokLParen
	ddlTokRParen
	ddlTokOther
)

type ddlToken struct {
	kind   ddlTokKind
	raw    string // decoded identifier text (for words & quoted idents)
	upper  string // uppercase text for keyword matching (only meaningful for ddlTokWord)
	quote  byte   // '"', '`', or '[' for ddlTokQuotedIdent
	closed bool   // true when ddlTokQuotedIdent had a matching closing quote
	depth  int
}

// InspectDDL parses leading structural DDL statements (DATABASE / SCHEMA / TABLE / VIEW / SEQUENCE).
// It skips SQL comments, preserves quoted identifiers accurately, and strictly rejects ambiguous
// DROP DATABASE target forms rather than guessing a database name.
func InspectDDL(stmt string, dialect Dialect) DDLAction {
	tokens, truncated := scanDDLTokens(stmt, dialect)
	if len(tokens) == 0 {
		return DDLAction{}
	}
	if tokens[0].kind != ddlTokWord {
		return DDLAction{}
	}

	verb := tokens[0].upper
	switch verb {
	case "CREATE", "DROP", "ALTER", "RENAME":
	default:
		return DDLAction{}
	}

	idx := 1

	// Skip modifiers that can appear between CREATE/DROP/ALTER and the object kind keyword.
	// Note: TEMP / TEMPORARY tables live in session-private schemas and do not appear in Explorer.
	isTemp := false
	for idx < len(tokens) && tokens[idx].kind == ddlTokWord {
		w := tokens[idx].upper
		switch w {
		case "OR":
			if idx+1 < len(tokens) && tokens[idx+1].kind == ddlTokWord && tokens[idx+1].upper == "REPLACE" {
				idx += 2
				continue
			}
		case "DEFINER":
			idx++
			continue
		case "TEMP", "TEMPORARY":
			isTemp = true
			idx++
			continue
		case "GLOBAL", "LOCAL", "UNLOGGED", "EXTERNAL", "MATERIALIZED", "RECURSIVE":
			idx++
			continue
		}
		break
	}
	if isTemp {
		return DDLAction{}
	}
	if idx >= len(tokens) || tokens[idx].kind != ddlTokWord {
		return DDLAction{}
	}

	objKind := tokens[idx].upper
	idx++

	// Skip optional IF [NOT] EXISTS / CONCURRENTLY / ONLY before target name.
	for idx < len(tokens) && tokens[idx].kind == ddlTokWord {
		w := tokens[idx].upper
		if w == "IF" {
			if idx+2 < len(tokens) && tokens[idx+1].kind == ddlTokWord && tokens[idx+1].upper == "NOT" && tokens[idx+2].kind == ddlTokWord && tokens[idx+2].upper == "EXISTS" {
				idx += 3
				continue
			}
			if idx+1 < len(tokens) && tokens[idx+1].kind == ddlTokWord && tokens[idx+1].upper == "EXISTS" {
				idx += 2
				continue
			}
		}
		if w == "CONCURRENTLY" || w == "ONLY" {
			idx++
			continue
		}
		break
	}

	switch objKind {
	case "DATABASE":
		if verb == "RENAME" {
			return DDLAction{}
		}
		isDrop := verb == "DROP"
		act := DDLAction{
			Scope:    DDLScopeDatabase,
			Verb:     verb,
			IsDropDB: isDrop,
		}
		if name, hasForce, ok := parseExactDatabaseTarget(tokens, idx, truncated, dialect, isDrop); ok {
			act.TargetName = name
			act.HasForce = hasForce
		}
		return act

	case "SCHEMA":
		if verb == "RENAME" {
			return DDLAction{}
		}
		// In MySQL / MariaDB, SCHEMA is strictly a synonym for DATABASE.
		if dialect == DialectMySQL {
			isDrop := verb == "DROP"
			act := DDLAction{
				Scope:    DDLScopeDatabase,
				Verb:     verb,
				IsDropDB: isDrop,
			}
			if name, _, ok := parseExactDatabaseTarget(tokens, idx, truncated, dialect, isDrop); ok {
				act.TargetName = name
			}
			return act
		}
		// PostgreSQL: CREATE / DROP / ALTER SCHEMA [IF [NOT] EXISTS] [AUTHORIZATION ...] <schema>
		if verb == "CREATE" && idx < len(tokens) && tokens[idx].kind == ddlTokWord && tokens[idx].upper == "AUTHORIZATION" {
			return DDLAction{
				Scope: DDLScopeSchema,
				Verb:  verb,
			}
		}
		name, _ := parseSingleIdent(tokens, idx, dialect)
		return DDLAction{
			Scope:      DDLScopeSchema,
			Verb:       verb,
			TargetName: name,
		}

	case "TABLE", "VIEW", "SEQUENCE":
		parts, next := parseQualifiedIdent(tokens, idx, dialect)
		act := DDLAction{
			Scope: DDLScopeObject,
			Verb:  verb,
		}
		assignQualifiedObjectTarget(&act, parts, dialect)
		if verb == "RENAME" && objKind == "TABLE" {
			parseRenameTableExtraTargets(tokens, next, dialect, &act)
		}
		return act
	}

	// MySQL CREATE [ALGORITHM=...] [DEFINER=...] [SQL SECURITY ...] VIEW ...
	if verb == "CREATE" && dialect == DialectMySQL {
		for i := 1; i < len(tokens); i++ {
			if tokens[i].depth == 0 && tokens[i].kind == ddlTokWord && (tokens[i].upper == "VIEW" || tokens[i].upper == "TABLE") {
				j := i + 1
				if j+2 < len(tokens) && tokens[j].kind == ddlTokWord && tokens[j].upper == "IF" &&
					tokens[j+1].kind == ddlTokWord && tokens[j+1].upper == "NOT" &&
					tokens[j+2].kind == ddlTokWord && tokens[j+2].upper == "EXISTS" {
					j += 3
				}
				parts, _ := parseQualifiedIdent(tokens, j, dialect)
				act := DDLAction{Scope: DDLScopeObject, Verb: verb}
				assignQualifiedObjectTarget(&act, parts, dialect)
				return act
			}
		}
	}

	return DDLAction{}
}

// parseExactDatabaseTarget extracts a database identifier ONLY when it is lexically unambiguous
// for the active dialect (preventing misidentification of PG U&"...", unquoted non-ASCII folding,
// unclosed quotes, or unexpected trailing syntax).
func parseExactDatabaseTarget(tokens []ddlToken, idx int, truncated bool, dialect Dialect, isDrop bool) (string, bool, bool) {
	if idx >= len(tokens) {
		return "", false, false
	}
	if isDrop && truncated {
		return "", false, false
	}

	tok := tokens[idx]
	rest := tokens[idx+1:]

	switch dialect {
	case DialectPostgres:
		var name string
		switch tok.kind {
		case ddlTokWord:
			// PostgreSQL only folds ASCII A-Z -> a-z (and non-ASCII unquoted bytes depend on server locale).
			// Reject unquoted non-ASCII names and reject U&"..." (where next token is '&').
			if !isPureASCII(tok.raw) || len(tok.raw) == 0 || len(tok.raw) > 63 {
				return "", false, false
			}
			name = asciiToLower(tok.raw)
		case ddlTokQuotedIdent:
			if tok.quote != '"' || !tok.closed || len(tok.raw) == 0 || len(tok.raw) > 63 || strings.ContainsRune(tok.raw, 0) {
				return "", false, false
			}
			name = tok.raw
		default:
			return "", false, false
		}

		if isDrop {
			hasForce, validTail := parsePGDropDBTail(rest)
			if !validTail {
				return "", false, false
			}
			return name, hasForce, true
		}

		// For CREATE/ALTER DATABASE, still reject obvious Unicode escape or malformed prefix (U&"...", dot).
		if len(rest) > 0 && ((rest[0].kind == ddlTokOther && rest[0].raw == "&") || rest[0].kind == ddlTokDot || (rest[0].kind == ddlTokWord && rest[0].upper == "UESCAPE")) {
			return "", false, false
		}
		return name, false, true

	case DialectMySQL:
		var name string
		switch tok.kind {
		case ddlTokWord:
			if len(tok.raw) == 0 || utf8.RuneCountInString(tok.raw) > 64 {
				return "", false, false
			}
			name = tok.raw
		case ddlTokQuotedIdent:
			if tok.quote != '`' || !tok.closed || len(tok.raw) == 0 || utf8.RuneCountInString(tok.raw) > 64 || strings.ContainsRune(tok.raw, 0) {
				return "", false, false
			}
			name = tok.raw
		default:
			return "", false, false
		}
		if isDrop && len(rest) != 0 {
			return "", false, false
		}
		return name, false, true

	default:
		return "", false, false
	}
}

// parsePGDropDBTail validates PostgreSQL DROP DATABASE trailing options per PostgreSQL gram.y:
//
//	DROP DATABASE [ IF EXISTS ] name [ [ WITH ] ( FORCE [, ...] ) ]
//
// Returns (hasForce, valid). Any unexpected token (such as `&` from U&"...", extra identifiers,
// UESCAPE, boolean arguments rejected by PG, or unknown clauses) returns valid=false so the
// target name is treated as ambiguous.
func parsePGDropDBTail(rest []ddlToken) (bool, bool) {
	if len(rest) == 0 {
		return false, true
	}
	pos := 0
	if rest[pos].kind == ddlTokWord && rest[pos].upper == "WITH" {
		pos++
		if pos >= len(rest) {
			return false, false
		}
	}
	if rest[pos].kind != ddlTokLParen {
		return false, false
	}
	pos++

	hasForce := false
	expectOption := true
	for pos < len(rest) {
		t := rest[pos]
		if t.kind == ddlTokRParen {
			// Closing ')' must end the statement with no trailing tokens.
			if expectOption || pos != len(rest)-1 {
				return false, false
			}
			return hasForce, true
		}
		if !expectOption {
			if t.kind == ddlTokOther && t.raw == "," {
				expectOption = true
				pos++
				continue
			}
			return false, false
		}
		// PostgreSQL DROP DATABASE option grammar only accepts bare FORCE (no boolean argument).
		if t.kind != ddlTokWord || t.upper != "FORCE" {
			return false, false
		}
		hasForce = true
		pos++
		expectOption = false
	}
	return false, false
}

// parseRenameTableExtraTargets parses MySQL/MariaDB RENAME TABLE destination(s) and additional pairs:
//
//	RENAME TABLE src1 TO dst1 [, src2 TO dst2] ...
func parseRenameTableExtraTargets(tokens []ddlToken, idx int, dialect Dialect, act *DDLAction) {
	addScope := func(parts []string) {
		var tmp DDLAction
		assignQualifiedObjectTarget(&tmp, parts, dialect)
		act.ExtraObjects = append(act.ExtraObjects, DDLObjectTarget{
			DB:     tmp.TargetDB,
			Schema: tmp.TargetSchema,
		})
	}

	// Destination of the first pair: ... TO dst1
	if idx < len(tokens) && tokens[idx].kind == ddlTokWord && tokens[idx].upper == "TO" {
		dstParts, next := parseQualifiedIdent(tokens, idx+1, dialect)
		if len(dstParts) > 0 {
			addScope(dstParts)
		}
		idx = next
	}

	// Optional additional pairs separated by commas: , src2 TO dst2 ...
	for idx < len(tokens) && tokens[idx].kind == ddlTokOther && tokens[idx].raw == "," {
		srcParts, next := parseQualifiedIdent(tokens, idx+1, dialect)
		if len(srcParts) == 0 {
			break
		}
		addScope(srcParts)
		idx = next
		if idx < len(tokens) && tokens[idx].kind == ddlTokWord && tokens[idx].upper == "TO" {
			dstParts, nextDst := parseQualifiedIdent(tokens, idx+1, dialect)
			if len(dstParts) > 0 {
				addScope(dstParts)
			}
			idx = nextDst
		}
	}
}

func assignQualifiedObjectTarget(act *DDLAction, parts []string, dialect Dialect) {
	switch len(parts) {
	case 0:
		return
	case 1:
		act.TargetName = parts[0]
	case 2:
		if dialect == DialectPostgres {
			act.TargetSchema = parts[0]
		} else {
			act.TargetDB = parts[0]
		}
		act.TargetName = parts[1]
	default:
		// 3+ parts: db.schema.object
		act.TargetDB = parts[0]
		act.TargetSchema = parts[1]
		act.TargetName = parts[2]
	}
}

func isPureASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiToLower(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if sb.Len() == 0 {
				sb.Grow(len(s))
				sb.WriteString(s[:i])
			}
			sb.WriteByte(c + ('a' - 'A'))
		} else if sb.Len() > 0 {
			sb.WriteByte(c)
		}
	}
	if sb.Len() == 0 {
		return s
	}
	return sb.String()
}

func normalizeIdent(tok ddlToken, dialect Dialect) string {
	if tok.kind == ddlTokQuotedIdent {
		return tok.raw
	}
	if dialect == DialectPostgres {
		return asciiToLower(tok.raw)
	}
	return tok.raw
}

func parseSingleIdent(tokens []ddlToken, idx int, dialect Dialect) (string, int) {
	if idx >= len(tokens) {
		return "", idx
	}
	if tokens[idx].kind == ddlTokWord || (tokens[idx].kind == ddlTokQuotedIdent && tokens[idx].closed) {
		return normalizeIdent(tokens[idx], dialect), idx + 1
	}
	return "", idx
}

func parseQualifiedIdent(tokens []ddlToken, idx int, dialect Dialect) ([]string, int) {
	var parts []string
	for idx < len(tokens) {
		if tokens[idx].kind != ddlTokWord && !(tokens[idx].kind == ddlTokQuotedIdent && tokens[idx].closed) {
			break
		}
		parts = append(parts, normalizeIdent(tokens[idx], dialect))
		idx++
		if idx < len(tokens) && tokens[idx].kind == ddlTokDot {
			idx++
			continue
		}
		break
	}
	return parts, idx
}

// scanDDLTokens tokenizes the leading portion of a SQL statement while skipping comments/strings
// and decoding double-quoted, backtick-quoted, and SQLite bracket-quoted identifiers.
func scanDDLTokens(s string, dialect Dialect) ([]ddlToken, bool) {
	const maxTokens = 128
	out := make([]ddlToken, 0, 12)
	n := len(s)
	depth := 0
	inExecComment := false
	i := 0

	for i < n {
		ch := s[i]

		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == ';' {
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
		if ch == '-' && i+1 < n && s[i+1] == '-' && (i+2 == n || s[i+2] == ' ' || s[i+2] == '\t' || s[i+2] == '\r' || s[i+2] == '\n' || dialect != DialectMySQL) {
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

		// Block comment /* ... */ (nested in PG; MySQL/MariaDB executable comments expanded)
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

		if len(out) >= maxTokens {
			return out, true
		}

		// Single-quoted string literal (preserve token presence so trailing syntax checks cannot overlook it)
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
			out = append(out, ddlToken{kind: ddlTokOther, raw: "'", depth: depth})
			continue
		}

		// Double-quoted identifier ("foo""bar")
		if ch == '"' {
			i++
			closed := false
			var sb strings.Builder
			for i < n {
				if dialect == DialectMySQL && s[i] == '\\' && i+1 < n {
					sb.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '"' {
					if i+1 < n && s[i+1] == '"' {
						sb.WriteByte('"')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				sb.WriteByte(s[i])
				i++
			}
			out = append(out, ddlToken{kind: ddlTokQuotedIdent, raw: sb.String(), quote: '"', closed: closed, depth: depth})
			continue
		}

		// Backtick-quoted identifier (`foo``bar`)
		if ch == '`' {
			i++
			closed := false
			var sb strings.Builder
			for i < n {
				if s[i] == '`' {
					if i+1 < n && s[i+1] == '`' {
						sb.WriteByte('`')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				sb.WriteByte(s[i])
				i++
			}
			out = append(out, ddlToken{kind: ddlTokQuotedIdent, raw: sb.String(), quote: '`', closed: closed, depth: depth})
			continue
		}

		// SQLite bracket-quoted identifier ([foo])
		if dialect == DialectSQLite && ch == '[' {
			i++
			closed := false
			start := i
			for i < n && s[i] != ']' {
				i++
			}
			raw := s[start:i]
			if i < n && s[i] == ']' {
				i++
				closed = true
			}
			out = append(out, ddlToken{kind: ddlTokQuotedIdent, raw: raw, quote: '[', closed: closed, depth: depth})
			continue
		}

		// PostgreSQL dollar-quoted string ($tag$...$tag$)
		if dialect == DialectPostgres && ch == '$' {
			if tag, ok := parseDollarTag(s[i:]); ok {
				i += len(tag)
				endIdx := strings.Index(s[i:], tag)
				if endIdx < 0 {
					break
				}
				i += endIdx + len(tag)
				continue
			}
		}

		if ch == '.' {
			out = append(out, ddlToken{kind: ddlTokDot, raw: ".", depth: depth})
			i++
			continue
		}
		if ch == '(' {
			depth++
			out = append(out, ddlToken{kind: ddlTokLParen, raw: "(", depth: depth})
			i++
			continue
		}
		if ch == ')' {
			out = append(out, ddlToken{kind: ddlTokRParen, raw: ")", depth: depth})
			if depth > 0 {
				depth--
			}
			i++
			continue
		}

		if isIdentStart(rune(ch)) || ch >= 0x80 {
			start := i
			i++
			for i < n && (isIdentPart(rune(s[i])) || s[i] == '$' || s[i] >= 0x80) {
				i++
			}
			raw := s[start:i]
			out = append(out, ddlToken{kind: ddlTokWord, raw: raw, upper: strings.ToUpper(raw), depth: depth})
			continue
		}

		out = append(out, ddlToken{kind: ddlTokOther, raw: string(ch), depth: depth})
		i++
	}

	return out, false
}
