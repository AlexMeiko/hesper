package sqlsplit

import (
	"strings"
	"testing"
)

func TestSplitMySQLDelimiterAndStrings(t *testing.T) {
	src := `
-- Leading comment
SELECT 'a;b', 'it\'s;ok', "x;y"; # trailing mysql comment
DELIMITER //
CREATE PROCEDURE demo_p()
BEGIN
    SELECT 1;
    SELECT 'inner;semi';
END //
DELIMITER ;
SELECT 2;
`
	stmts := Split(src, DialectMySQL)
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d: %+v", len(stmts), stmts)
	}
	if stmts[0].Kind != KindQuery || stmts[0].FirstWord != "SELECT" {
		t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
	}
	if stmts[1].Kind != KindDDL || !stmts[1].MutatesData {
		t.Fatalf("stmt[1] expected DDL mutating, got: %+v", stmts[1])
	}
	if stmts[2].Text != "SELECT 2" {
		t.Fatalf("stmt[2] unexpected text: %q", stmts[2].Text)
	}
}

func TestSplitPostgresDollarQuotingAndNestedComments(t *testing.T) {
	src := `
/* outer /* nested */ comment */
CREATE FUNCTION f() RETURNS int AS $body$
BEGIN
    RETURN 1;
END;
$body$ LANGUAGE plpgsql;
WITH ins AS (INSERT INTO t VALUES (1) RETURNING id) SELECT * FROM ins;
COMMIT;
`
	stmts := Split(src, DialectPostgres)
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d: %+v", len(stmts), stmts)
	}
	if stmts[0].Kind != KindDDL {
		t.Fatalf("expected DDL, got %+v", stmts[0])
	}
	if stmts[1].Kind != KindWrite || !stmts[1].MutatesData {
		t.Fatalf("expected data-modifying CTE classified as KindWrite, got %+v", stmts[1])
	}
	if stmts[2].Kind != KindCommit {
		t.Fatalf("expected KindCommit, got %+v", stmts[2])
	}
}

func TestStatementAtCursor(t *testing.T) {
	src := "SELECT 1;\nSELECT 2;\nSELECT 3;"
	st, ok := StatementAtCursor(src, 14, DialectMySQL)
	if !ok || st.Text != "SELECT 2" {
		t.Fatalf("expected SELECT 2 at offset 14, got ok=%v st=%+v", ok, st)
	}

	// Verify CJK UTF-8 byte offsets and semicolon adjacency ("SELECT 1;DELETE FROM t")
	adj := "-- 中文注释\nSELECT 1;DELETE FROM t"
	semiIdx := len("-- 中文注释\nSELECT 1;")
	picked, ok := StatementAtCursor(adj, semiIdx, DialectPostgres)
	if !ok || picked.Text != "DELETE FROM t" {
		t.Fatalf("expected DELETE FROM t right after semicolon (byte %d), got ok=%v text=%q", semiIdx, ok, picked.Text)
	}
}

func TestReadOnlyEscapeDetection(t *testing.T) {
	if !IsReadOnlyEscapeSet("SET SESSION transaction_read_only = 0", DialectMySQL) {
		t.Fatal("should detect MySQL read-only escape")
	}
	if !IsReadOnlyEscapeSet("/*! SET */ TRANSACTION READ WRITE", DialectMySQL) {
		t.Fatal("should detect MySQL executable comment read-only escape")
	}
	if !IsReadOnlyEscapeSet("SET default_transaction_read_only = off", DialectPostgres) {
		t.Fatal("should detect PG read-only escape")
	}
	if IsReadOnlyEscapeSet("SET NAMES utf8mb4", DialectMySQL) {
		t.Fatal("SET NAMES should not be flagged as read-only escape")
	}
}

func TestDetectHighRiskAndExplainAnalyze(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		dialect   Dialect
		wantKinds []string
	}{
		{"drop", "DROP TABLE users", DialectPostgres, []string{"drop"}},
		{"truncate", "TRUNCATE TABLE logs", DialectMySQL, []string{"truncate"}},
		{"delete_no_where", "DELETE FROM users", DialectPostgres, []string{"delete_no_where"}},
		{"update_no_where", "UPDATE users SET active = 0", DialectMySQL, []string{"update_no_where"}},
		{"delete_with_where_safe", "DELETE FROM users WHERE id = 1", DialectPostgres, nil},
		{"update_with_where_safe", "UPDATE users SET active = 0 WHERE id = 1", DialectMySQL, nil},
		{"explain_analyze_delete", "EXPLAIN ANALYZE DELETE FROM users", DialectPostgres, []string{"delete_no_where"}},
		{"explain_parens_analyze_update", "EXPLAIN (ANALYZE, BUFFERS) UPDATE users SET active = 0", DialectPostgres, []string{"update_no_where"}},
		{"explain_parens_analyze_false_safe", "EXPLAIN (ANALYZE FALSE) DELETE FROM users", DialectPostgres, nil},
		{"plain_explain_safe", "EXPLAIN DELETE FROM users", DialectPostgres, nil},
		{"mariadb_analyze_delete", "ANALYZE DELETE FROM users", DialectMySQL, []string{"delete_no_where"}},
		{"mariadb_analyze_format_json_update", "ANALYZE FORMAT=JSON UPDATE users SET active = 0", DialectMySQL, []string{"update_no_where"}},
		{"mysql_analyze_table_safe", "ANALYZE TABLE users", DialectMySQL, nil},
		{"mysql_exec_comment_drop", "/*! DROP */ TABLE users", DialectMySQL, []string{"drop"}},
		{"mysql_exec_comment_version_drop", "/*!50000 DROP TABLE users */", DialectMySQL, []string{"drop"}},
		{"mariadb_exec_comment_delete", "/*M!100301 DELETE FROM users */", DialectMySQL, []string{"delete_no_where"}},
		{"pg_cte_delete_no_where", "WITH d AS (DELETE FROM users RETURNING *) SELECT * FROM d", DialectPostgres, []string{"delete_no_where"}},
		{"pg_cte_delete_with_where_safe", "WITH d AS (DELETE FROM users WHERE id = 1 RETURNING *) SELECT * FROM d", DialectPostgres, nil},
		{"pg_cte_update_no_where", "WITH u(id) AS MATERIALIZED (UPDATE users SET a = 1 RETURNING id) SELECT * FROM u", DialectPostgres, []string{"update_no_where"}},
		{"cte_where_does_not_satisfy_outer_delete", "WITH target AS (SELECT id FROM users WHERE id = 1) DELETE FROM users", DialectPostgres, []string{"delete_no_where"}},
		{"malformed_with_fallback_delete", "WITH unrecognized_syntax DELETE FROM users", DialectPostgres, []string{"delete_no_where"}},
		{"explain_analyze_cte_delete", "EXPLAIN ANALYZE WITH d AS (DELETE FROM users RETURNING *) SELECT * FROM d", DialectPostgres, []string{"delete_no_where"}},
		{"quoted_where_ident_not_keyword", `DELETE FROM "where"`, DialectPostgres, []string{"delete_no_where"}},
		{"strings_and_comments_safe", "-- DROP TABLE x\n/* TRUNCATE t */\nSELECT $tag$DELETE FROM t;$tag$", DialectPostgres, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmts := Split(tc.sql, tc.dialect)
			if len(stmts) != 1 {
				t.Fatalf("expected 1 statement, got %d: %+v", len(stmts), stmts)
			}
			got := DetectHighRisk(stmts[0], tc.dialect)
			if len(got) != len(tc.wantKinds) {
				t.Fatalf("got %+v, want kinds %v", got, tc.wantKinds)
			}
			for i, wk := range tc.wantKinds {
				if got[i].Kind != wk {
					t.Fatalf("match[%d].Kind = %q, want %q", i, got[i].Kind, wk)
				}
			}
		})
	}
}

func TestClassifyParenthesizedExplainMariaAnalyzeAndWithForUpdate(t *testing.T) {
	cases := []struct {
		name        string
		sql         string
		dialect     Dialect
		wantKind    StatementKind
		wantFirst   string
		wantMutates bool
		wantRows    bool
	}{
		{"paren_union", "(SELECT 1) UNION (SELECT 2)", DialectPostgres, KindQuery, "SELECT", false, true},
		{"paren_single", "(SELECT * FROM t)", DialectMySQL, KindQuery, "SELECT", false, true},
		{"double_paren_select", "((SELECT 1))", DialectPostgres, KindQuery, "SELECT", false, true},
		{"explain_analyze_delete", "EXPLAIN ANALYZE DELETE FROM users WHERE id = 1", DialectPostgres, KindWrite, "EXPLAIN", true, true},
		{"explain_parens_analyze_update", "EXPLAIN (ANALYZE, BUFFERS) UPDATE users SET a = 1 WHERE id = 1", DialectPostgres, KindWrite, "EXPLAIN", true, true},
		{"plain_explain_delete", "EXPLAIN DELETE FROM users WHERE id = 1", DialectPostgres, KindQuery, "EXPLAIN", false, true},
		{"mariadb_analyze_delete", "ANALYZE DELETE FROM users WHERE id = 1", DialectMySQL, KindWrite, "ANALYZE", true, true},
		{"mariadb_analyze_select", "ANALYZE SELECT * FROM users", DialectMySQL, KindQuery, "ANALYZE", false, true},
		{"mysql_analyze_table", "ANALYZE TABLE users", DialectMySQL, KindDDL, "ANALYZE", true, false},
		{"mysql_exec_comment_drop", "/*! DROP */ TABLE users", DialectMySQL, KindDDL, "DROP", true, false},
		{"with_select_for_update", "WITH c AS (SELECT 1) SELECT * FROM c FOR UPDATE", DialectPostgres, KindQuery, "WITH", false, true},
		{"with_select_for_no_key_update", "WITH c AS (SELECT 1) SELECT * FROM c FOR NO KEY UPDATE", DialectPostgres, KindQuery, "WITH", false, true},
		{"with_cte_delete_then_select", "WITH d AS (DELETE FROM t WHERE id = 1) SELECT 1", DialectPostgres, KindWrite, "WITH", true, true},
		{"update_returning_ident_not_returning", "UPDATE t SET returning_flag = 1 WHERE id = 1", DialectPostgres, KindWrite, "UPDATE", true, false},
		{"cte_returning_outer_update_exec", "WITH d AS (DELETE FROM t WHERE id = 1 RETURNING id) UPDATE audit SET done = 1 WHERE id IN (SELECT id FROM d)", DialectPostgres, KindWrite, "WITH", true, false},
		{"malformed_with_fallback_write", "WITH unrecognized_syntax DELETE FROM users", DialectPostgres, KindWrite, "WITH", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmts := Split(tc.sql, tc.dialect)
			if len(stmts) != 1 {
				t.Fatalf("expected 1 statement, got %d: %+v", len(stmts), stmts)
			}
			st := stmts[0]
			if st.Kind != tc.wantKind || st.FirstWord != tc.wantFirst || st.MutatesData != tc.wantMutates || st.ReturnsRows != tc.wantRows {
				t.Fatalf("got (kind=%s first=%q mutates=%v rows=%v), want (kind=%s first=%q mutates=%v rows=%v)",
					st.Kind, st.FirstWord, st.MutatesData, st.ReturnsRows,
					tc.wantKind, tc.wantFirst, tc.wantMutates, tc.wantRows)
			}
		})
	}
}

func TestSplitSQLiteTriggersAndBracketIdentifiers(t *testing.T) {
	t.Run("trigger followed by SELECT 1 with cursor assertions", func(t *testing.T) {
		src := `CREATE TRIGGER trg_audit
AFTER INSERT ON customers
BEGIN
  INSERT INTO audit_log(customer_id) VALUES (new.id);
  UPDATE stats SET n = n + 1;
END;
SELECT 1;`
		stmts := Split(src, DialectSQLite)
		if len(stmts) != 2 {
			t.Fatalf("got %d statements, want 2: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL || !stmts[0].MutatesData {
			t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
		}
		if stmts[1].Text != "SELECT 1" || stmts[1].Kind != KindQuery || stmts[1].SplitError != "" {
			t.Fatalf("stmt[1] unexpected: %+v", stmts[1])
		}

		// Cursor on inner INSERT, inner UPDATE, and END MUST all select the full trigger definition.
		for _, needle := range []string{"INSERT INTO audit_log", "UPDATE stats SET", "END;"} {
			pos := indexOfMust(t, src, needle)
			picked, ok := SelectStatementAtCursor(stmts, pos)
			if !ok || picked.Text != stmts[0].Text {
				t.Fatalf("cursor on %q selected %q, want full trigger %q", needle, picked.Text, stmts[0].Text)
			}
		}

		// Cursor after END; on SELECT 1 MUST select SELECT 1 alone.
		selPos := indexOfMust(t, src, "SELECT 1")
		picked, ok := SelectStatementAtCursor(stmts, selPos)
		if !ok || picked.Text != "SELECT 1" {
			t.Fatalf("cursor on SELECT 1 selected %q, want SELECT 1", picked.Text)
		}
	})

	t.Run("CASE END inside trigger, comments around END, bracket idents, and EOF without trailing semi", func(t *testing.T) {
		src := `CREATE TEMP TRIGGER [trg;1]
AFTER UPDATE ON [my;tbl]
WHEN (CASE WHEN new.[end] > 0 THEN 1 ELSE 0 END) = 1
BEGIN
  UPDATE [my;tbl] SET [end] = CASE WHEN old.[end] = 1 THEN 2 ELSE 3 END;
  -- comment between ';' and END
  /* block comment */ END`
		stmts := Split(src, DialectSQLite)
		if len(stmts) != 1 {
			t.Fatalf("got %d statements, want 1: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL {
			t.Fatalf("expected valid complete trigger at EOF without trailing ';', got %+v", stmts[0])
		}

		// Bracket-quoted [where] must NOT satisfy DELETE WHERE check.
		delStmts := Split("DELETE FROM [where]; SELECT [a;b] FROM [t];", DialectSQLite)
		if len(delStmts) != 2 {
			t.Fatalf("expected 2 statements with bracket idents, got %d: %+v", len(delStmts), delStmts)
		}
		risks := DetectHighRisk(delStmts[0], DialectSQLite)
		if len(risks) != 1 || risks[0].Kind != "delete_no_where" {
			t.Fatalf("expected delete_no_where for DELETE FROM [where], got %+v", risks)
		}
	})

	t.Run("missing closing END fails closed and never isolates inner UPDATE", func(t *testing.T) {
		src := `CREATE TRIGGER trg_audit
AFTER INSERT ON customers
BEGIN
  INSERT INTO audit_log(customer_id) VALUES (new.id);
  UPDATE stats SET n = n + 1;
`
		stmts := Split(src, DialectSQLite)
		if len(stmts) != 1 {
			t.Fatalf("unclosed trigger MUST stay 1 statement, got %d: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError == "" {
			t.Fatalf("expected non-empty SplitError for unclosed SQLite trigger, got %+v", stmts[0])
		}
		updPos := indexOfMust(t, src, "UPDATE stats")
		picked, ok := SelectStatementAtCursor(stmts, updPos)
		if !ok || picked.SplitError == "" {
			t.Fatalf("cursor on inner UPDATE of unclosed trigger MUST carry SplitError, got ok=%v st=%+v", ok, picked)
		}
	})

	t.Run("plain SQLite transaction BEGIN ... END still splits cleanly", func(t *testing.T) {
		src := "BEGIN IMMEDIATE; INSERT INTO t VALUES (1); END TRANSACTION; SELECT 1;"
		stmts := Split(src, DialectSQLite)
		if len(stmts) != 4 {
			t.Fatalf("expected 4 statements, got %d: %+v", len(stmts), stmts)
		}
		if stmts[0].Kind != KindBegin || stmts[1].Kind != KindWrite || stmts[2].Kind != KindCommit || stmts[3].Kind != KindQuery {
			t.Fatalf("unexpected kinds: %+v", stmts)
		}
	})
}

func TestSplitMySQLCompoundStatementsAndBeginNotAtomic(t *testing.T) {
	t.Run("procedure without DELIMITER followed by SELECT 1 with cursor assertions", func(t *testing.T) {
		src := `CREATE DEFINER='root'@'localhost' PROCEDURE p_audit(IN x INT)
DETERMINISTIC
outer_lbl: BEGIN
  DECLARE v INT DEFAULT 0;
  SET v = CASE WHEN x > 0 THEN 1 ELSE 0 END;
  IF v = 1 THEN
    INSERT INTO audit_log VALUES (x);
  END IF;
  FOR i IN 1..2 DO
    UPDATE stats SET n = n + i WHERE id = 1;
  END FOR;
  inner_lbl: BEGIN
    SELECT IF(v = 1, 'a', 'b');
  END inner_lbl;
END outer_lbl;
SELECT 1;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 2 {
			t.Fatalf("got %d statements, want 2: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL {
			t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
		}
		if stmts[1].Text != "SELECT 1" || stmts[1].SplitError != "" {
			t.Fatalf("stmt[1] unexpected: %+v", stmts[1])
		}

		for _, needle := range []string{"INSERT INTO audit_log", "UPDATE stats SET", "END FOR;", "END outer_lbl;"} {
			pos := indexOfMust(t, src, needle)
			picked, ok := SelectStatementAtCursor(stmts, pos)
			if !ok || picked.Text != stmts[0].Text {
				t.Fatalf("cursor on %q selected %q, want full procedure", needle, picked.Text)
			}
		}
		selPos := indexOfMust(t, src, "SELECT 1")
		if picked, ok := SelectStatementAtCursor(stmts, selPos); !ok || picked.Text != "SELECT 1" {
			t.Fatalf("cursor on SELECT 1 selected %q", picked.Text)
		}
	})

	t.Run("trigger, function, and event without DELIMITER", func(t *testing.T) {
		src := `CREATE TRIGGER trg_u AFTER INSERT ON t FOR EACH ROW FOLLOWS trg_prev
BEGIN
  INSERT INTO log VALUES (NEW.id);
  UPDATE stats SET n = n + 1;
END;
CREATE FUNCTION fn_c(x INT) RETURNS VARCHAR(32) CHARSET utf8mb4 DETERMINISTIC
BEGIN
  CASE x WHEN 1 THEN RETURN 'one'; ELSE RETURN 'other'; END CASE;
END;
CREATE EVENT ev_clean ON SCHEDULE EVERY 1 DAY COMMENT 'do cleanup' DO
BEGIN
  DELETE FROM tmp WHERE expired = 1;
END;
CREATE TRIGGER trg_simple AFTER INSERT ON t FOR EACH ROW INSERT INTO log VALUES (NEW.id);
SELECT 1;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 5 {
			t.Fatalf("got %d statements, want 5: %+v", len(stmts), stmts)
		}
		for i := 0; i < 4; i++ {
			if stmts[i].SplitError != "" || stmts[i].Kind != KindDDL {
				t.Fatalf("stmt[%d] unexpected: %+v", i, stmts[i])
			}
		}
		if stmts[4].Text != "SELECT 1" || stmts[4].SplitError != "" {
			t.Fatalf("stmt[4] unexpected: %+v", stmts[4])
		}
	})

	t.Run("compound body without outer BEGIN advises DELIMITER and never splits inner statements", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  string
		}{
			{
				name: "direct_if_body",
				src: `CREATE PROCEDURE p_if(IN x INT)
IF x > 0 THEN
  INSERT INTO audit_log VALUES (x);
  UPDATE stats SET n = n + 1;
END IF;`,
			},
			{
				name: "direct_labeled_for_body",
				src: `CREATE PROCEDURE p_for()
lp: FOR i IN 1..3 DO
  INSERT INTO audit_log VALUES (i);
  UPDATE stats SET n = n + 1;
END FOR lp;`,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stmts := Split(tc.src, DialectMySQL)
				if len(stmts) != 1 || stmts[0].SplitError == "" {
					t.Fatalf("expected 1 statement with DELIMITER guidance SplitError, got %+v", stmts)
				}
				updPos := indexOfMust(t, tc.src, "UPDATE stats")
				picked, ok := SelectStatementAtCursor(stmts, updPos)
				if !ok || picked.SplitError == "" {
					t.Fatalf("cursor on inner UPDATE MUST carry SplitError, got %+v", picked)
				}
			})
		}

		// With explicit DELIMITER, direct IF body works cleanly.
		withDelim := "DELIMITER //\nCREATE PROCEDURE p_if(IN x INT) IF x > 0 THEN INSERT INTO a VALUES (1); END IF //\nDELIMITER ;\nSELECT 1;"
		stmts := Split(withDelim, DialectMySQL)
		if len(stmts) != 2 || stmts[0].SplitError != "" || stmts[1].Text != "SELECT 1" {
			t.Fatalf("explicit DELIMITER must take priority, got %+v", stmts)
		}
	})

	t.Run("missing closing END fails closed", func(t *testing.T) {
		src := `CREATE PROCEDURE p_unclosed()
BEGIN
  INSERT INTO a VALUES (1);
  UPDATE stats SET n = n + 1;
`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 1 || stmts[0].SplitError == "" {
			t.Fatalf("expected 1 statement with SplitError for unclosed MySQL procedure, got %+v", stmts)
		}
	})

	t.Run("MariaDB BEGIN NOT ATOMIC rejected explicitly and never classified as KindBegin", func(t *testing.T) {
		src := `BEGIN NOT ATOMIC
  INSERT INTO a VALUES (1);
  UPDATE stats SET n = n + 1;
END;
SELECT 1;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 2 {
			t.Fatalf("expected 2 statements (unsupported block + SELECT 1), got %d: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError == "" || stmts[0].Kind == KindBegin || !stmts[0].MutatesData {
			t.Fatalf("BEGIN NOT ATOMIC MUST have SplitError, Kind!=KindBegin, MutatesData=true, got %+v", stmts[0])
		}
		if stmts[1].Text != "SELECT 1" || stmts[1].SplitError != "" {
			t.Fatalf("trailing SELECT 1 unexpected: %+v", stmts[1])
		}

		// Even with DELIMITER //, BEGIN NOT ATOMIC is still explicitly rejected.
		delimSrc := "DELIMITER //\nmy_lbl: BEGIN NOT ATOMIC INSERT INTO a VALUES (1); END //\nDELIMITER ;"
		delimStmts := Split(delimSrc, DialectMySQL)
		if len(delimStmts) != 1 || delimStmts[0].SplitError == "" || delimStmts[0].Kind == KindBegin {
			t.Fatalf("labeled BEGIN NOT ATOMIC under DELIMITER MUST still carry SplitError, got %+v", delimStmts)
		}

		// Plain transaction BEGIN; ...; COMMIT; still works as 3 normal statements.
		txStmts := Split("BEGIN; INSERT INTO a VALUES (1); COMMIT;", DialectMySQL)
		if len(txStmts) != 3 || txStmts[0].Kind != KindBegin || txStmts[0].SplitError != "" {
			t.Fatalf("plain MySQL transaction BEGIN broken: %+v", txStmts)
		}
	})
}

func TestSplitPostgresBeginAtomic(t *testing.T) {
	src := `CREATE OR REPLACE FUNCTION add_audit(v INT) RETURNS INT
LANGUAGE SQL
BEGIN ATOMIC
  INSERT INTO audit_log VALUES (v);
  SELECT CASE WHEN v > 0 THEN v ELSE 0 END;
END;
SELECT 1;`
	stmts := Split(src, DialectPostgres)
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2: %+v", len(stmts), stmts)
	}
	if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL {
		t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
	}
	if stmts[1].Text != "SELECT 1" || stmts[1].SplitError != "" {
		t.Fatalf("stmt[1] unexpected: %+v", stmts[1])
	}
	insPos := indexOfMust(t, src, "INSERT INTO audit_log")
	if picked, ok := SelectStatementAtCursor(stmts, insPos); !ok || picked.Text != stmts[0].Text {
		t.Fatalf("cursor inside BEGIN ATOMIC selected %q, want full function", picked.Text)
	}
	selPos := indexOfMust(t, src, "SELECT 1")
	if picked, ok := SelectStatementAtCursor(stmts, selPos); !ok || picked.Text != "SELECT 1" {
		t.Fatalf("cursor on SELECT 1 selected %q", picked.Text)
	}

	// Missing closing END fails closed.
	unclosed := `CREATE PROCEDURE p_atomic(v INT) LANGUAGE SQL BEGIN ATOMIC
  INSERT INTO a VALUES (v);
  UPDATE stats SET n = n + 1;`
	unclosedStmts := Split(unclosed, DialectPostgres)
	if len(unclosedStmts) != 1 || unclosedStmts[0].SplitError == "" {
		t.Fatalf("expected 1 statement with SplitError for unclosed PG BEGIN ATOMIC, got %+v", unclosedStmts)
	}

	// Ordinary BEGIN; SELECT 1; COMMIT; stays 3 statements.
	tx := Split("BEGIN; SELECT 1; COMMIT;", DialectPostgres)
	if len(tx) != 3 || tx[0].Kind != KindBegin || tx[1].Kind != KindQuery || tx[2].Kind != KindCommit {
		t.Fatalf("plain PG tx split broken: %+v", tx)
	}
}

func TestClassifyEndAndAndChainPerDialect(t *testing.T) {
	cases := []struct {
		name             string
		sql              string
		dialect          Dialect
		wantKind         StatementKind
		wantStartsNextTx bool
	}{
		// MySQL / MariaDB
		{"mysql_end_is_other", "END", DialectMySQL, KindOther, false},
		{"mysql_end_tx_is_other", "END TRANSACTION", DialectMySQL, KindOther, false},
		{"mysql_commit", "COMMIT", DialectMySQL, KindCommit, false},
		{"mysql_commit_and_chain", "COMMIT AND CHAIN", DialectMySQL, KindCommit, true},
		{"mysql_commit_work_and_chain", "COMMIT WORK AND CHAIN", DialectMySQL, KindCommit, true},
		{"mysql_commit_and_no_chain", "COMMIT AND NO CHAIN", DialectMySQL, KindCommit, false},
		{"mysql_rollback_and_chain", "ROLLBACK AND CHAIN", DialectMySQL, KindRollback, true},
		{"mysql_rollback_and_no_chain", "ROLLBACK AND NO CHAIN", DialectMySQL, KindRollback, false},

		// SQLite
		{"sqlite_end", "END", DialectSQLite, KindCommit, false},
		{"sqlite_end_tx", "END TRANSACTION", DialectSQLite, KindCommit, false},
		{"sqlite_end_tx_named", "END TRANSACTION my_tx", DialectSQLite, KindCommit, false},
		{"sqlite_end_tx_quoted_name", "END TRANSACTION [my_tx]", DialectSQLite, KindCommit, false},
		{"sqlite_end_work_not_commit", "END WORK", DialectSQLite, KindOther, false},
		{"sqlite_end_if_not_commit", "END IF", DialectSQLite, KindOther, false},

		// PostgreSQL
		{"pg_end", "END", DialectPostgres, KindCommit, false},
		{"pg_end_work", "END WORK", DialectPostgres, KindCommit, false},
		{"pg_end_tx", "END TRANSACTION", DialectPostgres, KindCommit, false},
		{"pg_end_and_chain", "END AND CHAIN", DialectPostgres, KindCommit, true},
		{"pg_end_work_and_chain", "END WORK AND CHAIN", DialectPostgres, KindCommit, true},
		{"pg_end_tx_and_chain", "END TRANSACTION AND CHAIN", DialectPostgres, KindCommit, true},
		{"pg_end_and_no_chain", "END AND NO CHAIN", DialectPostgres, KindCommit, false},
		{"pg_end_tx_and_no_chain", "END TRANSACTION AND NO CHAIN", DialectPostgres, KindCommit, false},
		{"pg_end_if_not_commit", "END IF", DialectPostgres, KindOther, false},
		{"pg_end_invalid_tail", "END FOOBAR", DialectPostgres, KindOther, false},
		{"pg_commit_and_chain", "COMMIT AND CHAIN", DialectPostgres, KindCommit, true},
		{"pg_commit_and_no_chain", "COMMIT AND NO CHAIN", DialectPostgres, KindCommit, false},
		{"pg_rollback_and_chain", "ROLLBACK AND CHAIN", DialectPostgres, KindRollback, true},
		{"pg_rollback_and_no_chain", "ROLLBACK AND NO CHAIN", DialectPostgres, KindRollback, false},
		{"pg_rollback_to_savepoint", "ROLLBACK TO SAVEPOINT sp1", DialectPostgres, KindRollbackToSavepoint, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmts := Split(tc.sql, tc.dialect)
			if len(stmts) != 1 {
				t.Fatalf("expected 1 statement for %q, got %d: %+v", tc.sql, len(stmts), stmts)
			}
			st := stmts[0]
			if st.Kind != tc.wantKind || st.StartsNextTx != tc.wantStartsNextTx {
				t.Fatalf("sql=%q (%s): got (kind=%s startsNextTx=%v), want (kind=%s startsNextTx=%v)",
					tc.sql, tc.dialect, st.Kind, st.StartsNextTx, tc.wantKind, tc.wantStartsNextTx)
			}
		})
	}
}

func TestSplitCompoundEdgeCasesAndEntryPoints(t *testing.T) {
	t.Run("PostgreSQL BEGIN ATOMIC ignores alias and column named end", func(t *testing.T) {
		src := `CREATE OR REPLACE FUNCTION f_alias_end() RETURNS INT
LANGUAGE SQL
BEGIN ATOMIC
  SELECT 1 AS end;
  UPDATE stats SET n = n + 1;
  SELECT t.end FROM t;
  SELECT CASE WHEN true THEN 1 ELSE 2 END;
END;
SELECT 2;`
		stmts := Split(src, DialectPostgres)
		if len(stmts) != 2 {
			t.Fatalf("got %d statements, want 2: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL {
			t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
		}
		if stmts[1].Text != "SELECT 2" || stmts[1].SplitError != "" {
			t.Fatalf("stmt[1] unexpected: %+v", stmts[1])
		}
		updPos := indexOfMust(t, src, "UPDATE stats")
		if picked, ok := SelectStatementAtCursor(stmts, updPos); !ok || picked.Text != stmts[0].Text {
			t.Fatalf("cursor on inner UPDATE after 'SELECT 1 AS end;' selected %q, want full function", picked.Text)
		}

		// Missing closing END after 'SELECT 1 AS end;' MUST still fail closed.
		unclosed := `CREATE FUNCTION f_alias_end_unclosed() RETURNS INT LANGUAGE SQL
BEGIN ATOMIC
  SELECT 1 AS end;
  UPDATE stats SET n = n + 1;`
		unclosedStmts := Split(unclosed, DialectPostgres)
		if len(unclosedStmts) != 1 || unclosedStmts[0].SplitError == "" {
			t.Fatalf("expected 1 statement with SplitError for unclosed PG BEGIN ATOMIC after alias end, got %+v", unclosedStmts)
		}
	})

	t.Run("MySQL BEGIN block ignores qualified t.end/t.begin, aliases, and handles nested control flow", func(t *testing.T) {
		src := `CREATE PROCEDURE p_end_ident()
BEGIN
  DECLARE CONTINUE HANDLER FOR SQLEXCEPTION BEGIN
    UPDATE stats SET n = n + 2;
  END;
  SELECT t.end, t.begin, 1 AS end, 2 AS begin FROM t;
  SET @v = CASE WHEN 1 = 1 THEN t.begin ELSE t.end END;
  IF CASE WHEN 1 = 1 THEN t.end ELSE 0 END = 1 THEN
    WHILE @i < 3 DO
      inner_lbl: BEGIN
        UPDATE stats SET n = n + 1;
      END inner_lbl;
    END WHILE;
  END IF;
  UPDATE stats SET n = n + 3;
END;
SELECT 2;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 2 {
			t.Fatalf("got %d statements, want 2: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL {
			t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
		}
		if stmts[1].Text != "SELECT 2" || stmts[1].SplitError != "" {
			t.Fatalf("stmt[1] unexpected: %+v", stmts[1])
		}
		updPos := indexOfMust(t, src, "UPDATE stats SET n = n + 3")
		if picked, ok := SelectStatementAtCursor(stmts, updPos); !ok || picked.Text != stmts[0].Text {
			t.Fatalf("cursor on inner UPDATE after t.end selected %q, want full procedure", picked.Text)
		}

		// Missing closing outer END after SELECT t.end MUST still fail closed.
		unclosed := `CREATE PROCEDURE p_end_unclosed()
BEGIN
  SELECT t.end FROM t;
  UPDATE stats SET n = n + 1;`
		unclosedStmts := Split(unclosed, DialectMySQL)
		if len(unclosedStmts) != 1 || unclosedStmts[0].SplitError == "" {
			t.Fatalf("expected 1 statement with SplitError for unclosed MySQL proc after t.end, got %+v", unclosedStmts)
		}
	})

	t.Run("SQLite EXPLAIN and EXPLAIN QUERY PLAN CREATE TRIGGER stay atomic", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  string
		}{
			{
				name: "explain_create_trigger",
				src: `EXPLAIN CREATE TRIGGER trg_exp
AFTER INSERT ON customers
BEGIN
  INSERT INTO audit_log(customer_id) VALUES (new.id);
  UPDATE stats SET n = n + 1;
END;
SELECT 1;`,
			},
			{
				name: "explain_query_plan_create_temp_trigger",
				src: `EXPLAIN QUERY PLAN CREATE TEMP TRIGGER trg_eqp
AFTER INSERT ON customers
BEGIN
  INSERT INTO audit_log(customer_id) VALUES (new.id);
  UPDATE stats SET n = n + 1;
END;
SELECT 1;`,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stmts := Split(tc.src, DialectSQLite)
				if len(stmts) != 2 {
					t.Fatalf("got %d statements, want 2: %+v", len(stmts), stmts)
				}
				if stmts[0].SplitError != "" || stmts[0].Kind != KindQuery || !stmts[0].ReturnsRows {
					t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
				}
				if stmts[1].Text != "SELECT 1" || stmts[1].SplitError != "" {
					t.Fatalf("stmt[1] unexpected: %+v", stmts[1])
				}
				updPos := indexOfMust(t, tc.src, "UPDATE stats")
				if picked, ok := SelectStatementAtCursor(stmts, updPos); !ok || picked.Text != stmts[0].Text {
					t.Fatalf("cursor on inner UPDATE inside EXPLAIN CREATE TRIGGER selected %q", picked.Text)
				}
			})
		}

		unclosed := `EXPLAIN CREATE TRIGGER trg_exp_unclosed
AFTER INSERT ON customers
BEGIN
  UPDATE stats SET n = n + 1;`
		unclosedStmts := Split(unclosed, DialectSQLite)
		if len(unclosedStmts) != 1 || unclosedStmts[0].SplitError == "" {
			t.Fatalf("expected 1 statement with SplitError for unclosed EXPLAIN CREATE TRIGGER, got %+v", unclosedStmts)
		}
	})

	t.Run("MySQL executable comments in CREATE DEFINER and ambiguous multi-statement executable comments", func(t *testing.T) {
		src := `CREATE /*!50020 DEFINER='root'@'localhost'*/ PROCEDURE p_exec_comment()
BEGIN
  INSERT INTO audit_log VALUES (1);
  UPDATE stats SET n = n + 1;
END;
CREATE /*M!100301 DEFINER=CURRENT_USER()*/ TRIGGER trg_exec_comment
AFTER INSERT ON customers FOR EACH ROW
BEGIN
  INSERT INTO audit_log VALUES (NEW.id);
  UPDATE stats SET n = n + 1;
END;
SELECT 1;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 3 {
			t.Fatalf("got %d statements, want 3: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL {
			t.Fatalf("stmt[0] unexpected: %+v", stmts[0])
		}
		if stmts[1].SplitError != "" || stmts[1].Kind != KindDDL {
			t.Fatalf("stmt[1] unexpected: %+v", stmts[1])
		}
		if stmts[2].Text != "SELECT 1" || stmts[2].SplitError != "" {
			t.Fatalf("stmt[2] unexpected: %+v", stmts[2])
		}
		updPos := indexOfMust(t, src, "UPDATE stats SET n = n + 1;")
		if picked, ok := SelectStatementAtCursor(stmts, updPos); !ok || picked.Text != stmts[0].Text {
			t.Fatalf("cursor on inner UPDATE inside CREATE /*!50020 DEFINER=...*/ PROCEDURE selected %q", picked.Text)
		}

		// Ambiguous multi-statement executable comments MUST fail closed with DELIMITER guidance.
		for _, bad := range []string{
			`/*!50000 SELECT 1; SELECT 2 */`,
			`/*!50000 SELECT 1; */ SELECT 2;`,
		} {
			badStmts := Split(bad, DialectMySQL)
			if len(badStmts) != 1 || badStmts[0].SplitError == "" {
				t.Fatalf("expected SplitError for ambiguous executable comment %q, got %+v", bad, badStmts)
			}
		}

		// Single-statement executable comment with trailing ';' followed by outer ';' stays valid.
		okStmts := Split(`/*!50000 SELECT 1; */; SELECT 2;`, DialectMySQL)
		if len(okStmts) != 2 || okStmts[0].SplitError != "" || okStmts[1].Text != "SELECT 2" || okStmts[1].SplitError != "" {
			t.Fatalf("expected clean 2 statements for single-statement executable comment, got %+v", okStmts)
		}
	})

	t.Run("MariaDB standalone anonymous blocks fail closed without splitting", func(t *testing.T) {
		cases := []struct {
			name string
			src  string
		}{
			{
				name: "standalone_if",
				src: `IF 1 = 1 THEN
  INSERT INTO a VALUES (1);
  UPDATE stats SET n = n + 1;
END IF;`,
			},
			{
				name: "standalone_if_no_space_paren",
				src: `IF(1 = 1) THEN
  UPDATE stats SET n = n + 1;
END IF;`,
			},
			{
				name: "standalone_while",
				src: `WHILE @i < 3 DO
  SET @i = @i + 1;
  UPDATE stats SET n = n + 1;
END WHILE;`,
			},
			{
				name: "standalone_for",
				src: `FOR i IN 1..3 DO
  UPDATE stats SET n = n + 1;
END FOR;`,
			},
			{
				name: "standalone_repeat",
				src: `REPEAT
  UPDATE stats SET n = n + 1;
UNTIL 1 = 1
END REPEAT;`,
			},
			{
				name: "labeled_loop",
				src: `lp: LOOP
  UPDATE stats SET n = n + 1;
  LEAVE lp;
END LOOP lp;`,
			},
			{
				name: "standalone_case",
				src: `CASE @x
  WHEN 1 THEN UPDATE stats SET n = n + 1;
  ELSE UPDATE stats SET n = n + 2;
END CASE;`,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stmts := Split(tc.src, DialectMySQL)
				if len(stmts) != 1 {
					t.Fatalf("expected 1 unsplit statement for %s, got %d: %+v", tc.name, len(stmts), stmts)
				}
				st := stmts[0]
				if st.SplitError == "" || st.Kind != KindOther || !st.MutatesData {
					t.Fatalf("expected SplitError + KindOther + MutatesData=true for %s, got %+v", tc.name, st)
				}
				updPos := indexOfMust(t, tc.src, "UPDATE stats")
				picked, ok := SelectStatementAtCursor(stmts, updPos)
				if !ok || picked.SplitError == "" {
					t.Fatalf("cursor on inner UPDATE inside %s MUST carry SplitError, got %+v", tc.name, picked)
				}

				// Direct Classify must agree (KindOther + mutates=true).
				ck, _, cm := Classify(tc.src, DialectMySQL)
				if ck != KindOther || !cm {
					t.Fatalf("Classify(%s) = (%s, mutates=%v), want (KindOther, true)", tc.name, ck, cm)
				}

				// Custom DELIMITER must still reject MariaDB anonymous blocks cleanly.
				delimSrc := "DELIMITER //\n" + strings.TrimSuffix(tc.src, ";") + " //\nDELIMITER ;"
				delimStmts := Split(delimSrc, DialectMySQL)
				if len(delimStmts) != 1 || delimStmts[0].SplitError == "" || delimStmts[0].Kind != KindOther || !delimStmts[0].MutatesData {
					t.Fatalf("expected SplitError under custom DELIMITER for %s, got %+v", tc.name, delimStmts)
				}
			})
		}
	})

	t.Run("MySQL ALTER EVENT with DO compound block stays atomic", func(t *testing.T) {
		src := `ALTER EVENT ev_demo DO
BEGIN
  SELECT 1;
  UPDATE stats SET n = n + 1;
END;
ALTER /*!50020 DEFINER='root'@'localhost'*/ EVENT ev_def
ON SCHEDULE EVERY 1 HOUR DISABLE
DO BEGIN
  INSERT INTO audit_log VALUES (1);
  UPDATE stats SET n = n + 2;
END;
ALTER DEFINER=user@localhost EVENT app.do DO
BEGIN
  SELECT 1;
  UPDATE stats SET n = n + 3;
END;
ALTER DEFINER=event@127.0.0.1 EVENT do RENAME TO app.do DO
BEGIN
  SELECT 1;
  UPDATE stats SET n = n + 4;
END;
CREATE DEFINER=user@localhost EVENT IF NOT EXISTS app.do
ON SCHEDULE EVERY 1 DAY DISABLE
DO BEGIN
  SELECT 1;
  UPDATE stats SET n = n + 5;
END;
ALTER EVENT ev_toggle ENABLE;
SELECT 3;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 7 {
			t.Fatalf("got %d statements, want 7: %+v", len(stmts), stmts)
		}
		for i := 0; i < 6; i++ {
			if stmts[i].SplitError != "" || stmts[i].Kind != KindDDL {
				t.Fatalf("stmt[%d] unexpected: %+v", i, stmts[i])
			}
		}
		if stmts[6].Text != "SELECT 3" || stmts[6].SplitError != "" {
			t.Fatalf("stmt[6] unexpected: %+v", stmts[6])
		}
		for idx, needle := range []string{
			"UPDATE stats SET n = n + 1;",
			"UPDATE stats SET n = n + 2;",
			"UPDATE stats SET n = n + 3;",
			"UPDATE stats SET n = n + 4;",
			"UPDATE stats SET n = n + 5;",
		} {
			pos := indexOfMust(t, src, needle)
			if picked, ok := SelectStatementAtCursor(stmts, pos); !ok || picked.Text != stmts[idx].Text {
				t.Fatalf("cursor on %q selected %q, want stmt[%d]", needle, picked.Text, idx)
			}
		}

		// Non-BEGIN compound body inside ALTER EVENT advises DELIMITER instead of splitting.
		ifBody := `ALTER EVENT ev_if DO IF 1 = 1 THEN UPDATE stats SET n = n + 1; END IF;`
		ifStmts := Split(ifBody, DialectMySQL)
		if len(ifStmts) != 1 || ifStmts[0].SplitError == "" {
			t.Fatalf("expected SplitError advising DELIMITER for ALTER EVENT ... DO IF, got %+v", ifStmts)
		}

		// Missing closing END inside ALTER EVENT fails closed.
		unclosed := `ALTER DEFINER=user@localhost EVENT app.do DO BEGIN UPDATE stats SET n = n + 1;`
		unclosedStmts := Split(unclosed, DialectMySQL)
		if len(unclosedStmts) != 1 || unclosedStmts[0].SplitError == "" {
			t.Fatalf("expected SplitError for unclosed ALTER EVENT, got %+v", unclosedStmts)
		}
	})

	t.Run("MySQL outermost backtick-quoted block labels on routines stay atomic", func(t *testing.T) {
		src := "CREATE PROCEDURE p_bt_lbl()\n" +
			"`blk`: BEGIN\n" +
			"  SELECT 1;\n" +
			"  UPDATE stats SET n = n + 1;\n" +
			"END `blk`;\n" +
			"CREATE PROCEDURE p_bt_comment() COMMENT 'desc'\n" +
			"`blk2`: BEGIN\n" +
			"  INSERT INTO a VALUES (1);\n" +
			"  UPDATE stats SET n = n + 2;\n" +
			"END `blk2`;\n" +
			"CREATE FUNCTION f_bt_lbl() RETURNS INT DETERMINISTIC\n" +
			"`fblk`: BEGIN\n" +
			"  UPDATE stats SET n = n + 3;\n" +
			"  RETURN 1;\n" +
			"END `fblk`;\n" +
			"SELECT 2;"
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 4 {
			t.Fatalf("got %d statements, want 4: %+v", len(stmts), stmts)
		}
		for i := 0; i < 3; i++ {
			if stmts[i].SplitError != "" || stmts[i].Kind != KindDDL {
				t.Fatalf("stmt[%d] unexpected: %+v", i, stmts[i])
			}
		}
		if stmts[3].Text != "SELECT 2" || stmts[3].SplitError != "" {
			t.Fatalf("stmt[3] unexpected: %+v", stmts[3])
		}
		for idx, needle := range []string{
			"UPDATE stats SET n = n + 1;",
			"UPDATE stats SET n = n + 2;",
			"UPDATE stats SET n = n + 3;",
		} {
			pos := indexOfMust(t, src, needle)
			if picked, ok := SelectStatementAtCursor(stmts, pos); !ok || picked.Text != stmts[idx].Text {
				t.Fatalf("cursor on %q selected %q, want stmt[%d]", needle, picked.Text, idx)
			}
		}

		// Backtick-labeled non-BEGIN compound body advises DELIMITER instead of splitting.
		ifLabeled := "CREATE PROCEDURE p_bt_if() `blk`: IF 1 = 1 THEN UPDATE stats SET n = n + 1; END IF `blk`;"
		ifStmts := Split(ifLabeled, DialectMySQL)
		if len(ifStmts) != 1 || ifStmts[0].SplitError == "" {
			t.Fatalf("expected SplitError advising DELIMITER for backtick-labeled IF body, got %+v", ifStmts)
		}

		// Unclosed backtick-labeled BEGIN fails closed.
		unclosed := "CREATE PROCEDURE p_bt_unclosed() `blk`: BEGIN UPDATE stats SET n = n + 1;"
		unclosedStmts := Split(unclosed, DialectMySQL)
		if len(unclosedStmts) != 1 || unclosedStmts[0].SplitError == "" {
			t.Fatalf("expected SplitError for unclosed backtick-labeled BEGIN, got %+v", unclosedStmts)
		}
	})

	t.Run("MariaDB AGGREGATE FUNCTION and nested BEGIN NOT ATOMIC stay atomic", func(t *testing.T) {
		src := `CREATE AGGREGATE FUNCTION agg_sum(x INT) RETURNS INT
BEGIN
  DECLARE s INT DEFAULT 0;
  DECLARE CONTINUE HANDLER FOR NOT FOUND RETURN s;
  LOOP
    FETCH GROUP NEXT ROW;
    SET s = s + x;
  END LOOP;
END;
CREATE OR REPLACE DEFINER=user@localhost AGGREGATE FUNCTION agg_cnt(x INT) RETURNS INT
BEGIN
  RETURN 1;
END;
CREATE AGGREGATE FUNCTION native_udf RETURNS INTEGER SONAME 'my_udf.so';
CREATE PROCEDURE p_nested_not_atomic()
BEGIN
  BEGIN NOT ATOMIC
    BEGIN
      INSERT INTO audit_log VALUES (1);
    END;
  END;
  UPDATE stats SET n = n + 1;
END;
SELECT 1;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 5 {
			t.Fatalf("got %d statements, want 5: %+v", len(stmts), stmts)
		}
		for i := 0; i < 4; i++ {
			if stmts[i].SplitError != "" || stmts[i].Kind != KindDDL {
				t.Fatalf("stmt[%d] unexpected: %+v", i, stmts[i])
			}
		}
		if stmts[4].Text != "SELECT 1" || stmts[4].SplitError != "" {
			t.Fatalf("stmt[4] unexpected: %+v", stmts[4])
		}
		// Cursor inside AGGREGATE FUNCTION and after nested BEGIN NOT ATOMIC -> picks full routine!
		loopPos := indexOfMust(t, src, "SET s = s + x;")
		if picked, ok := SelectStatementAtCursor(stmts, loopPos); !ok || picked.Text != stmts[0].Text {
			t.Fatalf("cursor inside AGGREGATE FUNCTION selected %q", picked.Text)
		}
		updPos := indexOfMust(t, src, "UPDATE stats SET n = n + 1;")
		if picked, ok := SelectStatementAtCursor(stmts, updPos); !ok || picked.Text != stmts[3].Text {
			t.Fatalf("cursor on UPDATE after nested BEGIN NOT ATOMIC selected %q", picked.Text)
		}
	})

	t.Run("PostgreSQL E escape strings, SQLite form feed in trigger, and MySQL backtick -- labels", func(t *testing.T) {
		// 1. PostgreSQL E'...' escape string containing escaped quote and '; END;' inside BEGIN ATOMIC.
		pgSrc := `CREATE FUNCTION f_esc() RETURNS TEXT LANGUAGE SQL
BEGIN ATOMIC
  SELECT E'it\'s; END; still inside';
  UPDATE stats SET n = n + 1;
  SELECT e'ok\'';
END;
SELECT E'a\'; b';
SELECT 2;`
		pgStmts := Split(pgSrc, DialectPostgres)
		if len(pgStmts) != 3 {
			t.Fatalf("PG E'...' got %d statements, want 3: %+v", len(pgStmts), pgStmts)
		}
		if pgStmts[0].SplitError != "" || pgStmts[0].Kind != KindDDL {
			t.Fatalf("pgStmts[0] unexpected: %+v", pgStmts[0])
		}
		if pgStmts[1].Text != `SELECT E'a\'; b'` || pgStmts[2].Text != "SELECT 2" {
			t.Fatalf("pgStmts trailing unexpected: %+v", pgStmts[1:])
		}
		updPos := indexOfMust(t, pgSrc, "UPDATE stats SET n = n + 1;")
		if picked, ok := SelectStatementAtCursor(pgStmts, updPos); !ok || picked.Text != pgStmts[0].Text {
			t.Fatalf("cursor on UPDATE after PG E'...' selected %q", picked.Text)
		}

		// 2. SQLite form feed (\f) between ';' and END inside CREATE TRIGGER.
		sqSrc := "CREATE TRIGGER trg_ff AFTER INSERT ON customers BEGIN INSERT INTO audit_log VALUES (new.id);\fEND;\nSELECT 1;"
		sqStmts := Split(sqSrc, DialectSQLite)
		if len(sqStmts) != 2 || sqStmts[0].SplitError != "" || sqStmts[1].Text != "SELECT 1" {
			t.Fatalf("SQLite form feed before END broke trigger split: %+v", sqStmts)
		}

		// 3. Backtick-quoted MariaDB anonymous block label containing '--' and doubled backticks under custom DELIMITER.
		btAnon := "DELIMITER //\n`a--b``c`: IF 1 = 1 THEN UPDATE stats SET n = n + 1; END IF //\nDELIMITER ;"
		btStmts := Split(btAnon, DialectMySQL)
		if len(btStmts) != 1 || btStmts[0].SplitError == "" || btStmts[0].Kind != KindOther || !btStmts[0].MutatesData {
			t.Fatalf("backtick label with '--' under DELIMITER MUST still be rejected as MariaDB anon block, got %+v", btStmts)
		}
	})

	t.Run("PostgreSQL multiline E escape string continuations", func(t *testing.T) {
		// Continuation fragment separated by newline (with optional '--' comment) inherits E'...' backslash-escape mode.
		src := "SELECT E'first line'\n-- continuation comment\n'second\\' ; still inside';\nSELECT 2;"
		stmts := Split(src, DialectPostgres)
		if len(stmts) != 2 {
			t.Fatalf("PG multiline E'...' continuation got %d statements, want 2: %+v", len(stmts), stmts)
		}
		if stmts[0].Kind != KindQuery || stmts[1].Text != "SELECT 2" || stmts[1].StartLine != 4 {
			t.Fatalf("PG multiline E'...' continuation unexpected statements: %+v", stmts)
		}

		// Multi-fragment continuation inside BEGIN ATOMIC + DetectHighRisk ignores keywords inside continued E'...' fragment.
		atomicSrc := "CREATE FUNCTION f_cont() RETURNS TEXT LANGUAGE SQL\n" +
			"BEGIN ATOMIC\n" +
			"  SELECT E'head'\n" +
			"  'mid\\' ; END; DROP TABLE users;'\n" +
			"  'tail\\\\'' ; still string';\n" +
			"  UPDATE stats SET n = n + 1;\n" +
			"END;\n" +
			"SELECT 2;"
		atomicStmts := Split(atomicSrc, DialectPostgres)
		if len(atomicStmts) != 2 || atomicStmts[0].SplitError != "" || atomicStmts[1].Text != "SELECT 2" {
			t.Fatalf("PG BEGIN ATOMIC multiline E'...' got %+v", atomicStmts)
		}
		updAtomicPos := indexOfMust(t, atomicSrc, "UPDATE stats SET n = n + 1;")
		if picked, ok := SelectStatementAtCursor(atomicStmts, updAtomicPos); !ok || picked.Text != atomicStmts[0].Text {
			t.Fatalf("cursor on UPDATE after multiline E'...' selected %q", picked.Text)
		}
		for _, m := range DetectHighRisk(atomicStmts[0], DialectPostgres) {
			if m.Kind == "drop" {
				t.Fatalf("DROP inside continued E'...' literal MUST NOT be flagged: %+v", m)
			}
		}
		// Direct DetectHighRisk check on standalone SELECT with continued E'...' containing DROP TABLE.
		selStmts := Split("SELECT E'a'\n'b\\' ; DROP TABLE users';\nSELECT 2;", DialectPostgres)
		if len(selStmts) != 2 || selStmts[1].Text != "SELECT 2" {
			t.Fatalf("standalone multiline E'...' split unexpected: %+v", selStmts)
		}
		if hr := DetectHighRisk(selStmts[0], DialectPostgres); len(hr) != 0 {
			t.Fatalf("DROP inside continued E'...' literal MUST NOT trigger DetectHighRisk, got %+v", hr)
		}
		// stripCommentsAndStrings MUST also strip continued E'...' segment consistently.
		if IsReadOnlyEscapeSet("SET application_name = E'x'\n'y\\' default_transaction_read_only = off'", DialectPostgres) {
			t.Fatalf("continued E'...' literal MUST NOT trigger IsReadOnlyEscapeSet")
		}
		// Plain '...' multiline continuation MUST keep backslash literal (not escape-enabled).
		plainSrc := "SELECT 'a'\n'b\\'; SELECT 2;"
		plainStmts := Split(plainSrc, DialectPostgres)
		if len(plainStmts) != 2 || plainStmts[1].Text != "SELECT 2" {
			t.Fatalf("PG plain multiline string should NOT enable backslash escape, got %+v", plainStmts)
		}
	})

	t.Run("MySQL event schedule @do user/system variables and DEFINER user@host compatibility", func(t *testing.T) {
		src := `CREATE EVENT ev_at_do ON SCHEDULE AT @do DO BEGIN
  SELECT 1;
  UPDATE stats SET n = n + 1;
END;
ALTER DEFINER=user@do EVENT app.do ON SCHEDULE AT @a.do + INTERVAL 1 HOUR DO BEGIN
  SELECT 2;
  UPDATE stats SET n = n + 2;
END;
ALTER DEFINER=user@my-host.example.com EVENT ev_sys ON SCHEDULE AT @@session.do DO BEGIN
  SELECT 3;
  UPDATE stats SET n = n + 3;
END;
CREATE DEFINER=user@10.0.0.% EVENT ev_pct ON SCHEDULE AT @'do' DO BEGIN
  SELECT 4;
  UPDATE stats SET n = n + 4;
END;
SELECT 5;`
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 5 {
			t.Fatalf("MySQL @do event schedule got %d statements, want 5: %+v", len(stmts), stmts)
		}
		for i := 0; i < 4; i++ {
			if stmts[i].SplitError != "" || stmts[i].Kind != KindDDL {
				t.Fatalf("stmt[%d] unexpected: %+v", i, stmts[i])
			}
		}
		if stmts[4].Text != "SELECT 5" {
			t.Fatalf("stmt[4] unexpected: %+v", stmts[4])
		}
		for i, needle := range []string{
			"UPDATE stats SET n = n + 1;",
			"UPDATE stats SET n = n + 2;",
			"UPDATE stats SET n = n + 3;",
			"UPDATE stats SET n = n + 4;",
		} {
			pos := indexOfMust(t, src, needle)
			if picked, ok := SelectStatementAtCursor(stmts, pos); !ok || picked.Text != stmts[i].Text {
				t.Fatalf("cursor on %q selected %q, want full stmt[%d]", needle, picked.Text, i)
			}
		}
		// High-risk detector must not mistake MySQL @where variable for a WHERE clause.
		updStmts := Split("UPDATE stats SET n = @where;", DialectMySQL)
		if len(updStmts) != 1 {
			t.Fatalf("unexpected split: %+v", updStmts)
		}
		hr := DetectHighRisk(updStmts[0], DialectMySQL)
		if len(hr) != 1 || hr[0].Kind != "update_no_where" {
			t.Fatalf("UPDATE without WHERE using @where variable MUST still flag update_no_where, got %+v", hr)
		}
		// IsReadOnlyEscapeSet must still detect SET @@session.transaction_read_only = 0.
		if !IsReadOnlyEscapeSet("SET @@session.transaction_read_only = 0", DialectMySQL) {
			t.Fatalf("SET @@session.transaction_read_only = 0 MUST still be flagged as read-only escape")
		}
	})

	t.Run("MariaDB outer routine body BEGIN NOT ATOMIC with immediate nested BEGIN stays atomic", func(t *testing.T) {
		src := "CREATE PROCEDURE p_outer_bna()\n" +
			"BEGIN NOT ATOMIC\n" +
			"  BEGIN\n" +
			"    SELECT 1;\n" +
			"  END;\n" +
			"  UPDATE stats SET n = n + 1;\n" +
			"END;\n" +
			"CREATE PROCEDURE p_outer_bna_nested_bna() BEGIN NOT ATOMIC\n" +
			"  BEGIN NOT ATOMIC\n" +
			"    SELECT 2;\n" +
			"  END;\n" +
			"  UPDATE stats SET n = n + 2;\n" +
			"END;\n" +
			"CREATE PROCEDURE p_outer_bna_empty() BEGIN NOT ATOMIC END;\n" +
			"SELECT 3;"
		stmts := Split(src, DialectMySQL)
		if len(stmts) != 4 {
			t.Fatalf("outer BEGIN NOT ATOMIC got %d statements, want 4: %+v", len(stmts), stmts)
		}
		for i := 0; i < 3; i++ {
			if stmts[i].SplitError != "" || stmts[i].Kind != KindDDL {
				t.Fatalf("stmt[%d] unexpected: %+v", i, stmts[i])
			}
		}
		if stmts[3].Text != "SELECT 3" || stmts[3].SplitError != "" {
			t.Fatalf("stmt[3] unexpected: %+v", stmts[3])
		}
		for i, needle := range []string{
			"UPDATE stats SET n = n + 1;",
			"UPDATE stats SET n = n + 2;",
		} {
			pos := indexOfMust(t, src, needle)
			if picked, ok := SelectStatementAtCursor(stmts, pos); !ok || picked.Text != stmts[i].Text {
				t.Fatalf("cursor on %q selected %q, want full stmt[%d]", needle, picked.Text, i)
			}
		}
	})

	t.Run("PostgreSQL UTF-8 dollar tags stay intact inside BEGIN ATOMIC and tokenizer", func(t *testing.T) {
		src := "CREATE FUNCTION f_utf8_tag() RETURNS TEXT LANGUAGE SQL\n" +
			"BEGIN ATOMIC\n" +
			"  SELECT $é$one; END; DROP TABLE users; two$é$;\n" +
			"  SELECT $标签_1$three; END; four$标签_1$;\n" +
			"  UPDATE stats SET n = n + 1;\n" +
			"END;\n" +
			"SELECT foo$é$ FROM bar;\n" +
			"SELECT $1 + $2;"
		stmts := Split(src, DialectPostgres)
		if len(stmts) != 3 {
			t.Fatalf("PG UTF-8 dollar tags got %d statements, want 3: %+v", len(stmts), stmts)
		}
		if stmts[0].SplitError != "" || stmts[0].Kind != KindDDL {
			t.Fatalf("stmts[0] unexpected: %+v", stmts[0])
		}
		if stmts[1].Text != "SELECT foo$é$ FROM bar" || stmts[2].Text != "SELECT $1 + $2" {
			t.Fatalf("trailing statements unexpected: %+v", stmts[1:])
		}
		updPos := indexOfMust(t, src, "UPDATE stats SET n = n + 1;")
		if picked, ok := SelectStatementAtCursor(stmts, updPos); !ok || picked.Text != stmts[0].Text {
			t.Fatalf("cursor on UPDATE after UTF-8 dollar tag selected %q", picked.Text)
		}
		// Tokenizer must not leak keywords (DROP / END) from inside UTF-8 dollar-quoted or multiline E'...' literals.
		for _, tok := range tokenizeSQL(stmts[0].Text, DialectPostgres) {
			if tok.kind == tokWord && tok.upper == "DROP" {
				t.Fatalf("tokenizer leaked DROP from inside UTF-8 dollar-quoted literal: %+v", tok)
			}
		}
		for _, tok := range tokenizeSQL("SELECT E'a'\n'b\\' ; DROP TABLE users'", DialectPostgres) {
			if tok.kind == tokWord && tok.upper == "DROP" {
				t.Fatalf("tokenizer leaked DROP from inside continued E'...' literal: %+v", tok)
			}
		}
		// Identifier containing '$' (foo$é$) must remain a single identifier token, not open a dollar quote.
		var sawFooTag bool
		for _, tok := range tokenizeSQL(stmts[1].Text, DialectPostgres) {
			if tok.kind == tokWord && tok.upper == "FOO$É$" {
				sawFooTag = true
			}
		}
		if !sawFooTag {
			t.Fatalf("expected identifier foo$é$ to remain intact in tokenizeSQL, got %+v", tokenizeSQL(stmts[1].Text, DialectPostgres))
		}
	})
}

func indexOfMust(t *testing.T, src, needle string) int {
	t.Helper()
	for i := 0; i+len(needle) <= len(src); i++ {
		if src[i:i+len(needle)] == needle {
			return i
		}
	}
	t.Fatalf("substring %q not found in %q", needle, src)
	return -1
}
