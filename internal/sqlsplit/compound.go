package sqlsplit

import (
	"strings"
	"unicode/utf8"
)

const (
	errSQLiteUnclosedTrigger       = "SQLite 触发器定义缺少闭合的 END，已阻止拆分执行"
	errPGUnclosedAtomicBody        = "PostgreSQL BEGIN ATOMIC 语句体缺少闭合的 END，已阻止拆分执行"
	errMySQLUnclosedCompound       = "MySQL/MariaDB 复合语句块缺少闭合的 END，已阻止拆分执行"
	errMySQLNeedsDelimiter         = "检测到未使用 DELIMITER 的 MySQL/MariaDB 复合语句或边界不明确的可执行注释，请使用 DELIMITER 声明语句结束符后再执行"
	errMariaDBAnonBlockUnsupported = "暂不支持 MariaDB 匿名复合块 (BEGIN NOT ATOMIC / IF / CASE / LOOP / WHILE / REPEAT / FOR)，请改用存储过程配合 DELIMITER 或拆分为独立语句执行"
)

type compoundMode uint8

const (
	modeInit compoundMode = iota
	modeNormal

	// SQLite: [EXPLAIN [QUERY PLAN]] CREATE [TEMP|TEMPORARY] TRIGGER ... BEGIN ... ; END ;
	modeSQLiteExplain
	modeSQLiteExplainQuery
	modeSQLiteAfterExplainPlan
	modeSQLiteCreate
	modeSQLiteTriggerHeader
	modeSQLiteTriggerBody
	modeSQLiteTriggerEnded

	// PostgreSQL: CREATE [OR REPLACE] FUNCTION|PROCEDURE ... BEGIN ATOMIC ... END ;
	modePGCreate
	modePGCreateOr
	modePGRoutineHeader
	modePGBeginCandidate
	modePGAtomicBody
	modePGAtomicEnded

	// MySQL / MariaDB:
	//   - Anonymous blocks: BEGIN NOT ATOMIC / IF / WHILE / FOR / REPEAT / LOOP / CASE (with or without lbl:)
	//   - CREATE [OR REPLACE] [DEFINER=...] (TRIGGER|PROCEDURE|FUNCTION|EVENT) ... <body_start>
	//   - ALTER [DEFINER=...] EVENT ... DO <body_start>
	modeMySQLLabelCandidate
	modeMySQLLabelAfterColon
	modeMySQLBeginCheck
	modeMySQLBeginNot
	modeMySQLCreate
	modeMySQLCreateOr
	modeMySQLCreateAggregate
	modeMySQLDefiner
	modeMySQLAlter
	modeMySQLTriggerHeader
	modeMySQLTriggerAfterRow
	modeMySQLTriggerSkipOrderName
	modeMySQLEventHeader
	modeMySQLProcHeader
	modeMySQLFuncHeader
	modeMySQLCharacteristics
	modeMySQLBodyStart
	modeMySQLBodyLabelOrSimple
	modeMySQLCompoundBody
	modeMySQLCompoundEnded
	modeMySQLUnsupportedCompound
	modeMySQLUnsupportedAnonymous
)

type mysqlBeginLookahead uint8

const (
	mysqlBeginNone mysqlBeginLookahead = iota
	mysqlBeginSawBegin
	mysqlBeginSawNot
)

type definerPhase uint8

const (
	defWaitEq definerPhase = iota
	defWaitUser
	defAfterUser
	defInCurrentUserParens
	defWaitHost
	defInHost
)

type eventHeaderPhase uint8

const (
	eventWaitNameOrIf eventHeaderPhase = iota
	eventAfterIf
	eventAfterIfNot
	eventWaitName
	eventAfterFirstName
	eventWaitQualName
	eventOptions
	eventAfterRename
)

type procHeaderPhase uint8

const (
	procWaitParams procHeaderPhase = iota
	procInParams
)

type funcHeaderPhase uint8

const (
	funcWaitParams funcHeaderPhase = iota
	funcInParams
	funcWaitReturns
	funcInReturnType
)

// compoundTracker tracks dialect-specific compound statement boundaries when splitting with
// the default ';' delimiter so inner semicolons inside triggers/routines never split into
// standalone executable statements.
type compoundTracker struct {
	dialect    Dialect
	mode       compoundMode
	parenDepth int

	// SQLite trigger state (matches SQLite src/complete.c "; END ;" rule).
	sqliteLastWasSemi bool

	// PostgreSQL BEGIN ATOMIC state: closing END is only valid at statement start (after BEGIN ATOMIC or ';').
	pgAtStmtStart bool

	// MySQL / MariaDB state.
	definerReturnMode          compoundMode
	definerPhase               definerPhase
	eventPhase                 eventHeaderPhase
	triggerSeq                 int // 0=none, 1=FOR, 2=FOR EACH
	procPhase                  procHeaderPhase
	funcPhase                  funcHeaderPhase
	funcSawTypeName            bool
	funcSkipNextIdent          bool
	funcPrevWord               string
	charSkipNext               bool
	mysqlBeginDepth            int
	mysqlAtStmtStart           bool
	mysqlStmtFirstWord         string
	mysqlStmtWordCount         int
	mysqlInDeclareHandler      bool
	mysqlExprCaseDepth         int
	mysqlAfterDot              bool
	mysqlPendingEnd            bool
	mysqlBeginLook             mysqlBeginLookahead
	mysqlIsAnonBlock           bool
	execCommentSawTrailingSemi bool
	execCommentAmbiguous       bool
}

func newCompoundTracker(dialect Dialect) compoundTracker {
	return compoundTracker{dialect: dialect, mode: modeInit}
}

func (t *compoundTracker) reset() {
	d := t.dialect
	*t = compoundTracker{dialect: d, mode: modeInit}
}

func (t *compoundTracker) enterMySQLDefiner(returnMode compoundMode) {
	t.mode = modeMySQLDefiner
	t.definerReturnMode = returnMode
	t.definerPhase = defWaitEq
}

func (t *compoundTracker) enterMySQLEventHeader(allowIfNotExists bool) {
	t.mode = modeMySQLEventHeader
	if allowIfNotExists {
		t.eventPhase = eventWaitNameOrIf
	} else {
		t.eventPhase = eventWaitName
	}
}

func (t *compoundTracker) enterMySQLCompoundBlock(isAnon bool) {
	t.mode = modeMySQLCompoundBody
	t.mysqlBeginDepth = 1
	t.mysqlAtStmtStart = true
	t.mysqlStmtFirstWord = ""
	t.mysqlStmtWordCount = 0
	t.mysqlInDeclareHandler = false
	t.mysqlExprCaseDepth = 0
	t.mysqlAfterDot = false
	t.mysqlPendingEnd = false
	t.mysqlBeginLook = mysqlBeginNone
	t.mysqlIsAnonBlock = isAnon
}

func (t *compoundTracker) resetMySQLInnerStmt() {
	t.mysqlAtStmtStart = true
	t.mysqlStmtFirstWord = ""
	t.mysqlStmtWordCount = 0
	t.mysqlInDeclareHandler = false
	t.mysqlExprCaseDepth = 0
	t.mysqlAfterDot = false
}

// noteStmtToken marks the current statement ambiguous if a ';' already ended a statement
// inside an executable comment (/*!...*/ / /*M!...*/) and another token appears before the
// outer delimiter flushes the statement.
func (t *compoundTracker) noteStmtToken() {
	if t.execCommentSawTrailingSemi {
		t.execCommentAmbiguous = true
	}
}

func (t *compoundTracker) onLParen() {
	t.mysqlAfterDot = false
	if t.resolveMySQLPending(eventLParen, "") {
		t.parenDepth++
		return
	}
	if t.mode == modeMySQLDefiner && t.definerPhase == defAfterUser && t.parenDepth == 0 {
		// CURRENT_USER() parentheses inside DEFINER = CURRENT_USER()
		t.parenDepth++
		t.definerPhase = defInCurrentUserParens
		return
	}
	if t.mode == modeMySQLDefiner {
		t.mode = modeNormal
	}
	if t.mode == modeMySQLProcHeader && t.procPhase == procWaitParams && t.parenDepth == 0 {
		t.parenDepth++
		t.procPhase = procInParams
		return
	}
	if t.mode == modeMySQLFuncHeader && t.funcPhase == funcWaitParams && t.parenDepth == 0 {
		t.parenDepth++
		t.funcPhase = funcInParams
		return
	}
	t.parenDepth++
	if t.mode == modeSQLiteTriggerBody {
		t.sqliteLastWasSemi = false
	}
	if t.mode == modePGAtomicBody {
		t.pgAtStmtStart = false
	}
	if t.mode == modeMySQLCompoundBody {
		t.mysqlAtStmtStart = false
		t.mysqlStmtWordCount++
	}
	if t.mode == modeMySQLBodyLabelOrSimple || t.mode == modeMySQLLabelCandidate ||
		t.mode == modeMySQLLabelAfterColon || t.mode == modeMySQLBeginCheck || t.mode == modeMySQLBeginNot ||
		t.mode == modeMySQLCreateAggregate || t.mode == modeMySQLAlter ||
		t.mode == modeSQLiteExplain || t.mode == modeSQLiteExplainQuery || t.mode == modeSQLiteAfterExplainPlan {
		t.mode = modeNormal
	}
}

func (t *compoundTracker) onRParen() {
	t.mysqlAfterDot = false
	t.resolveMySQLPending(eventRParen, "")
	if t.parenDepth > 0 {
		t.parenDepth--
	}
	if t.mode == modeMySQLDefiner && t.definerPhase == defInCurrentUserParens && t.parenDepth == 0 {
		t.mode = t.definerReturnMode
		return
	}
	if t.mode == modeMySQLProcHeader && t.procPhase == procInParams && t.parenDepth == 0 {
		t.mode = modeMySQLCharacteristics
		t.charSkipNext = false
		return
	}
	if t.mode == modeMySQLFuncHeader && t.funcPhase == funcInParams && t.parenDepth == 0 {
		t.funcPhase = funcWaitReturns
		return
	}
	if t.mode == modeSQLiteTriggerBody {
		t.sqliteLastWasSemi = false
	}
	if t.mode == modePGAtomicBody {
		t.pgAtStmtStart = false
	}
	if t.mode == modeMySQLCompoundBody {
		t.mysqlAtStmtStart = false
	}
}

func (t *compoundTracker) onColon() {
	t.mysqlAfterDot = false
	if t.resolveMySQLPending(eventColon, "") {
		return
	}
	switch t.mode {
	case modeSQLiteTriggerBody:
		t.sqliteLastWasSemi = false
	case modePGAtomicBody:
		t.pgAtStmtStart = false
	case modeMySQLLabelCandidate:
		t.mode = modeMySQLLabelAfterColon
	case modeMySQLBodyLabelOrSimple:
		t.mode = modeMySQLBodyStart
	case modeMySQLCompoundBody:
		// Labeled nested block or loop inside BEGIN ... END (e.g. inner_lbl: BEGIN ... END inner_lbl;).
		if t.parenDepth == 0 && t.mysqlStmtWordCount == 1 {
			t.resetMySQLInnerStmt()
		} else {
			t.mysqlAtStmtStart = false
		}
	case modeMySQLTriggerAfterRow, modeMySQLBeginCheck, modeMySQLBeginNot, modeMySQLLabelAfterColon,
		modeMySQLCreateAggregate, modeMySQLAlter, modeMySQLDefiner,
		modeSQLiteExplain, modeSQLiteExplainQuery, modeSQLiteAfterExplainPlan:
		t.mode = modeNormal
	}
}

func (t *compoundTracker) onSymbol(ch byte) {
	if t.resolveMySQLPending(eventSymbol, "") {
		t.mysqlAfterDot = (ch == '.')
		return
	}
	t.mysqlAfterDot = (ch == '.')
	switch t.mode {
	case modeSQLiteTriggerBody:
		t.sqliteLastWasSemi = false
	case modePGAtomicBody:
		t.pgAtStmtStart = false
	case modeMySQLCompoundBody:
		t.mysqlAtStmtStart = false
		t.mysqlStmtWordCount++
	case modeMySQLDefiner:
		if t.parenDepth > 0 {
			return
		}
		switch t.definerPhase {
		case defWaitEq:
			if ch == '=' {
				t.definerPhase = defWaitUser
			} else {
				t.mode = modeNormal
			}
		case defAfterUser:
			if ch == '@' {
				t.definerPhase = defWaitHost
			} else {
				t.mode = t.definerReturnMode
				t.onSymbol(ch)
			}
		case defWaitHost:
			if ch == '%' {
				t.definerPhase = defInHost
			} else {
				t.mode = modeNormal
			}
		case defInHost:
			switch ch {
			case '.', '-':
				t.definerPhase = defWaitHost
			case '%':
				// trailing wildcard stays in defInHost
			default:
				t.mode = t.definerReturnMode
				t.onSymbol(ch)
			}
		default:
			t.mode = modeNormal
		}
	case modeMySQLEventHeader:
		if t.parenDepth == 0 && ch == '.' && t.eventPhase == eventAfterFirstName {
			t.eventPhase = eventWaitQualName
		}
	case modeMySQLTriggerAfterRow, modeMySQLBodyLabelOrSimple, modeMySQLLabelCandidate, modeMySQLLabelAfterColon,
		modeMySQLBeginCheck, modeMySQLBeginNot, modeMySQLCreate, modeMySQLCreateOr, modeMySQLCreateAggregate, modeMySQLAlter,
		modeSQLiteCreate, modeSQLiteExplain, modeSQLiteExplainQuery, modeSQLiteAfterExplainPlan,
		modePGCreate, modePGCreateOr:
		t.mode = modeNormal
	case modePGBeginCandidate:
		t.mode = modePGRoutineHeader
	}
}

func (t *compoundTracker) onLiteralOrQuotedIdent() {
	t.mysqlAfterDot = false
	if t.resolveMySQLPending(eventLiteral, "") {
		return
	}
	switch t.mode {
	case modeInit:
		if t.dialect == DialectMySQL {
			t.mode = modeMySQLLabelCandidate
		} else {
			t.mode = modeNormal
		}
	case modeSQLiteCreate, modeSQLiteExplain, modeSQLiteExplainQuery, modeSQLiteAfterExplainPlan,
		modePGCreate, modePGCreateOr, modeMySQLCreate, modeMySQLCreateOr, modeMySQLCreateAggregate, modeMySQLAlter,
		modeMySQLLabelCandidate, modeMySQLLabelAfterColon, modeMySQLBeginCheck, modeMySQLBeginNot,
		modeMySQLBodyLabelOrSimple:
		t.mode = modeNormal
	case modeSQLiteTriggerBody:
		t.sqliteLastWasSemi = false
	case modePGAtomicBody:
		t.pgAtStmtStart = false
	case modePGBeginCandidate:
		t.mode = modePGRoutineHeader
	case modeMySQLCompoundBody:
		if t.parenDepth == 0 {
			if t.mysqlAtStmtStart {
				t.mysqlStmtFirstWord = ""
			}
			t.mysqlAtStmtStart = false
			t.mysqlStmtWordCount++
		}
	case modeMySQLDefiner:
		if t.parenDepth > 0 {
			return
		}
		switch t.definerPhase {
		case defWaitUser:
			t.definerPhase = defAfterUser
		case defWaitHost:
			t.definerPhase = defInHost
		default:
			t.mode = modeNormal
		}
	case modeMySQLEventHeader:
		if t.parenDepth > 0 {
			return
		}
		switch t.eventPhase {
		case eventWaitNameOrIf, eventWaitName:
			t.eventPhase = eventAfterFirstName
		case eventWaitQualName:
			t.eventPhase = eventOptions
		case eventAfterFirstName, eventAfterIf, eventAfterIfNot, eventAfterRename:
			t.eventPhase = eventOptions
		}
	case modeMySQLTriggerHeader:
		t.triggerSeq = 0
	case modeMySQLTriggerAfterRow:
		t.mode = modeMySQLBodyLabelOrSimple
	case modeMySQLTriggerSkipOrderName:
		t.mode = modeMySQLBodyStart
	case modeMySQLFuncHeader:
		if t.funcPhase == funcInReturnType && t.parenDepth == 0 {
			if t.funcSkipNextIdent {
				t.funcSkipNextIdent = false
			} else if !t.funcSawTypeName {
				t.funcSawTypeName = true
			} else {
				t.mode = modeMySQLBodyStart
				t.onLiteralOrQuotedIdent()
			}
		}
	case modeMySQLCharacteristics:
		if t.parenDepth == 0 {
			if t.charSkipNext {
				t.charSkipNext = false
			} else {
				// Could be a backtick-quoted outermost block label (e.g. `blk`: BEGIN ... END `blk`);
				// let the next ':' confirm or fall back to modeNormal otherwise.
				t.mode = modeMySQLBodyLabelOrSimple
			}
		}
	case modeMySQLBodyStart:
		t.mode = modeMySQLBodyLabelOrSimple
	}
}

// onMySQLAtVar consumes an unquoted MySQL/MariaDB @var or @@var lexical unit.
// When immediately following a DEFINER user name (e.g. DEFINER=user@localhost or user@do),
// a single-@ token advances the DEFINER account parser directly into defInHost; everywhere
// else it behaves as a non-keyword value token so names like @do or @end never match keywords.
func (t *compoundTracker) onMySQLAtVar(isSysVar bool) {
	if !isSysVar && t.mode == modeMySQLDefiner && t.parenDepth == 0 && t.definerPhase == defAfterUser {
		t.mysqlAfterDot = false
		t.definerPhase = defInHost
		return
	}
	t.onLiteralOrQuotedIdent()
}

func (t *compoundTracker) onWord(upper string) {
	if t.resolveMySQLPending(eventWord, upper) {
		t.mysqlAfterDot = false
		return
	}

	switch t.dialect {
	case DialectSQLite:
		t.onSQLiteWord(upper)
	case DialectPostgres:
		t.onPGWord(upper)
	case DialectMySQL:
		t.onMySQLWord(upper)
	}
	t.mysqlAfterDot = false
}

func (t *compoundTracker) onSQLiteWord(upper string) {
	switch t.mode {
	case modeInit:
		switch upper {
		case "CREATE":
			t.mode = modeSQLiteCreate
		case "EXPLAIN":
			t.mode = modeSQLiteExplain
		default:
			t.mode = modeNormal
		}
	case modeSQLiteExplain:
		switch upper {
		case "QUERY":
			t.mode = modeSQLiteExplainQuery
		case "CREATE":
			t.mode = modeSQLiteCreate
		default:
			t.mode = modeNormal
		}
	case modeSQLiteExplainQuery:
		if upper == "PLAN" {
			t.mode = modeSQLiteAfterExplainPlan
		} else {
			t.mode = modeNormal
		}
	case modeSQLiteAfterExplainPlan:
		if upper == "CREATE" {
			t.mode = modeSQLiteCreate
		} else {
			t.mode = modeNormal
		}
	case modeSQLiteCreate:
		switch upper {
		case "TEMP", "TEMPORARY":
			// stay in modeSQLiteCreate
		case "TRIGGER":
			t.mode = modeSQLiteTriggerHeader
		default:
			t.mode = modeNormal
		}
	case modeSQLiteTriggerHeader:
		if t.parenDepth == 0 && upper == "BEGIN" {
			t.mode = modeSQLiteTriggerBody
			t.sqliteLastWasSemi = false
		}
	case modeSQLiteTriggerBody:
		// SQLite src/complete.c rule: a trigger ends at END immediately after ';'
		// (ignoring intervening whitespace/comments), so CASE ... END; never closes the trigger.
		if t.sqliteLastWasSemi && t.parenDepth == 0 && upper == "END" {
			t.mode = modeSQLiteTriggerEnded
			t.sqliteLastWasSemi = false
			return
		}
		t.sqliteLastWasSemi = false
	case modeSQLiteTriggerEnded:
		// Trailing tokens after "; END" without ';' stay in the same statement until ';'/EOF.
		t.mode = modeNormal
	}
}

func (t *compoundTracker) onPGWord(upper string) {
	switch t.mode {
	case modeInit:
		if upper == "CREATE" {
			t.mode = modePGCreate
		} else {
			t.mode = modeNormal
		}
	case modePGCreate:
		switch upper {
		case "OR":
			t.mode = modePGCreateOr
		case "FUNCTION", "PROCEDURE":
			t.mode = modePGRoutineHeader
		default:
			t.mode = modeNormal
		}
	case modePGCreateOr:
		if upper == "REPLACE" {
			t.mode = modePGCreate
		} else {
			t.mode = modeNormal
		}
	case modePGRoutineHeader:
		if t.parenDepth == 0 && upper == "BEGIN" {
			t.mode = modePGBeginCandidate
		}
	case modePGBeginCandidate:
		if t.parenDepth == 0 && upper == "ATOMIC" {
			t.mode = modePGAtomicBody
			t.pgAtStmtStart = true
		} else {
			t.mode = modePGRoutineHeader
			if t.parenDepth == 0 && upper == "BEGIN" {
				t.mode = modePGBeginCandidate
			}
		}
	case modePGAtomicBody:
		// In PostgreSQL BEGIN ATOMIC ... END, every inner statement ends with ';', so the closing END
		// can ONLY appear at a statement-start position (right after BEGIN ATOMIC or ';') at parenDepth == 0.
		// Aliases (SELECT 1 AS end;), column names, and CASE ... END expressions never close BEGIN ATOMIC.
		if t.pgAtStmtStart && t.parenDepth == 0 && upper == "END" {
			t.mode = modePGAtomicEnded
			t.pgAtStmtStart = false
			return
		}
		t.pgAtStmtStart = false
	case modePGAtomicEnded:
		// PostgreSQL allows routine options (e.g. IMMUTABLE) after BEGIN ATOMIC ... END before ';'.
	}
}

func (t *compoundTracker) onMySQLWord(upper string) {
	switch t.mode {
	case modeInit:
		switch upper {
		case "CREATE":
			t.mode = modeMySQLCreate
		case "ALTER":
			t.mode = modeMySQLAlter
		case "BEGIN":
			t.mode = modeMySQLBeginCheck
		case "IF", "WHILE", "FOR", "REPEAT", "LOOP", "CASE":
			// MariaDB top-level anonymous compound blocks (IF...END IF, WHILE...END WHILE, etc.)
			// execute immediately and are not supported in this version; fail closed without splitting ';'.
			t.mode = modeMySQLUnsupportedAnonymous
			t.mysqlIsAnonBlock = true
		default:
			// Could be a labeled MariaDB anonymous block: lbl: BEGIN NOT ATOMIC / lbl: LOOP / lbl: WHILE ...
			t.mode = modeMySQLLabelCandidate
		}

	case modeMySQLLabelCandidate:
		t.mode = modeNormal

	case modeMySQLLabelAfterColon:
		switch upper {
		case "BEGIN":
			t.mode = modeMySQLBeginCheck
		case "IF", "WHILE", "FOR", "REPEAT", "LOOP", "CASE":
			t.mode = modeMySQLUnsupportedAnonymous
			t.mysqlIsAnonBlock = true
		default:
			t.mode = modeNormal
		}

	case modeMySQLBeginCheck:
		if upper == "NOT" {
			t.mode = modeMySQLBeginNot
		} else {
			t.mode = modeNormal
		}

	case modeMySQLBeginNot:
		if upper == "ATOMIC" {
			t.enterMySQLCompoundBlock(true)
		} else {
			t.mode = modeNormal
		}

	case modeMySQLCreate:
		switch upper {
		case "OR":
			t.mode = modeMySQLCreateOr
		case "DEFINER":
			t.enterMySQLDefiner(modeMySQLCreate)
		case "AGGREGATE":
			t.mode = modeMySQLCreateAggregate
		case "TRIGGER":
			t.mode = modeMySQLTriggerHeader
			t.triggerSeq = 0
		case "EVENT":
			t.enterMySQLEventHeader(true)
		case "PROCEDURE":
			t.mode = modeMySQLProcHeader
			t.procPhase = procWaitParams
		case "FUNCTION":
			t.mode = modeMySQLFuncHeader
			t.funcPhase = funcWaitParams
		default:
			t.mode = modeNormal
		}

	case modeMySQLCreateOr:
		if upper == "REPLACE" {
			t.mode = modeMySQLCreate
		} else {
			t.mode = modeNormal
		}

	case modeMySQLCreateAggregate:
		if upper == "FUNCTION" {
			t.mode = modeMySQLFuncHeader
			t.funcPhase = funcWaitParams
		} else {
			t.mode = modeNormal
		}

	case modeMySQLAlter:
		switch upper {
		case "DEFINER":
			t.enterMySQLDefiner(modeMySQLAlter)
		case "EVENT":
			t.enterMySQLEventHeader(false)
		default:
			t.mode = modeNormal
		}

	case modeMySQLDefiner:
		if t.parenDepth > 0 {
			return
		}
		switch t.definerPhase {
		case defWaitUser:
			t.definerPhase = defAfterUser
		case defWaitHost:
			t.definerPhase = defInHost
		case defAfterUser, defInHost:
			// DEFINER account is complete; return to CREATE or ALTER and inspect the object keyword.
			t.mode = t.definerReturnMode
			t.onMySQLWord(upper)
		default:
			t.mode = modeNormal
		}

	case modeMySQLTriggerHeader:
		if t.parenDepth > 0 {
			return
		}
		switch {
		case t.triggerSeq == 0 && upper == "FOR":
			t.triggerSeq = 1
		case t.triggerSeq == 1 && upper == "EACH":
			t.triggerSeq = 2
		case t.triggerSeq == 2 && upper == "ROW":
			t.mode = modeMySQLTriggerAfterRow
		default:
			if upper == "FOR" {
				t.triggerSeq = 1
			} else {
				t.triggerSeq = 0
			}
		}

	case modeMySQLTriggerAfterRow:
		if upper == "FOLLOWS" || upper == "PRECEDES" {
			t.mode = modeMySQLTriggerSkipOrderName
			return
		}
		t.mode = modeMySQLBodyStart
		t.onMySQLWord(upper)

	case modeMySQLTriggerSkipOrderName:
		t.mode = modeMySQLBodyStart

	case modeMySQLEventHeader:
		if t.parenDepth > 0 {
			return
		}
		switch t.eventPhase {
		case eventWaitNameOrIf:
			if upper == "IF" {
				t.eventPhase = eventAfterIf
			} else {
				t.eventPhase = eventAfterFirstName
			}
		case eventAfterIf:
			if upper == "NOT" {
				t.eventPhase = eventAfterIfNot
			} else {
				t.eventPhase = eventOptions
				t.onMySQLWord(upper)
			}
		case eventAfterIfNot:
			if upper == "EXISTS" {
				t.eventPhase = eventWaitName
			} else {
				t.eventPhase = eventOptions
				t.onMySQLWord(upper)
			}
		case eventWaitName:
			t.eventPhase = eventAfterFirstName
		case eventAfterFirstName:
			// No '.' followed the first identifier -> event_name is complete; inspect option or DO.
			t.eventPhase = eventOptions
			t.onMySQLWord(upper)
		case eventWaitQualName:
			// Consumed qualified event_name after '.' (even if named "do").
			t.eventPhase = eventOptions
		case eventAfterRename:
			if upper == "TO" {
				t.eventPhase = eventWaitName
			} else {
				t.eventPhase = eventOptions
				t.onMySQLWord(upper)
			}
		case eventOptions:
			switch upper {
			case "RENAME":
				t.eventPhase = eventAfterRename
			case "DO":
				t.mode = modeMySQLBodyStart
			}
		}

	case modeMySQLProcHeader:
		// Wait for '(' ... ')' parameter list; handled by onLParen/onRParen.

	case modeMySQLFuncHeader:
		if t.parenDepth > 0 {
			return
		}
		switch t.funcPhase {
		case funcWaitParams, funcInParams:
			// Handled by onLParen/onRParen.
		case funcWaitReturns:
			if upper == "RETURNS" {
				t.funcPhase = funcInReturnType
				t.funcSawTypeName = false
				t.funcSkipNextIdent = false
				t.funcPrevWord = ""
			}
		case funcInReturnType:
			if t.funcSkipNextIdent {
				t.funcSkipNextIdent = false
				t.funcPrevWord = upper
				return
			}
			if !t.funcSawTypeName {
				t.funcSawTypeName = true
				t.funcPrevWord = upper
				return
			}
			switch upper {
			case "UNSIGNED", "SIGNED", "ZEROFILL", "PRECISION", "VARYING", "BYTE", "BINARY",
				"ASCII", "UNICODE", "VARCHAR", "CHAR", "CHARACTER", "VARBINARY", "TEXT", "BLOB",
				"INT", "INTEGER", "DOUBLE", "NATIONAL":
				t.funcPrevWord = upper
				return
			case "CHARSET", "COLLATE":
				t.funcSkipNextIdent = true
				t.funcPrevWord = upper
				return
			case "SET":
				if t.funcPrevWord == "CHARACTER" || t.funcPrevWord == "CHAR" {
					t.funcSkipNextIdent = true
					t.funcPrevWord = upper
					return
				}
			}
			t.mode = modeMySQLCharacteristics
			t.charSkipNext = false
			t.onMySQLWord(upper)
		}

	case modeMySQLCharacteristics:
		if t.parenDepth > 0 {
			return
		}
		if t.charSkipNext {
			t.charSkipNext = false
			return
		}
		switch upper {
		case "COMMENT", "LANGUAGE":
			t.charSkipNext = true
			return
		case "NOT", "DETERMINISTIC", "CONTAINS", "NO", "READS", "MODIFIES",
			"SQL", "DATA", "SECURITY", "DEFINER", "INVOKER":
			return
		}
		t.mode = modeMySQLBodyStart
		t.onMySQLWord(upper)

	case modeMySQLBodyStart:
		switch upper {
		case "BEGIN":
			t.enterMySQLCompoundBlock(false)
		case "IF", "CASE", "LOOP", "WHILE", "REPEAT", "FOR":
			// Valid MySQL/MariaDB compound body without an outer BEGIN (e.g. IF...END IF or FOR...END FOR).
			// Refuse to fall back to splitting on ';'; instruct caller to use DELIMITER.
			t.mode = modeMySQLUnsupportedCompound
		default:
			// Could be a labeled compound body ("lbl: BEGIN" / "lbl: LOOP") or a single simple statement.
			t.mode = modeMySQLBodyLabelOrSimple
		}

	case modeMySQLBodyLabelOrSimple:
		// Not followed by ':' -> simple single-statement routine/trigger body.
		t.mode = modeNormal

	case modeMySQLCompoundBody:
		t.onMySQLCompoundBodyWord(upper)

	case modeMySQLCompoundEnded, modeMySQLUnsupportedCompound, modeMySQLUnsupportedAnonymous, modeNormal:
		// Nothing to update.
	}
}

// onMySQLCompoundBodyWord updates compound block nesting inside MySQL/MariaDB BEGIN ... END.
// Block-closing END is ONLY recognized at a statement-start position (parenDepth == 0 && mysqlAtStmtStart),
// so column names (SELECT t.end FROM t;), aliases (SELECT 1 AS end;), and CASE ... END expressions
// can NEVER prematurely close an outer BEGIN block.
func (t *compoundTracker) onMySQLCompoundBodyWord(upper string) {
	if t.parenDepth > 0 {
		return
	}

	// Qualified identifier right after '.' (e.g. t.end, t.begin, NEW.case) is never a SQL keyword.
	if t.mysqlAfterDot {
		t.mysqlAtStmtStart = false
		t.mysqlStmtWordCount++
		return
	}

	atStart := t.mysqlAtStmtStart
	inHandler := t.mysqlInDeclareHandler

	if atStart && upper == "END" {
		t.mysqlPendingEnd = true
		t.mysqlAtStmtStart = false
		return
	}

	if upper == "BEGIN" && (atStart || inHandler) {
		t.mysqlBeginLook = mysqlBeginSawBegin
		t.mysqlAtStmtStart = false
		return
	}

	if atStart {
		t.mysqlStmtFirstWord = upper
	}
	t.mysqlAtStmtStart = false
	t.mysqlStmtWordCount++

	switch upper {
	case "HANDLER":
		if t.mysqlStmtFirstWord == "DECLARE" {
			t.mysqlInDeclareHandler = true
		}
	case "CASE":
		if !atStart {
			t.mysqlExprCaseDepth++
		}
	case "END":
		if !atStart && t.mysqlExprCaseDepth > 0 {
			t.mysqlExprCaseDepth--
		}
	case "ELSE", "LOOP", "REPEAT":
		if atStart {
			// Statement-level ELSE / LOOP / REPEAT immediately precedes a nested inner statement or BEGIN.
			t.resetMySQLInnerStmt()
		}
	case "THEN":
		if t.mysqlExprCaseDepth == 0 {
			switch t.mysqlStmtFirstWord {
			case "IF", "ELSEIF", "CASE", "WHEN":
				t.resetMySQLInnerStmt()
			}
		}
	case "DO":
		switch t.mysqlStmtFirstWord {
		case "WHILE", "FOR":
			t.resetMySQLInnerStmt()
		}
	}
}

type trackerEventKind uint8

const (
	eventWord trackerEventKind = iota
	eventLiteral
	eventLParen
	eventRParen
	eventColon
	eventSymbol
	eventSemi
	eventEOF
)

// resolveMySQLPending resolves lookahead after a statement-level BEGIN (including MariaDB nested
// BEGIN NOT ATOMIC) or END inside modeMySQLCompoundBody.
// Returns true if the current event was fully consumed by the lookahead resolution.
func (t *compoundTracker) resolveMySQLPending(ev trackerEventKind, word string) bool {
	if t.mode != modeMySQLCompoundBody {
		return false
	}

	switch t.mysqlBeginLook {
	case mysqlBeginSawBegin:
		if ev == eventWord && word == "WORK" {
			t.mysqlBeginLook = mysqlBeginNone
			return true
		}
		if ev == eventSemi {
			t.mysqlBeginLook = mysqlBeginNone
			return false
		}
		if ev == eventWord && word == "NOT" {
			// Could be MariaDB nested "BEGIN NOT ATOMIC ... END"; wait for ATOMIC before dispatching first body statement.
			t.mysqlBeginLook = mysqlBeginSawNot
			return true
		}
		t.mysqlBeginLook = mysqlBeginNone
		t.mysqlBeginDepth++
		t.resetMySQLInnerStmt()
		// Do not consume ev; let caller process the first token inside the nested BEGIN block.

	case mysqlBeginSawNot:
		t.mysqlBeginLook = mysqlBeginNone
		t.mysqlBeginDepth++
		t.resetMySQLInnerStmt()
		if ev == eventWord && word == "ATOMIC" {
			// Consumed full "BEGIN NOT ATOMIC" opener; the next token starts the first inner statement at statement start.
			return true
		}
		// Malformed "BEGIN NOT <other>": keep depth incremented and record "NOT" as first body token.
		t.mysqlStmtFirstWord = "NOT"
		t.mysqlAtStmtStart = false
		t.mysqlStmtWordCount = 1
	}

	if !t.mysqlPendingEnd {
		return false
	}
	t.mysqlPendingEnd = false

	if ev == eventWord {
		switch word {
		case "IF", "LOOP", "WHILE", "REPEAT", "FOR", "CASE":
			// Two-word control closer (END IF / END LOOP / END WHILE / END REPEAT / END FOR / END CASE):
			// closes an inner control structure without popping any BEGIN frame.
			return true
		}
	}

	// Bare END at statement-start (before ';', EOF, or optional trailing block label).
	if t.mysqlBeginDepth > 0 {
		t.mysqlBeginDepth--
	}
	if t.mysqlBeginDepth == 0 {
		t.mode = modeMySQLCompoundEnded
		// Keep optional trailing label identifier (END my_label;) attached to the ended block.
		return ev != eventSemi && ev != eventEOF
	}
	return false
}

// shouldSplitOnSemicolon reports whether a ';' at the current position terminates the statement.
func (t *compoundTracker) shouldSplitOnSemicolon() bool {
	t.mysqlAfterDot = false
	switch t.mode {
	case modeSQLiteTriggerBody:
		if t.parenDepth == 0 {
			t.sqliteLastWasSemi = true
		}
		return false
	case modePGAtomicBody:
		if t.parenDepth == 0 {
			t.pgAtStmtStart = true
		}
		return false
	case modeMySQLUnsupportedCompound, modeMySQLUnsupportedAnonymous:
		return false
	case modeMySQLCompoundBody:
		t.resolveMySQLPending(eventSemi, "")
		if t.mode == modeMySQLCompoundEnded {
			return true
		}
		if t.parenDepth == 0 {
			t.resetMySQLInnerStmt()
		}
		return false
	default:
		return true
	}
}

// finalizeError returns a non-empty user-facing error when a flushed statement is an unclosed
// compound block, an unsupported MySQL compound body without DELIMITER, an ambiguous executable
// comment containing multiple statements, or an unsupported MariaDB anonymous block.
func (t *compoundTracker) finalizeError(atEOF bool) string {
	if t.mode == modeMySQLCompoundBody && atEOF {
		t.resolveMySQLPending(eventEOF, "")
	}

	if t.mysqlIsAnonBlock {
		return errMariaDBAnonBlockUnsupported
	}
	if t.execCommentAmbiguous {
		return errMySQLNeedsDelimiter
	}

	switch t.mode {
	case modeSQLiteTriggerBody:
		return errSQLiteUnclosedTrigger
	case modePGAtomicBody:
		return errPGUnclosedAtomicBody
	case modeMySQLCompoundBody:
		return errMySQLUnclosedCompound
	case modeMySQLUnsupportedCompound:
		return errMySQLNeedsDelimiter
	case modeMySQLUnsupportedAnonymous:
		return errMariaDBAnonBlockUnsupported
	default:
		return ""
	}
}

// isMySQLUnsupportedAnonBlockRaw detects MariaDB top-level anonymous compound blocks
// ("BEGIN NOT ATOMIC", "IF", "WHILE", "FOR", "REPEAT", "LOOP", "CASE", with or without a leading
// "<label>:") regardless of whether a custom DELIMITER was used.
func isMySQLUnsupportedAnonBlockRaw(stmt string) bool {
	stripped := strings.TrimSpace(stripCommentsAndStrings(stmt, DialectMySQL))
	if stripped == "" {
		return false
	}

	i := 0
	n := len(stripped)
	skipWS := func() {
		for i < n && (stripped[i] == ' ' || stripped[i] == '\t' || stripped[i] == '\r' || stripped[i] == '\n' || stripped[i] == '\f' || stripped[i] == '\v') {
			i++
		}
	}
	readWord := func() (string, bool) {
		skipWS()
		if i >= n {
			return "", false
		}
		r, size := utf8.DecodeRuneInString(stripped[i:])
		if !isIdentStart(r) {
			return "", false
		}
		start := i
		i += size
		for i < n {
			nr, nsize := utf8.DecodeRuneInString(stripped[i:])
			if isIdentPart(nr) || nr == '$' {
				i += nsize
				continue
			}
			break
		}
		return strings.ToUpper(stripped[start:i]), true
	}

	var words []string
	// Handle optional leading label: either `quoted_label`: or unquoted_label: (never ':=').
	if stripped[0] == '`' {
		i = 1
		for i < n {
			if stripped[i] == '`' {
				if i+1 < n && stripped[i+1] == '`' {
					i += 2
					continue
				}
				i++
				break
			}
			i++
		}
		skipWS()
		if i >= n || stripped[i] != ':' || (i+1 < n && stripped[i+1] == '=') {
			return false
		}
		i++ // consume ':'
	} else {
		first, ok := readWord()
		if !ok {
			return false
		}
		skipWS()
		if i < n && stripped[i] == ':' && (i+1 >= n || stripped[i+1] != '=') {
			// Leading label followed by ':' (not ':='): skip label and inspect block opener after ':'.
			i++
		} else {
			words = append(words, first)
		}
	}

	for len(words) < 3 {
		w, ok := readWord()
		if !ok {
			break
		}
		words = append(words, w)
	}
	return isMariaDBAnonBlockStart(words)
}

func isMariaDBAnonBlockStart(words []string) bool {
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "BEGIN":
		return len(words) >= 3 && words[1] == "NOT" && words[2] == "ATOMIC"
	case "IF", "WHILE", "FOR", "REPEAT", "LOOP", "CASE":
		return true
	default:
		return false
	}
}
