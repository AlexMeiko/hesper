package sqlsplit

import "testing"

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
