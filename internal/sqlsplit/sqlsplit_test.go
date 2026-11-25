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
}

func TestReadOnlyEscapeDetection(t *testing.T) {
	if !IsReadOnlyEscapeSet("SET SESSION transaction_read_only = 0", DialectMySQL) {
		t.Fatal("should detect MySQL read-only escape")
	}
	if !IsReadOnlyEscapeSet("SET default_transaction_read_only = off", DialectPostgres) {
		t.Fatal("should detect PG read-only escape")
	}
	if IsReadOnlyEscapeSet("SET NAMES utf8mb4", DialectMySQL) {
		t.Fatal("SET NAMES should not be flagged as read-only escape")
	}
}
