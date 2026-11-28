package service_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"hesper/internal/changeset"
	"hesper/internal/driver"
	sqlitedrv "hesper/internal/driver/sqlite"
	"hesper/internal/dto"
	"hesper/internal/repository"
	"hesper/internal/resultbuf"
	"hesper/internal/secret"
	"hesper/internal/service"
	"hesper/internal/session"
)

type sqliteStack struct {
	dbPath    string
	connID    string
	roConnID  string
	connSvc   *service.ConnectionService
	schemaSvc *service.SchemaService
	querySvc  *service.QueryService
	changeSvc *service.ChangeService
	sessMgr   *session.Manager
	rawDB     *sql.DB
}

func newSQLiteStack(t *testing.T, initDDLs []string) *sqliteStack {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "target.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	for _, stmt := range initDDLs {
		if _, err := rawDB.Exec(stmt); err != nil {
			t.Fatalf("init ddl %q failed: %v", stmt, err)
		}
	}

	localDB, err := repository.OpenLocalDB(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatalf("open local app.db: %v", err)
	}
	t.Cleanup(func() { _ = localDB.Close() })

	reg := driver.NewRegistry()
	reg.Register(sqlitedrv.NewOpener())
	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: filepath.Join(dir, "spill")})
	t.Cleanup(func() { resMgr.CloseAll() })

	connSvc := service.NewConnectionService(localDB, secret.NewStore(dir), nil, resMgr)
	sessMgr := session.NewManager(reg, connSvc.ResolveConfig)
	connSvc.SetSessionManager(sessMgr)
	t.Cleanup(func() { sessMgr.CloseAll() })

	rwConn, err := connSvc.SaveConnection(context.Background(), dto.SaveConnectionRequest{
		Name:     "SQLite RW",
		Driver:   dto.EngineSQLite,
		EnvColor: "green",
		ReadOnly: false,
		Params:   dto.ConnectionParams{FilePath: dbPath},
	})
	if err != nil {
		t.Fatalf("save rw conn: %v", err)
	}
	roConn, err := connSvc.SaveConnection(context.Background(), dto.SaveConnectionRequest{
		Name:     "SQLite RO",
		Driver:   dto.EngineSQLite,
		EnvColor: "green",
		ReadOnly: true,
		Params:   dto.ConnectionParams{FilePath: dbPath},
	})
	if err != nil {
		t.Fatalf("save ro conn: %v", err)
	}

	return &sqliteStack{
		dbPath:    dbPath,
		connID:    rwConn.ID,
		roConnID:  roConn.ID,
		connSvc:   connSvc,
		schemaSvc: service.NewSchemaService(sessMgr),
		querySvc:  service.NewQueryService(sessMgr, resMgr, localDB),
		changeSvc: service.NewChangeService(sessMgr, localDB, changeset.NewPlanStore(changeset.PlanStoreOptions{})),
		sessMgr:   sessMgr,
		rawDB:     rawDB,
	}
}

func cellToOrig(c dto.CellValue) dto.OriginalCellValue {
	return dto.OriginalCellValue{
		Kind:         c.Kind,
		Text:         c.Text,
		Truncated:    c.Truncated,
		StorageClass: c.StorageClass,
	}
}

func assertErrCode(t *testing.T, err error, want dto.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	var ae *dto.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *dto.AppError(%s), got %T: %v", want, err, err)
	}
	if ae.Code != want {
		t.Fatalf("expected error code %s, got %s (msg=%q detail=%q)", want, ae.Code, ae.Message, ae.Detail)
	}
}

func assertApplyOutcome(t *testing.T, resp dto.ApplyGridChangesResponse, err error, wantOutcome dto.GridApplyOutcome, wantCode dto.ErrorCode) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected (resp, nil) for terminal ApplyGridChanges outcome %s so Wails resolves resp, got err=%v (resp=%+v)", wantOutcome, err, resp)
	}
	if resp.Outcome != wantOutcome {
		t.Fatalf("expected outcome %s, got %+v", wantOutcome, resp)
	}
	if wantCode != "" {
		if resp.Error == nil || resp.Error.Code != wantCode {
			t.Fatalf("expected resp.Error.Code=%s, got %+v", wantCode, resp.Error)
		}
	}
}

func TestSQLiteReal354StableKeyPositivesAndNegatives(t *testing.T) {
	st := newSQLiteStack(t, []string{
		// Positives:
		`CREATE TABLE t_int_pk (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE t_without_rowid (k TEXT PRIMARY KEY, val TEXT) WITHOUT ROWID`,
		`CREATE TABLE t_strict_pk (id TEXT PRIMARY KEY, val TEXT) STRICT`,
		`CREATE TABLE t_comp_notnull (a TEXT NOT NULL, b INTEGER NOT NULL, val TEXT, PRIMARY KEY (a, b))`,
		`CREATE TABLE t_unique_notnull (code TEXT NOT NULL UNIQUE, val TEXT)`,
		// Negatives (nullable PKs on non-STRICT rowid tables + implicit rowid table):
		`CREATE TABLE t_nullable_text_pk (id TEXT PRIMARY KEY, val TEXT)`,
		`CREATE TABLE t_nullable_int_pk (id INT PRIMARY KEY, val TEXT)`,
		`CREATE TABLE t_int_pk_desc (id INTEGER PRIMARY KEY DESC, val TEXT)`,
		`CREATE TABLE t_comp_nullable_pk (a TEXT, b TEXT, val TEXT, PRIMARY KEY (a, b))`,
		`CREATE TABLE t_no_key (val TEXT)`,
	})

	// Prove SQLite 3.54 indeed permits duplicate (NULL, 'k') rows on non-STRICT composite PRIMARY KEY(a, b)!
	if _, err := st.rawDB.Exec(`INSERT INTO t_comp_nullable_pk (a, b, val) VALUES (NULL, 'k', 'r1'), (NULL, 'k', 'r2')`); err != nil {
		t.Fatalf("expected SQLite to accept duplicate (NULL,'k') in non-STRICT composite PK: %v", err)
	}

	ctx := context.Background()

	// Verify positive tables classify properly and support INSERT -> UPDATE -> DELETE end-to-end.
	positives := []struct {
		table     string
		wantIdent dto.RowIdentityKind
		insertRow map[string]dto.NewCellValue
		updCol    string
		updVal    string
	}{
		{
			table: "t_int_pk", wantIdent: dto.RowIdentityPrimaryKey,
			insertRow: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "v1"}},
			updCol:    "name", updVal: "v2",
		},
		{
			table: "t_without_rowid", wantIdent: dto.RowIdentityPrimaryKey,
			insertRow: map[string]dto.NewCellValue{
				"k":   {Mode: dto.CellInputLiteral, Text: "k1"},
				"val": {Mode: dto.CellInputLiteral, Text: "v1"},
			},
			updCol: "val", updVal: "v2",
		},
		{
			table: "t_strict_pk", wantIdent: dto.RowIdentityPrimaryKey,
			insertRow: map[string]dto.NewCellValue{
				"id":  {Mode: dto.CellInputLiteral, Text: "s1"},
				"val": {Mode: dto.CellInputLiteral, Text: "v1"},
			},
			updCol: "val", updVal: "v2",
		},
		{
			table: "t_comp_notnull", wantIdent: dto.RowIdentityPrimaryKey,
			insertRow: map[string]dto.NewCellValue{
				"a":   {Mode: dto.CellInputLiteral, Text: "x"},
				"b":   {Mode: dto.CellInputLiteral, Text: "7"},
				"val": {Mode: dto.CellInputLiteral, Text: "v1"},
			},
			updCol: "val", updVal: "v2",
		},
		{
			table: "t_unique_notnull", wantIdent: dto.RowIdentityUniqueNotNull,
			insertRow: map[string]dto.NewCellValue{
				"code": {Mode: dto.CellInputLiteral, Text: "u1"},
				"val":  {Mode: dto.CellInputLiteral, Text: "v1"},
			},
			updCol: "val", updVal: "v2",
		},
	}

	for _, tc := range positives {
		t.Run("positive_"+tc.table, func(t *testing.T) {
			detail, err := st.schemaSvc.GetTableDetail(ctx, st.connID, "main", "", tc.table)
			if err != nil {
				t.Fatalf("GetTableDetail: %v", err)
			}
			if detail.Editability.RowIdentity != tc.wantIdent {
				t.Fatalf("%s: want RowIdentity=%s, got %+v", tc.table, tc.wantIdent, detail.Editability)
			}

			// 1. INSERT via ChangeService.
			prevIns, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: st.connID, Database: "main", Table: tc.table,
				Inserts: []dto.GridRowInsert{{RowKey: "i1", Values: tc.insertRow}},
			})
			if err != nil {
				t.Fatalf("preview insert: %v", err)
			}
			resIns, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevIns.PlanID})
			if err != nil || resIns.Outcome != dto.GridApplyCommitted || resIns.AppliedCount != 1 {
				t.Fatalf("apply insert failed: resp=%+v err=%v", resIns, err)
			}

			// 2. BrowseTable and extract authoritative original values (including StorageClass!).
			browse, err := st.querySvc.BrowseTable(ctx, dto.BrowseTableRequest{
				ConnectionID: st.connID, Database: "main", Table: tc.table, Page: 1, PageSize: 20,
			})
			if err != nil || len(browse.Rows) != 1 {
				t.Fatalf("browse %s: err=%v rows=%d", tc.table, err, len(browse.Rows))
			}
			colIdx := make(map[string]int, len(browse.Columns))
			for idx, c := range browse.Columns {
				colIdx[c.Name] = idx
			}
			keyVals := make(map[string]dto.OriginalCellValue)
			for _, k := range detail.Editability.KeyColumns {
				keyVals[k] = cellToOrig(browse.Rows[0][colIdx[k]])
			}

			// 3. UPDATE via ChangeService.
			prevUpd, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: st.connID, Database: "main", Table: tc.table,
				Updates: []dto.GridRowUpdate{{
					RowKey:         "u1",
					KeyValues:      keyVals,
					OriginalValues: map[string]dto.OriginalCellValue{tc.updCol: cellToOrig(browse.Rows[0][colIdx[tc.updCol]])},
					NewValues:      map[string]dto.NewCellValue{tc.updCol: {Mode: dto.CellInputLiteral, Text: tc.updVal}},
				}},
			})
			if err != nil {
				t.Fatalf("preview update: %v", err)
			}
			resUpd, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevUpd.PlanID})
			if err != nil || resUpd.Outcome != dto.GridApplyCommitted || resUpd.AppliedCount != 1 {
				t.Fatalf("apply update failed: resp=%+v err=%v", resUpd, err)
			}

			// 4. DELETE via ChangeService.
			prevDel, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: st.connID, Database: "main", Table: tc.table,
				Deletes: []dto.GridRowDelete{{RowKey: "d1", KeyValues: keyVals}},
			})
			if err != nil {
				t.Fatalf("preview delete: %v", err)
			}
			resDel, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevDel.PlanID})
			if err != nil || resDel.Outcome != dto.GridApplyCommitted || resDel.AppliedCount != 1 {
				t.Fatalf("apply delete failed: resp=%+v err=%v", resDel, err)
			}
		})
	}

	// Verify negative tables classify as RowIdentityNone, forbid UPDATE/DELETE, and allow INSERT.
	negatives := []string{"t_nullable_text_pk", "t_nullable_int_pk", "t_int_pk_desc", "t_comp_nullable_pk", "t_no_key"}
	for _, tbl := range negatives {
		t.Run("negative_"+tbl, func(t *testing.T) {
			detail, err := st.schemaSvc.GetTableDetail(ctx, st.connID, "main", "", tbl)
			if err != nil {
				t.Fatalf("GetTableDetail %s: %v", tbl, err)
			}
			if detail.Editability.RowIdentity != dto.RowIdentityNone {
				t.Fatalf("%s: expected RowIdentityNone, got %+v", tbl, detail.Editability)
			}

			// UPDATE / DELETE must be rejected with ErrRowNotIdentifiable.
			_, err = st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: st.connID, Database: "main", Table: tbl,
				Deletes: []dto.GridRowDelete{{RowKey: "d1", KeyValues: map[string]dto.OriginalCellValue{"val": {Kind: dto.KindString, Text: "x"}}}},
			})
			assertErrCode(t, err, dto.ErrRowNotIdentifiable)

			// INSERT must still succeed!
			prevIns, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: st.connID, Database: "main", Table: tbl,
				Inserts: []dto.GridRowInsert{{RowKey: "i1", Values: map[string]dto.NewCellValue{"val": {Mode: dto.CellInputLiteral, Text: "ok"}}}},
			})
			if err != nil {
				t.Fatalf("%s: INSERT preview should succeed: %v", tbl, err)
			}
			resIns, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevIns.PlanID})
			if err != nil || resIns.Outcome != dto.GridApplyCommitted {
				t.Fatalf("%s: INSERT apply failed: resp=%+v err=%v", tbl, resIns, err)
			}
		})
	}

	// VACUUM implicit-rowid regression + mode=ro read-only check.
	if _, err := st.rawDB.Exec(`VACUUM`); err != nil {
		t.Fatalf("VACUUM failed: %v", err)
	}
	_, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.roConnID, Database: "main", Table: "t_int_pk",
		Inserts: []dto.GridRowInsert{{RowKey: "ro1", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "blocked"}}}},
	})
	assertErrCode(t, err, dto.ErrReadOnlyDenied)
}

func TestSQLiteReal354NoopTriggerNullGeneratedCollationDatetimeAndPrecision(t *testing.T) {
	st := newSQLiteStack(t, []string{
		`CREATE TABLE audit_counter (cnt INTEGER NOT NULL)`,
		`INSERT INTO audit_counter VALUES (0)`,
		`CREATE TABLE items (
			id INTEGER PRIMARY KEY,
			name TEXT COLLATE NOCASE,
			note TEXT,
			dyn,
			ratio REAL,
			created_at DATETIME,
			amt DECIMAL(38,10),
			v_upper TEXT GENERATED ALWAYS AS (upper(name)) VIRTUAL,
			s_len INTEGER GENERATED ALWAYS AS (length(name)) STORED
		)`,
		`CREATE TRIGGER trg_items_after_upd AFTER UPDATE ON items BEGIN UPDATE audit_counter SET cnt = cnt + 1; END`,
		`INSERT INTO items (id, name, note, dyn, ratio, created_at, amt) VALUES
			(1, 'Alice', NULL, 1, 0.1, '2026-11-27T12:00:00Z', '2026-11-27T12:00:00Z')`,
	})
	ctx := context.Background()

	// 1. Verify PRAGMA table_xinfo identifies VIRTUAL and STORED generated columns accurately.
	detail, err := st.schemaSvc.GetTableDetail(ctx, st.connID, "main", "", "items")
	if err != nil {
		t.Fatalf("GetTableDetail: %v", err)
	}
	colMap := make(map[string]dto.ColumnDetail, len(detail.Columns))
	for _, c := range detail.Columns {
		colMap[c.Name] = c
	}
	if colMap["v_upper"].GeneratedKind != "virtual" || colMap["v_upper"].Insertable || colMap["v_upper"].Updatable {
		t.Fatalf("v_upper generated metadata mismatch: %+v", colMap["v_upper"])
	}
	if colMap["s_len"].GeneratedKind != "stored" || colMap["s_len"].Insertable || colMap["s_len"].Updatable {
		t.Fatalf("s_len generated metadata mismatch: %+v", colMap["s_len"])
	}

	// Explicit write to generated column must fail; omitting it on INSERT succeeds.
	_, err = st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "items",
		Inserts: []dto.GridRowInsert{{RowKey: "bad-gen", Values: map[string]dto.NewCellValue{"v_upper": {Mode: dto.CellInputLiteral, Text: "X"}}}},
	})
	assertErrCode(t, err, dto.ErrInvalidArgument)

	// 2. BrowseTable preserves verbatim DATETIME text ('2026-11-27T12:00:00Z') and populates StorageClass!
	browse, err := st.querySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: st.connID, Database: "main", Table: "items", Page: 1, PageSize: 20,
	})
	if err != nil || len(browse.Rows) != 1 {
		t.Fatalf("BrowseTable: err=%v rows=%d", err, len(browse.Rows))
	}
	idxOf := make(map[string]int, len(browse.Columns))
	for i, c := range browse.Columns {
		idxOf[c.Name] = i
	}
	row := browse.Rows[0]
	if row[idxOf["created_at"]].Text != "2026-11-27T12:00:00Z" || row[idxOf["created_at"]].StorageClass != "text" {
		t.Fatalf("expected verbatim DATETIME text with StorageClass=text, got %+v", row[idxOf["created_at"]])
	}
	if row[idxOf["dyn"]].StorageClass != "integer" || row[idxOf["ratio"]].StorageClass != "real" || row[idxOf["note"]].Kind != dto.KindNull {
		t.Fatalf("unexpected StorageClass values: dyn=%+v ratio=%+v note=%+v", row[idxOf["dyn"]], row[idxOf["ratio"]], row[idxOf["note"]])
	}

	key1 := map[string]dto.OriginalCellValue{"id": cellToOrig(row[idxOf["id"]])}

	// 3. No-op update emits 0 statements and leaves AFTER UPDATE trigger count == 0!
	prevNoop, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "items",
		Updates: []dto.GridRowUpdate{{
			RowKey:    "noop-1",
			KeyValues: key1,
			OriginalValues: map[string]dto.OriginalCellValue{
				"name":       cellToOrig(row[idxOf["name"]]),
				"note":       cellToOrig(row[idxOf["note"]]),
				"created_at": cellToOrig(row[idxOf["created_at"]]),
			},
			NewValues: map[string]dto.NewCellValue{
				"name":       {Mode: dto.CellInputLiteral, Text: "Alice"},
				"note":       {Mode: dto.CellInputNull},
				"created_at": {Mode: dto.CellInputLiteral, Text: "2026-11-27T12:00:00Z"},
			},
		}},
	})
	if err != nil || len(prevNoop.Steps) != 0 {
		t.Fatalf("expected 0 steps for no-op edit, got len=%d err=%v", len(prevNoop.Steps), err)
	}
	resNoop, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevNoop.PlanID})
	if err != nil || resNoop.Outcome != dto.GridApplyCommitted || resNoop.AppliedCount != 0 {
		t.Fatalf("apply no-op failed: resp=%+v err=%v", resNoop, err)
	}
	var triggerCnt int
	if err := st.rawDB.QueryRow(`SELECT cnt FROM audit_counter`).Scan(&triggerCnt); err != nil {
		t.Fatalf("query audit_counter: %v", err)
	}
	if triggerCnt != 0 {
		t.Fatalf("expected AFTER UPDATE trigger count == 0 for no-op, got %d", triggerCnt)
	}

	// 4. Update NULL -> value, existing REAL 0.1 -> 0.25, and verbatim DATETIME text -> new ISO string.
	prevUpd, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "items",
		Updates: []dto.GridRowUpdate{{
			RowKey:    "upd-real-null-dt",
			KeyValues: key1,
			OriginalValues: map[string]dto.OriginalCellValue{
				"note":       cellToOrig(row[idxOf["note"]]),
				"ratio":      cellToOrig(row[idxOf["ratio"]]),
				"created_at": cellToOrig(row[idxOf["created_at"]]),
			},
			NewValues: map[string]dto.NewCellValue{
				"note":       {Mode: dto.CellInputLiteral, Text: "1234567890123456789012345678.1234567890"}, // TEXT arbitrary precision!
				"ratio":      {Mode: dto.CellInputLiteral, Text: "0.25"},
				"created_at": {Mode: dto.CellInputLiteral, Text: "2026-11-28T08:30:00Z"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("preview update NULL+REAL+DATETIME failed: %v", err)
	}
	resUpd, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevUpd.PlanID})
	if err != nil || resUpd.Outcome != dto.GridApplyCommitted || resUpd.AppliedCount != 1 {
		t.Fatalf("apply update failed: resp=%+v err=%v", resUpd, err)
	}
	var savedNote, savedDT string
	if err := st.rawDB.QueryRow(`SELECT note, created_at FROM items WHERE id = 1`).Scan(&savedNote, &savedDT); err != nil {
		t.Fatalf("query updated note/created_at: %v", err)
	}
	if savedNote != "1234567890123456789012345678.1234567890" || savedDT != "2026-11-28T08:30:00Z" {
		t.Fatalf("unexpected saved values: note=%q created_at=%q", savedNote, savedDT)
	}

	// 5. COLLATE NOCASE concurrent conflict ('Alice' -> 'alice' by another session MUST conflict!).
	prevCase, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "items",
		Updates: []dto.GridRowUpdate{{
			RowKey:         "case-row",
			KeyValues:      key1,
			OriginalValues: map[string]dto.OriginalCellValue{"name": {Kind: dto.KindString, Text: "Alice", StorageClass: "text"}},
			NewValues:      map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "Alicia"}},
		}},
	})
	if err != nil {
		t.Fatalf("preview case update: %v", err)
	}
	// Concurrent session changes 'Alice' -> 'alice'.
	if _, err := st.rawDB.Exec(`UPDATE items SET name = 'alice' WHERE id = 1`); err != nil {
		t.Fatalf("concurrent nocase update: %v", err)
	}
	respCase, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevCase.PlanID})
	assertApplyOutcome(t, respCase, err, dto.GridApplyNotCommitted, dto.ErrConcurrencyConflict)
	if respCase.ConflictRowKey != "case-row" {
		t.Fatalf("expected ConflictRowKey=case-row, got %q", respCase.ConflictRowKey)
	}

	// 6. typeof discrimination: dyn was INTEGER 1; concurrent session changes it to TEXT '1' -> MUST conflict!
	prevType, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "items",
		Updates: []dto.GridRowUpdate{{
			RowKey:         "type-row",
			KeyValues:      key1,
			OriginalValues: map[string]dto.OriginalCellValue{"dyn": {Kind: dto.KindInt, Text: "1", StorageClass: "integer"}},
			NewValues:      map[string]dto.NewCellValue{"dyn": {Mode: dto.CellInputLiteral, Text: "2"}},
		}},
	})
	if err != nil {
		t.Fatalf("preview typeof update: %v", err)
	}
	if _, err := st.rawDB.Exec(`UPDATE items SET dyn = '1' WHERE id = 1`); err != nil {
		t.Fatalf("concurrent typeof update: %v", err)
	}
	var actualDynType string
	if err := st.rawDB.QueryRow(`SELECT typeof(dyn) FROM items WHERE id = 1`).Scan(&actualDynType); err != nil {
		t.Fatalf("query typeof(dyn): %v", err)
	}
	if actualDynType != "text" {
		t.Fatalf("expected concurrent session to store dyn as text, got %q", actualDynType)
	}
	respType, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevType.PlanID})
	assertApplyOutcome(t, respType, err, dto.GridApplyNotCommitted, dto.ErrConcurrencyConflict)

	// 7. SQLite NUMERIC precision vs non-numeric text:
	//    - DECIMAL(38,10) rejects precision-losing numeric literals (incl. 1e23!) with ErrPrecisionUnsupported.
	//    - DECIMAL(38,10) / DATETIME accepts non-numeric JSON '{"a":1}' & ISO datetime verbatim as TEXT.
	for _, badNum := range []string{"1e23", "1234567890123456789012345678.1234567890"} {
		_, err = st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
			ConnectionID: st.connID, Database: "main", Table: "items",
			Inserts: []dto.GridRowInsert{{
				RowKey: "prec-bad",
				Values: map[string]dto.NewCellValue{
					"amt": {Mode: dto.CellInputLiteral, Text: badNum},
				},
			}},
		})
		assertErrCode(t, err, dto.ErrPrecisionUnsupported)
	}

	prevNonNum, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "items",
		Inserts: []dto.GridRowInsert{{
			RowKey: "nonnum-ok",
			Values: map[string]dto.NewCellValue{
				"name":       {Mode: dto.CellInputLiteral, Text: "Bob"},
				"created_at": {Mode: dto.CellInputLiteral, Text: "2026-11-27T12:00:00Z"},
				"amt":        {Mode: dto.CellInputLiteral, Text: `{"a":1}`},
			},
		}},
	})
	if err != nil {
		t.Fatalf("preview non-numeric text on NUMERIC cols failed: %v", err)
	}
	resNonNum, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevNonNum.PlanID})
	if err != nil || resNonNum.Outcome != dto.GridApplyCommitted {
		t.Fatalf("apply non-numeric text failed: resp=%+v err=%v", resNonNum, err)
	}
	var typeofAmt, valAmt string
	if err := st.rawDB.QueryRow(`SELECT typeof(amt), amt FROM items WHERE name = 'Bob'`).Scan(&typeofAmt, &valAmt); err != nil {
		t.Fatalf("query typeof(amt): %v", err)
	}
	if typeofAmt != "text" || valAmt != `{"a":1}` {
		t.Fatalf("expected NUMERIC non-numeric JSON stored verbatim as text, got typeof=%q val=%q", typeofAmt, valAmt)
	}
}

func TestSQLiteReal354StrictAnyAndMissingFileFinalization(t *testing.T) {
	st := newSQLiteStack(t, []string{
		`CREATE TABLE t_strict_any (id INTEGER PRIMARY KEY, payload ANY) STRICT`,
		`CREATE TABLE t_regular_any (id INTEGER PRIMARY KEY, payload ANY)`,
	})
	ctx := context.Background()

	// 1. STRICT table ANY column MUST preserve leading zeros ("000123") as TEXT!
	prevStrict, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_strict_any",
		Inserts: []dto.GridRowInsert{{
			RowKey: "strict-any-1",
			Values: map[string]dto.NewCellValue{
				"id":      {Mode: dto.CellInputLiteral, Text: "1"},
				"payload": {Mode: dto.CellInputLiteral, Text: "000123"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("preview strict ANY: %v", err)
	}
	resStrict, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevStrict.PlanID})
	if err != nil || resStrict.Outcome != dto.GridApplyCommitted {
		t.Fatalf("apply strict ANY failed: resp=%+v err=%v", resStrict, err)
	}
	var strictType, strictVal string
	if err := st.rawDB.QueryRow(`SELECT typeof(payload), payload FROM t_strict_any WHERE id = 1`).Scan(&strictType, &strictVal); err != nil {
		t.Fatalf("query t_strict_any: %v", err)
	}
	if strictType != "text" || strictVal != "000123" {
		t.Fatalf("STRICT ANY must preserve '000123' as text, got typeof=%q val=%q", strictType, strictVal)
	}

	// 2. Non-STRICT regular table ANY column keeps standard NUMERIC affinity ("000123" -> integer 123).
	prevReg, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_regular_any",
		Inserts: []dto.GridRowInsert{{
			RowKey: "reg-any-1",
			Values: map[string]dto.NewCellValue{
				"id":      {Mode: dto.CellInputLiteral, Text: "1"},
				"payload": {Mode: dto.CellInputLiteral, Text: "000123"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("preview regular ANY: %v", err)
	}
	resReg, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevReg.PlanID})
	if err != nil || resReg.Outcome != dto.GridApplyCommitted {
		t.Fatalf("apply regular ANY failed: resp=%+v err=%v", resReg, err)
	}
	var regType, regVal string
	if err := st.rawDB.QueryRow(`SELECT typeof(payload), CAST(payload AS TEXT) FROM t_regular_any WHERE id = 1`).Scan(&regType, &regVal); err != nil {
		t.Fatalf("query t_regular_any: %v", err)
	}
	if regType != "integer" || regVal != "123" {
		t.Fatalf("regular non-STRICT ANY should coerce to integer 123, got typeof=%q val=%q", regType, regVal)
	}

	// 3. Deleting SQLite file after Preview MUST finalize plan as Finished(NotCommitted), NOT release back to Ready!
	prevMissing, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_strict_any",
		Inserts: []dto.GridRowInsert{{
			RowKey: "missing-1",
			Values: map[string]dto.NewCellValue{"payload": {Mode: dto.CellInputLiteral, Text: "gone"}},
		}},
	})
	if err != nil {
		t.Fatalf("preview before delete: %v", err)
	}
	_ = st.rawDB.Close()
	if err := os.Remove(st.dbPath); err != nil {
		t.Fatalf("remove sqlite db file: %v", err)
	}

	respMissing, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevMissing.PlanID})
	assertApplyOutcome(t, respMissing, err, dto.GridApplyNotCommitted, dto.ErrConnectionFailed)

	// Re-applying MUST report ErrPlanInvalidated (plan finalized, NOT returned to Ready!).
	state, hasPayload, ok := st.changeSvc.PlanStore().StateOf(prevMissing.PlanID)
	if !ok || state != changeset.PlanFinished || hasPayload {
		t.Fatalf("expected deleted-file Apply to finalize to Finished (not Ready), got state=%s payload=%v ok=%v", state, hasPayload, ok)
	}
	respRepeat, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevMissing.PlanID})
	assertApplyOutcome(t, respRepeat, err, dto.GridApplyNotCommitted, dto.ErrPlanInvalidated)
}

func TestSQLiteReal354IntegerRealNoopAndRollbackContext(t *testing.T) {
	st := newSQLiteStack(t, []string{
		`CREATE TABLE t_int_real (id INTEGER PRIMARY KEY, score INTEGER, label TEXT)`,
		`INSERT INTO t_int_real (id, score, label) VALUES (1, 0.1, 'before')`,
	})
	ctx := context.Background()

	// 1. BrowseTable reads score as REAL 0.1 on the non-STRICT INTEGER column.
	browse, err := st.querySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_int_real", Page: 1, PageSize: 10,
	})
	if err != nil || len(browse.Rows) != 1 {
		t.Fatalf("browse t_int_real failed: err=%v rows=%d", err, len(browse.Rows))
	}
	idxOf := map[string]int{}
	for i, c := range browse.Columns {
		idxOf[c.Name] = i
	}
	row := browse.Rows[0]
	if row[idxOf["score"]].StorageClass != "real" || row[idxOf["score"]].Text != "0.1" {
		t.Fatalf("expected existing REAL 0.1 in non-STRICT INTEGER column, got %+v", row[idxOf["score"]])
	}

	key1 := map[string]dto.OriginalCellValue{"id": cellToOrig(row[idxOf["id"]])}

	// 2. Restoring score="0.1" alongside editing label="after" MUST succeed and strip score from UPDATE!
	prev, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_int_real",
		Updates: []dto.GridRowUpdate{{
			RowKey:    "r-int-real",
			KeyValues: key1,
			OriginalValues: map[string]dto.OriginalCellValue{
				"score": cellToOrig(row[idxOf["score"]]),
				"label": cellToOrig(row[idxOf["label"]]),
			},
			NewValues: map[string]dto.NewCellValue{
				"score": {Mode: dto.CellInputLiteral, Text: "0.1"},
				"label": {Mode: dto.CellInputLiteral, Text: "after"},
			},
		}},
	})
	if err != nil || len(prev.Steps) != 1 {
		t.Fatalf("preview with restored REAL 0.1 on INTEGER column failed: len=%d err=%v", len(prev.Steps), err)
	}
	res, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prev.PlanID})
	if err != nil || res.Outcome != dto.GridApplyCommitted || res.AppliedCount != 1 {
		t.Fatalf("apply failed: resp=%+v err=%v", res, err)
	}
	var scoreType, scoreText, labelText string
	if err := st.rawDB.QueryRow(`SELECT typeof(score), CAST(score AS TEXT), label FROM t_int_real WHERE id = 1`).Scan(&scoreType, &scoreText, &labelText); err != nil {
		t.Fatalf("query t_int_real: %v", err)
	}
	if scoreType != "real" || scoreText != "0.1" || labelText != "after" {
		t.Fatalf("expected score preserved as real 0.1 and label=after, got type=%q score=%q label=%q", scoreType, scoreText, labelText)
	}

	// 3. SQLite TableWriteTx.Rollback(canceledCtx) MUST forward ctx AND leave tx retryable so
	//    a follow-up Rollback(validCtx) actually releases the BEGIN IMMEDIATE write lock while lease is open!
	lease, err := st.sessMgr.AcquireOperation(ctx, st.connID, "main", nil)
	if err != nil {
		t.Fatalf("acquire sqlite op: %v", err)
	}
	defer lease.Release()
	tx, err := lease.Conn().(driver.TableWriteStarter).BeginTableWrite(ctx, driver.WriteTarget{Database: "main", Table: "t_int_real"}, false)
	if err != nil {
		t.Fatalf("BeginTableWrite: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := tx.Rollback(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected SQLite Rollback(canceledCtx) to return context.Canceled, got %v", err)
	}
	// Retrying Rollback with a valid context MUST execute ROLLBACK (not silently return nil due to premature done=true).
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("retry Rollback(ctx) failed: %v", err)
	}
	// Prove BEGIN IMMEDIATE lock is truly released while lease is STILL open: another connection writes cleanly!
	if _, err := st.rawDB.Exec(`INSERT INTO t_int_real (id, score, label) VALUES (2, 10, 'lock_released')`); err != nil {
		t.Fatalf("expected BEGIN IMMEDIATE lock released after retry Rollback(ctx), got: %v", err)
	}

	// 4. SQLite Commit(alreadyCanceledCtx) MUST roll back with bounded context and release the lock, returning NotCommitted.
	tx2, err := lease.Conn().(driver.TableWriteStarter).BeginTableWrite(ctx, driver.WriteTarget{Database: "main", Table: "t_int_real"}, false)
	if err != nil {
		t.Fatalf("BeginTableWrite #2: %v", err)
	}
	if _, err := tx2.Exec(ctx, driver.BoundStatement{SQL: `INSERT INTO t_int_real (id, score, label) VALUES (3, 99, 'must_rollback')`}); err != nil {
		t.Fatalf("Exec inside tx2: %v", err)
	}
	commitRes := tx2.Commit(canceledCtx)
	if commitRes.Outcome != driver.CommitOutcomeNotCommitted {
		t.Fatalf("expected pre-canceled SQLite Commit to return NotCommitted, got %+v", commitRes)
	}
	// Prove lock released AND row 3 was rolled back while lease is STILL open!
	if _, err := st.rawDB.Exec(`INSERT INTO t_int_real (id, score, label) VALUES (4, 20, 'after_canceled_commit')`); err != nil {
		t.Fatalf("expected BEGIN IMMEDIATE lock released after pre-canceled Commit, got: %v", err)
	}
	var cnt3 int
	if err := st.rawDB.QueryRow(`SELECT COUNT(*) FROM t_int_real WHERE id = 3`).Scan(&cnt3); err != nil {
		t.Fatalf("query id=3 count: %v", err)
	}
	if cnt3 != 0 {
		t.Fatalf("expected id=3 rolled back on pre-canceled Commit, got %d rows", cnt3)
	}
}

func TestSQLiteReal354WideTableOver1000ColsPreservesStorageClassAndEditing(t *testing.T) {
	// Build a 1005-column table (> 1000 cols, so paired val+typeof projection would produce 2010 cols
	// and breach SQLite's default SQLITE_MAX_COLUMN=2000 limit).
	const extraCols = 1000
	var colDefs []string
	colDefs = append(colDefs,
		"id INTEGER PRIMARY KEY",
		"created_at DATETIME",
		"score REAL",
		"raw_bin BLOB",
		"note TEXT",
	)
	for i := 1; i <= extraCols; i++ {
		colDefs = append(colDefs, fmt.Sprintf("c%04d TEXT", i))
	}
	createSQL := fmt.Sprintf("CREATE TABLE t_wide (%s)", strings.Join(colDefs, ", "))
	insertSQL := `INSERT INTO t_wide (id, created_at, score, raw_bin, note, c1000)
		VALUES (1, '2026-11-27 12:00:00', 0.5, X'DEADBEEF', NULL, 'tail_orig')`

	st := newSQLiteStack(t, []string{createSQL, insertSQL})
	ctx := context.Background()

	browse, err := st.querySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_wide", Page: 1, PageSize: 10,
	})
	if err != nil {
		t.Fatalf("BrowseTable on 1005-column SQLite wide table failed: %v", err)
	}
	if len(browse.Columns) != 1005 || len(browse.Rows) != 1 {
		t.Fatalf("expected 1005 columns and 1 row, got cols=%d rows=%d", len(browse.Columns), len(browse.Rows))
	}

	idxOf := make(map[string]int, len(browse.Columns))
	for i, c := range browse.Columns {
		idxOf[c.Name] = i
	}
	row := browse.Rows[0]

	// Verify exact StorageClass inferred from scanned driver values AND verbatim non-RFC3339 DATETIME spelling!
	if row[idxOf["id"]].StorageClass != "integer" || row[idxOf["id"]].Text != "1" {
		t.Fatalf("wide table id mismatch: %+v", row[idxOf["id"]])
	}
	if row[idxOf["created_at"]].StorageClass != "text" || row[idxOf["created_at"]].Text != "2026-11-27 12:00:00" {
		t.Fatalf("wide table DATETIME must keep verbatim text and StorageClass=text, got %+v", row[idxOf["created_at"]])
	}
	if row[idxOf["score"]].StorageClass != "real" || row[idxOf["score"]].Text != "0.5" {
		t.Fatalf("wide table score mismatch: %+v", row[idxOf["score"]])
	}
	if row[idxOf["raw_bin"]].StorageClass != "blob" || row[idxOf["raw_bin"]].Kind != dto.KindBinary {
		t.Fatalf("wide table raw_bin mismatch: %+v", row[idxOf["raw_bin"]])
	}
	if row[idxOf["note"]].StorageClass != "null" || row[idxOf["note"]].Kind != dto.KindNull {
		t.Fatalf("wide table note mismatch: %+v", row[idxOf["note"]])
	}
	if row[idxOf["c1000"]].StorageClass != "text" || row[idxOf["c1000"]].Text != "tail_orig" {
		t.Fatalf("wide table c1000 mismatch: %+v", row[idxOf["c1000"]])
	}

	// Verify end-to-end grid editing works on the wide table using the inferred StorageClass!
	prev, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_wide",
		Updates: []dto.GridRowUpdate{{
			RowKey:    "wide-row-1",
			KeyValues: map[string]dto.OriginalCellValue{"id": cellToOrig(row[idxOf["id"]])},
			OriginalValues: map[string]dto.OriginalCellValue{
				"created_at": cellToOrig(row[idxOf["created_at"]]),
				"c1000":      cellToOrig(row[idxOf["c1000"]]),
			},
			NewValues: map[string]dto.NewCellValue{
				"created_at": {Mode: dto.CellInputLiteral, Text: "2026-11-28 09:30:00"},
				"c1000":      {Mode: dto.CellInputLiteral, Text: "tail_updated"},
			},
		}},
	})
	if err != nil || len(prev.Steps) != 1 {
		t.Fatalf("preview wide table update failed: len=%d err=%v", len(prev.Steps), err)
	}
	res, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prev.PlanID})
	if err != nil || res.Outcome != dto.GridApplyCommitted || res.AppliedCount != 1 {
		t.Fatalf("apply wide table update failed: resp=%+v err=%v", res, err)
	}

	var gotDT, gotTail string
	if err := st.rawDB.QueryRow(`SELECT CAST(created_at AS TEXT), c1000 FROM t_wide WHERE id = 1`).Scan(&gotDT, &gotTail); err != nil {
		t.Fatalf("query updated wide row: %v", err)
	}
	if gotDT != "2026-11-28 09:30:00" || gotTail != "tail_updated" {
		t.Fatalf("unexpected wide row persisted values: dt=%q tail=%q", gotDT, gotTail)
	}
}

func TestSQLiteReal354FormFeedVerticalTabAndGeneratedExprFingerprint(t *testing.T) {
	st := newSQLiteStack(t, []string{
		`CREATE TABLE t_ws_num (id INTEGER PRIMARY KEY, num_col NUMERIC)`,
		`CREATE TABLE t_gen_expr (
			id INTEGER PRIMARY KEY,
			qty INTEGER NOT NULL,
			g INTEGER GENERATED ALWAYS AS (qty * 2) VIRTUAL
		)`,
	})
	ctx := context.Background()

	// 1. SQLite treats \f and \v as numeric whitespace; rounded literals prefixed with \f or \v MUST be rejected.
	for _, bad := range []string{"\f0.1", "\v0.1", "0.1\f", "\v1e23"} {
		_, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
			ConnectionID: st.connID, Database: "main", Table: "t_ws_num",
			Inserts: []dto.GridRowInsert{{
				RowKey: "bad-ws",
				Values: map[string]dto.NewCellValue{
					"id":      {Mode: dto.CellInputLiteral, Text: "1"},
					"num_col": {Mode: dto.CellInputLiteral, Text: bad},
				},
			}},
		})
		assertErrCode(t, err, dto.ErrPrecisionUnsupported)
	}

	// Lossless \f / \v numeric literals bind and store cleanly as integer / real.
	prevWS, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_ws_num",
		Inserts: []dto.GridRowInsert{
			{
				RowKey: "good-int",
				Values: map[string]dto.NewCellValue{
					"id":      {Mode: dto.CellInputLiteral, Text: "1"},
					"num_col": {Mode: dto.CellInputLiteral, Text: "\f42\v"},
				},
			},
			{
				RowKey: "good-real",
				Values: map[string]dto.NewCellValue{
					"id":      {Mode: dto.CellInputLiteral, Text: "2"},
					"num_col": {Mode: dto.CellInputLiteral, Text: "\v0.5\f"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("preview lossless \\f/\\v values: %v", err)
	}
	resWS, err := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevWS.PlanID})
	if err != nil || resWS.Outcome != dto.GridApplyCommitted || resWS.AppliedCount != 2 {
		t.Fatalf("apply lossless \\f/\\v values failed: resp=%+v err=%v", resWS, err)
	}
	var t1, t2 string
	if err := st.rawDB.QueryRow(`SELECT typeof(num_col) FROM t_ws_num WHERE id = 1`).Scan(&t1); err != nil {
		t.Fatalf("query typeof id=1: %v", err)
	}
	if err := st.rawDB.QueryRow(`SELECT typeof(num_col) FROM t_ws_num WHERE id = 2`).Scan(&t2); err != nil {
		t.Fatalf("query typeof id=2: %v", err)
	}
	if t1 != "integer" || t2 != "real" {
		t.Fatalf("expected storage classes integer/real, got %s/%s", t1, t2)
	}

	// 2. Replacing virtual generated column expression g AS (qty * 2) -> g AS (qty * 3) between Preview and Apply
	//    MUST fail SchemaFingerprint verification with ErrSchemaChanged and leave 0 rows inserted!
	prevGen, err := st.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: st.connID, Database: "main", Table: "t_gen_expr",
		Inserts: []dto.GridRowInsert{{
			RowKey: "gen-ins",
			Values: map[string]dto.NewCellValue{
				"id":  {Mode: dto.CellInputLiteral, Text: "1"},
				"qty": {Mode: dto.CellInputLiteral, Text: "10"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("preview t_gen_expr: %v", err)
	}
	if _, err := st.rawDB.Exec(`ALTER TABLE t_gen_expr DROP COLUMN g`); err != nil {
		t.Fatalf("drop generated col g: %v", err)
	}
	if _, err := st.rawDB.Exec(`ALTER TABLE t_gen_expr ADD COLUMN g INTEGER GENERATED ALWAYS AS (qty * 3) VIRTUAL`); err != nil {
		t.Fatalf("re-add generated col g with new expression: %v", err)
	}

	respGen, errGen := st.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: st.connID, PlanID: prevGen.PlanID})
	assertApplyOutcome(t, respGen, errGen, dto.GridApplyNotCommitted, dto.ErrSchemaChanged)
	var genCnt int
	if err := st.rawDB.QueryRow(`SELECT COUNT(*) FROM t_gen_expr`).Scan(&genCnt); err != nil {
		t.Fatalf("count t_gen_expr: %v", err)
	}
	if genCnt != 0 {
		t.Fatalf("expected 0 rows inserted after generated expression drift, got %d", genCnt)
	}
}
