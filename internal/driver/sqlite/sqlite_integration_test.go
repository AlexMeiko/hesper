package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"hesper/internal/driver"
	"hesper/internal/driver/sqlite"
	"hesper/internal/dto"
	"hesper/internal/resultbuf"
	"hesper/internal/service"
	"hesper/internal/session"
)

func seedSQLiteTestFile(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "gate1_sqlite.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("seed open sqlite: %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE explicit_pk_items (
			id INTEGER PRIMARY KEY,
			snowflake_id BIGINT NOT NULL,
			amount_text TEXT NOT NULL,
			ratio REAL NULL,
			note TEXT NULL,
			meta JSON NULL,
			raw_bytes BLOB NULL
		);`,
		`INSERT INTO explicit_pk_items VALUES
			(1, 9223372036854775807, '1234567890123456789012345678.1234567890', 3.141592653589793, NULL, '{"engine":"sqlite"}', X'89504E470D0A1A0A'),
			(2, -9223372036854775808, '-0.0000000001', -0.0001, '', '{"empty":true}', X'DEADBEEF'),
			(3, 9007199254740993, '100.5000000000', NULL, 'SQLite 中文 🚀', NULL, NULL);`,

		`CREATE TABLE unique_not_null_codes (
			code TEXT NOT NULL UNIQUE,
			label TEXT NOT NULL
		);`,
		`INSERT INTO unique_not_null_codes VALUES ('A-001', 'alpha'), ('B-002', 'beta');`,

		`CREATE TABLE implicit_rowid_notes (
			title TEXT NOT NULL,
			body TEXT NULL
		);`,
		`INSERT INTO implicit_rowid_notes VALUES ('first-row', 'to-be-deleted'), ('second-row', 'will-shift-rowid-on-vacuum');`,

		`CREATE TABLE partial_unique_only (
			code TEXT NOT NULL,
			active INTEGER NOT NULL
		);`,
		`CREATE UNIQUE INDEX idx_partial_active ON partial_unique_only(code) WHERE active = 1;`,

		`CREATE TABLE events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			category TEXT NOT NULL,
			message TEXT NULL
		);`,
		`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 250)
		 INSERT INTO events(category, message)
		 SELECT CASE n % 3 WHEN 0 THEN 'auth' WHEN 1 THEN 'query' ELSE 'sync' END,
		        CASE WHEN n % 10 = 0 THEN NULL WHEN n % 10 = 5 THEN '' ELSE 'SQLiteEvent #' || n END
		 FROM seq;`,

		`CREATE VIEW v_explicit_items AS SELECT id, snowflake_id, amount_text FROM explicit_pk_items;`,

		`CREATE TABLE write_probe (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tag TEXT NOT NULL
		);`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed stmt failed (%s): %v", s, err)
		}
	}
	return dbPath
}

func newSQLiteHarness(t *testing.T, configs map[string]driver.ResolvedConfig, resOpts resultbuf.Options) (
	*session.Manager,
	*resultbuf.Manager,
	*service.SchemaService,
	*service.QueryService,
) {
	t.Helper()
	reg := driver.NewRegistry()
	reg.Register(sqlite.NewOpener())

	resolver := func(_ context.Context, connID string, _ string) (driver.ResolvedConfig, error) {
		cfg, ok := configs[connID]
		if !ok {
			return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrNotFound, "unknown test connection", connID)
		}
		return cfg, nil
	}

	if resOpts.SpillDir == "" {
		resOpts.SpillDir = filepath.Join(t.TempDir(), "results")
	}
	resMgr := resultbuf.NewManager(resOpts)
	sessMgr := session.NewManager(reg, resolver)
	schemaSvc := service.NewSchemaService(sessMgr)
	querySvc := service.NewQueryService(sessMgr, resMgr, nil)

	t.Cleanup(func() {
		sessMgr.CloseAll()
		resMgr.CloseAll()
	})
	return sessMgr, resMgr, schemaSvc, querySvc
}

func TestSQLite354_Gate1_AcceptanceAndVacuumRowidRegression(t *testing.T) {
	ctx := context.Background()
	dbPath := seedSQLiteTestFile(t)
	missingPath := filepath.Join(t.TempDir(), "does_not_exist.db")

	configs := map[string]driver.ResolvedConfig{
		"sq-rw": {
			ConnectionID: "sq-rw",
			Engine:       dto.EngineSQLite,
			FilePath:     dbPath,
			ReadOnly:     false,
		},
		"sq-ro": {
			ConnectionID: "sq-ro",
			Engine:       dto.EngineSQLite,
			FilePath:     dbPath,
			ReadOnly:     true,
		},
		"sq-missing": {
			ConnectionID: "sq-missing",
			Engine:       dto.EngineSQLite,
			FilePath:     missingPath,
			ReadOnly:     true,
		},
	}

	sessMgr, resMgr, schemaSvc, querySvc := newSQLiteHarness(t, configs, resultbuf.Options{
		MaxMemRows:  40,
		MaxMemBytes: 16 * 1024,
		IdleTTL:     350 * time.Millisecond,
	})

	// 0. Nonexistent file must fail cleanly (never silently create empty file!)
	if err := sessMgr.WithMetaConn(ctx, "sq-missing", "", func(driver.Conn) error { return nil }); err == nil {
		t.Fatalf("expected error when opening nonexistent SQLite file")
	}

	// 1. Capabilities & Metadata check
	var fact driver.ServerFact
	var caps dto.Capabilities
	err := sessMgr.WithMetaConn(ctx, "sq-rw", "", func(conn driver.Conn) error {
		var ferr error
		fact, ferr = conn.ServerFact(ctx)
		if ferr != nil {
			return ferr
		}
		caps = conn.Capabilities(fact.ServerVersion)
		return nil
	})
	if err != nil {
		t.Fatalf("Connect SQLite failed: %v", err)
	}
	t.Logf("Connected real SQLite target driver: version=%s hasDatabases=%v hasSchemas=%v supportsSequences=%v",
		fact.ServerVersion, caps.HasDatabases, caps.HasSchemas, caps.SupportsSequences)
	if fact.DetectedType != dto.EngineSQLite || caps.HasDatabases || caps.HasSchemas || caps.SupportsSequences || !caps.DDLTransactional {
		t.Fatalf("unexpected SQLite capabilities: fact=%+v caps=%+v", fact, caps)
	}

	tables, err := schemaSvc.ListTables(ctx, "sq-rw", "", "")
	if err != nil || len(tables) < 5 {
		t.Fatalf("ListTables failed: len=%d err=%v", len(tables), err)
	}
	views, err := schemaSvc.ListViews(ctx, "sq-rw", "", "")
	if err != nil || len(views) != 1 || views[0].Name != "v_explicit_items" {
		t.Fatalf("ListViews failed: views=%+v err=%v", views, err)
	}
	ddl, err := schemaSvc.GetTableDDL(ctx, "sq-rw", "", "", "explicit_pk_items")
	if err != nil || ddl == "" {
		t.Fatalf("GetTableDDL failed: ddl=%q err=%v", ddl, err)
	}

	// 2. Section 3.2 Mandatory Regression: VACUUM shifts implicit rowid + Editability MUST reject implicit rowid!
	// Verify VACUUM actually shifts implicit rowid on implicit_rowid_notes:
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite for VACUUM check: %v", err)
	}
	var rowidBefore, rowidAfter int64
	if err := rawDB.QueryRow(`SELECT rowid FROM implicit_rowid_notes WHERE title = 'second-row'`).Scan(&rowidBefore); err != nil {
		t.Fatalf("query rowid before: %v", err)
	}
	if _, err := rawDB.Exec(`DELETE FROM implicit_rowid_notes WHERE title = 'first-row'; VACUUM;`); err != nil {
		t.Fatalf("delete + vacuum: %v", err)
	}
	if err := rawDB.QueryRow(`SELECT rowid FROM implicit_rowid_notes WHERE title = 'second-row'`).Scan(&rowidAfter); err != nil {
		t.Fatalf("query rowid after: %v", err)
	}
	_ = rawDB.Close()
	if rowidBefore != 2 || rowidAfter != 1 {
		t.Fatalf("expected VACUUM to shift implicit rowid from 2 -> 1, got %d -> %d", rowidBefore, rowidAfter)
	}
	t.Logf("Reproduced SQLite VACUUM implicit rowid shift: second-row rowid %d -> %d", rowidBefore, rowidAfter)

	// Assert InspectTableEditability enforces explicit key rules:
	pkDet, err := schemaSvc.GetTableDetail(ctx, "sq-rw", "", "", "explicit_pk_items")
	if err != nil || pkDet.Editability.RowIdentity != dto.RowIdentityPrimaryKey || len(pkDet.Editability.KeyColumns) != 1 || pkDet.Editability.KeyColumns[0] != "id" {
		t.Fatalf("expected explicit INTEGER PRIMARY KEY to yield RowIdentityPrimaryKey, got %+v err=%v", pkDet, err)
	}
	uniqDet, err := schemaSvc.GetTableDetail(ctx, "sq-rw", "", "", "unique_not_null_codes")
	if err != nil || uniqDet.Editability.RowIdentity != dto.RowIdentityUniqueNotNull || len(uniqDet.Editability.KeyColumns) != 1 || uniqDet.Editability.KeyColumns[0] != "code" {
		t.Fatalf("expected UNIQUE NOT NULL to yield RowIdentityUniqueNotNull, got %+v err=%v", uniqDet, err)
	}
	rowidDet, err := schemaSvc.GetTableDetail(ctx, "sq-rw", "", "", "implicit_rowid_notes")
	if err != nil || rowidDet.Editability.RowIdentity != dto.RowIdentityNone {
		t.Fatalf("REGRESSION: implicit rowid table MUST return RowIdentityNone, got %+v err=%v", rowidDet, err)
	}
	partialDet, err := schemaSvc.GetTableDetail(ctx, "sq-rw", "", "", "partial_unique_only")
	if err != nil || partialDet.Editability.RowIdentity != dto.RowIdentityNone {
		t.Fatalf("partial unique index MUST NOT qualify as RowIdentityUniqueNotNull, got %+v err=%v", partialDet, err)
	}

	// 3. True Engine-Level Read-Only File Opening (mode=ro + query_only=ON)
	// First verify direct driver-level Exec (bypassing client guardrail!) is rejected by SQLite engine itself!
	roConn, err := sqlite.NewOpener().Open(ctx, configs["sq-ro"])
	if err != nil {
		t.Fatalf("open read-only sqlite file: %v", err)
	}
	_, engineWriteErr := roConn.(driver.QueryExecutor).Exec(ctx, driver.BoundStatement{
		SQL: "INSERT INTO write_probe(tag) VALUES ('must-fail-engine-ro')",
	})
	_ = roConn.Close()
	if engineWriteErr == nil {
		t.Fatalf("expected SQLite mode=ro engine to reject direct write")
	}
	roAppErr := roConn.ClassifyError(engineWriteErr, false)
	if roAppErr.Code != dto.ErrReadOnlyDenied {
		t.Fatalf("expected ERR_READ_ONLY_DENIED from SQLite mode=ro, got %+v", roAppErr)
	}
	t.Logf("Verified SQLite engine-level mode=ro rejected direct write: %s", roAppErr.Error())

	// Also verify QueryService read-only tab blocks INSERT and PRAGMA query_only = OFF
	roTab := "sq-tab-ro"
	roResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: roTab, ExecID: "sq-ro-1", ConnectionID: "sq-ro",
		SQL: "PRAGMA query_only = OFF;",
	})
	if err != nil || roResp.Error == nil || roResp.Error.Code != dto.ErrReadOnlyDenied {
		t.Fatalf("expected PRAGMA query_only escape blocked with ERR_READ_ONLY_DENIED, got err=%v resp=%+v", err, roResp)
	}
	_ = querySvc.CloseTab(roTab)

	// 4. Criterion 1: SQLite query cancel + session/tx/temp table preservation
	tabID := "sq-tab-rw"
	if _, err := querySvc.OpenTabSession(ctx, tabID, "sq-rw", "main", ""); err != nil {
		t.Fatalf("OpenTabSession sq-rw: %v", err)
	}
	txSetup, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "sq-tx-setup", ConnectionID: "sq-rw",
		SQL: "CREATE TEMP TABLE tmp_sq_keep (v TEXT); INSERT INTO tmp_sq_keep VALUES ('kept'); BEGIN;",
	})
	if err != nil || txSetup.Error != nil || txSetup.SessionState != dto.SessionAliveInTx {
		t.Fatalf("tx setup failed: err=%v resp=%+v", err, txSetup)
	}

	var slowResp *dto.ExecuteSQLResponse
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowResp, _ = querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID: tabID, ExecID: "sq-slow-A", ConnectionID: "sq-rw",
			SQL: "WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM cnt WHERE x < 50000000) SELECT SUM(x) FROM cnt",
		})
	}()
	time.Sleep(80 * time.Millisecond)
	cancelRes := querySvc.CancelExecution(ctx, tabID, "sq-slow-A")
	wg.Wait()
	if slowResp == nil || slowResp.Error == nil || slowResp.Error.Code != dto.ErrStatementCanceled {
		t.Fatalf("expected ERR_STATEMENT_CANCELED on SQLite cancel, got resp=%+v cancel=%+v", slowResp, cancelRes)
	}
	// Verify session + temp table + queued query B survive intact
	afterCancel, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "sq-after-cancel", ConnectionID: "sq-rw",
		SQL: "SELECT v FROM tmp_sq_keep; ROLLBACK;",
	})
	if err != nil || afterCancel.Error != nil || afterCancel.Statements[0].Rows[0][0].Text != "kept" {
		t.Fatalf("SQLite session/temp table did not survive cancel: err=%v resp=%+v", err, afterCancel)
	}

	// 5. Criterion 2: Execute-Once INSERT ... RETURNING * + Snapshot Spill/Expiry + Live Browse + CloseTab
	retResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "sq-ret", ConnectionID: "sq-rw",
		SQL: "INSERT INTO write_probe (tag) VALUES ('sqlite-once') RETURNING id, tag;",
	})
	if err != nil || retResp.Error != nil || !retResp.Statements[0].IsResultSet {
		t.Fatalf("SQLite INSERT ... RETURNING failed: err=%v resp=%+v", err, retResp)
	}
	if _, err := querySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: retResp.Statements[0].ResultID, Page: 1, PageSize: 10}); err != nil {
		t.Fatalf("FetchResultPage on RETURNING failed: %v", err)
	}
	cntResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "sq-ret-cnt", ConnectionID: "sq-rw",
		SQL: "SELECT COUNT(*) FROM write_probe WHERE tag = 'sqlite-once'",
	})
	if err != nil || cntResp.Statements[0].Rows[0][0].Text != "1" {
		t.Fatalf("expected strictly 1 row from Execute-Once RETURNING, got %+v", cntResp)
	}

	// Spill + TTL expiry
	evResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "sq-ev", ConnectionID: "sq-rw",
		SQL: "SELECT id, category, message FROM events ORDER BY id", PageSize: 60,
	})
	if err != nil || evResp.Error != nil {
		t.Fatalf("events query failed: %v / %v", err, evResp.Error)
	}
	p2, err := querySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: evResp.Statements[0].ResultID, Page: 2, PageSize: 60})
	if err != nil || len(p2.Rows) != 60 || p2.Rows[0][0].Text != "61" {
		t.Fatalf("page 2 snapshot failed: err=%v p2=%+v", err, p2)
	}
	time.Sleep(450 * time.Millisecond)
	if _, expErr := querySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: evResp.Statements[0].ResultID, Page: 3, PageSize: 60}); expErr == nil {
		t.Fatalf("expected ERR_RESULT_EXPIRED after IdleTTL")
	}

	// Live BrowseTable
	browse, err := querySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: "sq-rw", Database: "main", Table: "events", Page: 2, PageSize: 50,
	})
	if err != nil || len(browse.Rows) != 50 || browse.TotalRows != 250 || !browse.StableOrderUsed {
		t.Fatalf("SQLite BrowseTable failed: err=%v browse=%+v", err, browse)
	}

	// 6. Criterion 3: Data fidelity (64-bit BIGINT, TEXT decimal, REAL float, NULL vs "", UTF-8, BLOB /stream)
	fidResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "sq-fid", ConnectionID: "sq-rw",
		SQL: "SELECT id, snowflake_id, amount_text, ratio, note, raw_bytes FROM explicit_pk_items ORDER BY id ASC",
	})
	if err != nil || fidResp.Error != nil {
		t.Fatalf("SQLite fidelity query failed: err=%v resp=%+v", err, fidResp)
	}
	r0 := fidResp.Statements[0].Rows[0]
	r1 := fidResp.Statements[0].Rows[1]
	r2 := fidResp.Statements[0].Rows[2]
	if r0[1].Text != "9223372036854775807" || r0[2].Text != "1234567890123456789012345678.1234567890" || r0[3].Text != "3.141592653589793" || r0[4].Kind != dto.KindNull {
		t.Fatalf("SQLite row 0 fidelity mismatch: %+v", r0)
	}
	if r1[1].Text != "-9223372036854775808" || r1[2].Text != "-0.0000000001" || r1[3].Text != "-0.0001" || r1[4].Kind != dto.KindString || r1[4].Text != "" {
		t.Fatalf("SQLite row 1 fidelity mismatch: %+v", r1)
	}
	if r2[1].Text != "9007199254740993" || r2[3].Kind != dto.KindNull || r2[4].Text != "SQLite 中文 🚀" {
		t.Fatalf("SQLite row 2 fidelity mismatch: %+v", r2)
	}
	if r0[5].Kind != dto.KindBinary || r0[5].StreamRef == "" {
		t.Fatalf("expected SQLite BLOB StreamRef, got %+v", r0[5])
	}
	blob, ok := querySvc.GetStreamBlob(r0[5].StreamRef)
	if !ok || len(blob.Data) != 8 {
		t.Fatalf("expected 8-byte BLOB via /stream, got ok=%v blob=%+v", ok, blob)
	}

	// CloseTab releases connection & snapshots
	if err := querySvc.CloseTab(tabID); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if sessMgr.ActiveTabCount() != 0 || resMgr.ActiveCount() != 0 {
		t.Fatalf("CloseTab leaked resources: tabs=%d snaps=%d", sessMgr.ActiveTabCount(), resMgr.ActiveCount())
	}
}
