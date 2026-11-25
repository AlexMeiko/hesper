package mysql_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sql-manager/internal/driver"
	"sql-manager/internal/driver/mysql"
	"sql-manager/internal/dto"
	"sql-manager/internal/repository"
	"sql-manager/internal/resultbuf"
	"sql-manager/internal/secret"
	"sql-manager/internal/service"
	"sql-manager/internal/session"
)

func mysqlHostPort() (string, int) {
	host := os.Getenv("SQLM_TEST_MYSQL_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 13306
	if p := os.Getenv("SQLM_TEST_MYSQL_PORT"); p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}
	return host, port
}

func requireRealMySQL(t *testing.T) (string, int) {
	t.Helper()
	host, port := mysqlHostPort()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		t.Skipf("real MySQL 8.4 not reachable at %s:%d (start with: docker compose -f docker-compose.test.yml up -d --wait): %v", host, port, err)
	}
	_ = c.Close()
	return host, port
}

type realTestHarness struct {
	repo    *repository.LocalDB
	secrets *secret.Store
	reg     *driver.Registry
	resMgr  *resultbuf.Manager
	sessMgr *session.Manager
	connSvc *service.ConnectionService
	schSvc  *service.SchemaService
	qrySvc  *service.QueryService
	now     time.Time
	nowMu   sync.Mutex
}

func newRealMySQLHarness(t *testing.T, customDial func(ctx context.Context, network, addr string) (net.Conn, error)) *realTestHarness {
	t.Helper()
	dir := t.TempDir()
	repo, err := repository.OpenLocalDB(dir + "/app.db")
	if err != nil {
		t.Fatalf("open local app.db: %v", err)
	}
	sec := secret.NewFileOnlyStore(dir)
	reg := driver.NewRegistry()
	reg.Register(mysql.NewOpener(dto.EngineMySQL))

	h := &realTestHarness{
		repo:    repo,
		secrets: sec,
		reg:     reg,
		now:     time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
	}
	h.resMgr = resultbuf.NewManager(resultbuf.Options{
		SpillDir:      dir + "/results",
		MaxMemRows:    15, // low threshold to exercise transparent spill on real MySQL data too
		MaxTotalRows:  5000,
		MaxTotalBytes: 16 * 1024 * 1024,
		IdleTTL:       10 * time.Minute,
	})
	h.resMgr.SetClock(func() time.Time {
		h.nowMu.Lock()
		defer h.nowMu.Unlock()
		return h.now
	})

	h.connSvc = service.NewConnectionService(repo, sec, nil, h.resMgr)
	h.sessMgr = session.NewManager(reg, func(ctx context.Context, connID, overrideDB string) (driver.ResolvedConfig, error) {
		cfg, err := h.connSvc.ResolveConfig(ctx, connID, overrideDB)
		if err != nil {
			return cfg, err
		}
		if customDial != nil {
			cfg.DialContext = customDial
		}
		return cfg, nil
	})
	h.connSvc.SetSessionManager(h.sessMgr)
	h.schSvc = service.NewSchemaService(h.sessMgr)
	h.qrySvc = service.NewQueryService(h.sessMgr, h.resMgr, repo)

	t.Cleanup(func() {
		h.sessMgr.CloseAll()
		h.resMgr.CloseAll()
		_ = h.repo.Close()
	})
	return h
}

func (h *realTestHarness) saveMySQLConn(t *testing.T, name, user, pw, db string, clientRO bool) dto.ConnectionDTO {
	t.Helper()
	host, port := mysqlHostPort()
	c, err := h.connSvc.SaveConnection(context.Background(), dto.SaveConnectionRequest{
		Name:           name,
		Driver:         dto.EngineMySQL,
		EnvColor:       "green",
		ReadOnly:       clientRO,
		Password:       pw,
		UpdatePassword: true,
		Params: dto.ConnectionParams{
			Host:            host,
			Port:            port,
			Username:        user,
			DefaultDatabase: db,
			TLS:             dto.TLSConfig{Mode: "disable"},
		},
	})
	if err != nil {
		t.Fatalf("save connection %s: %v", name, err)
	}
	return c
}

// Acceptance Criterion #1 (Real MySQL 8.4):
// 1) Bypass KILL QUERY cancels running SLEEP query A while preserving physical connection, open tx, and temp table.
// 2) Delayed cancel race: Cancel(A) approved right as A finishes naturally while B is queued -> B is NEVER killed by A's cancel!
func TestMySQL84_Criterion1_CancelAndLateCancelRace(t *testing.T) {
	requireRealMySQL(t)
	h := newRealMySQLHarness(t, nil)
	ctx := context.Background()

	conn := h.saveMySQLConn(t, "mysql84-rw", "sqlm_rw", "sqlm_rw_pw", "sqlm_demo", false)
	connectRes, err := h.connSvc.Connect(ctx, conn.ID)
	if err != nil {
		t.Fatalf("connect real MySQL 8.4: %v", err)
	}
	t.Logf("Connected real database: type=%s version=%s", connectRes.DetectedType, connectRes.ServerVersion)
	if !strings.HasPrefix(connectRes.ServerVersion, "8.4.") {
		t.Fatalf("expected MySQL 8.4.x, got %s", connectRes.ServerVersion)
	}

	tabID := "tab-cancel-1"
	info1, err := h.qrySvc.OpenTabSession(ctx, tabID, conn.ID, "sqlm_demo", "")
	if err != nil {
		t.Fatalf("open tab session: %v", err)
	}
	origServerConnID := info1.ServerConnID

	// Create a session-scoped temp table and begin a transaction to prove KILL QUERY keeps the session alive!
	setupResp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "setup-1", ConnectionID: conn.ID, Database: "sqlm_demo",
		SQL: "CREATE TEMPORARY TABLE tmp_alive_check (v VARCHAR(32)); INSERT INTO tmp_alive_check VALUES ('still_here'); BEGIN;",
		RunMode: "all",
	})
	if err != nil || setupResp.Error != nil {
		t.Fatalf("setup temp table & tx failed: err=%v respErr=%+v", err, setupResp.Error)
	}
	if setupResp.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected AliveInTx after BEGIN, got %s", setupResp.SessionState)
	}

	// Start a genuinely interruptible slow query A on real MySQL 8.4.
	// Note: MySQL's standalone `SELECT SLEEP(15)` catches KILL QUERY internally and returns 1 without error 1317,
	// whereas a real slow query over tables raises MySQL Error 1317 (Query execution was interrupted).
	var respA *dto.ExecuteSQLResponse
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		respA, _ = h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID: tabID, ExecID: "exec-slow-A", ConnectionID: conn.ID, Database: "sqlm_demo",
			SQL: "SELECT COUNT(*) AS cnt FROM events a JOIN events b ON a.id <= 1250 AND b.id <= 1250 WHERE SHA2(CONCAT(a.id, '-', b.id, '-', COALESCE(a.message, '')), 512) = 'never_matches'",
			RunMode: "all",
		})
	}()

	time.Sleep(250 * time.Millisecond)
	cancelStart := time.Now()
	cancelResp := h.qrySvc.CancelExecution(ctx, tabID, "exec-slow-A")
	wg.Wait()
	cancelElapsed := time.Since(cancelStart)

	if cancelResp.Status != "canceled" {
		t.Fatalf("expected cancel status=canceled, got %+v", cancelResp)
	}
	if respA == nil || respA.Error == nil || respA.Error.Code != dto.ErrStatementCanceled {
		t.Fatalf("expected query A interrupted with ERR_STATEMENT_CANCELED, got %+v", respA)
	}
	if cancelElapsed > 3*time.Second {
		t.Fatalf("cancel took too long: %v", cancelElapsed)
	}

	// Verify physical connection, CONNECTION_ID(), open transaction, AND temporary table survived!
	checkResp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "verify-alive", ConnectionID: conn.ID, Database: "sqlm_demo",
		SQL: "SELECT CONNECTION_ID() AS cid, v FROM tmp_alive_check; ROLLBACK;", RunMode: "all",
	})
	if err != nil || checkResp.Error != nil {
		t.Fatalf("session did not survive KILL QUERY: err=%v resp=%+v", err, checkResp)
	}
	if checkResp.Statements[0].Rows[0][0].Text != origServerConnID {
		t.Fatalf("expected same MySQL CONNECTION_ID=%s, got %s", origServerConnID, checkResp.Statements[0].Rows[0][0].Text)
	}
	if checkResp.Statements[0].Rows[0][1].Text != "still_here" {
		t.Fatalf("temporary table lost after KILL QUERY: %+v", checkResp.Statements[0].Rows)
	}

	// --- Late Cancel Race: A finishes naturally right around Cancel(A) while B is queued ---
	// Repeat multiple rounds to verify determinism under race timing.
	for round := 0; round < 5; round++ {
		execA := fmt.Sprintf("race-A-%d", round)
		execB := fmt.Sprintf("race-B-%d", round)

		var respB *dto.ExecuteSQLResponse
		var raceWG sync.WaitGroup
		raceWG.Add(3)

		go func() {
			defer raceWG.Done()
			_, _ = h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				TabID: tabID, ExecID: execA, ConnectionID: conn.ID, Database: "sqlm_demo",
				SQL: "SELECT SLEEP(0.08) AS a_done", RunMode: "all",
			})
		}()
		go func() {
			defer raceWG.Done()
			// Queue B right behind A
			time.Sleep(30 * time.Millisecond)
			respB, _ = h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				TabID: tabID, ExecID: execB, ConnectionID: conn.ID, Database: "sqlm_demo",
				SQL: "SELECT SLEEP(0.15), 'B_INTACT' AS marker", RunMode: "all",
			})
		}()
		go func() {
			defer raceWG.Done()
			// Fire cancel for A right near A's natural completion window
			time.Sleep(75 * time.Millisecond)
			_ = h.qrySvc.CancelExecution(ctx, tabID, execA)
		}()
		raceWG.Wait()

		if respB == nil || respB.Error != nil {
			t.Fatalf("round %d: queued query B MUST NOT be killed by late cancel of A! got respB=%+v", round, respB)
		}
		if len(respB.Statements) != 1 || respB.Statements[0].Rows[0][1].Text != "B_INTACT" {
			t.Fatalf("round %d: unexpected result for B: %+v", round, respB)
		}
	}
}

// Acceptance Criterion #2 (Real MySQL 8.4):
// 1) Editor Execute-Once pagination: side-effecting CALL sp_side_effect_rows paged across pages 1..4 executes ONLY ONCE.
// 2) Snapshot expiration returns ERR_RESULT_EXPIRED and does NOT re-run SQL.
// 3) Double-click live BrowseTable is completely separate from editor snapshots.
// 4) CloseTab releases both MySQL physical connection and snapshot cache.
func TestMySQL84_Criterion2_ExecuteOncePaginationLiveBrowseAndCloseCleanup(t *testing.T) {
	requireRealMySQL(t)
	h := newRealMySQLHarness(t, nil)
	ctx := context.Background()

	conn := h.saveMySQLConn(t, "mysql84-write", "sqlm_rw", "sqlm_rw_pw", "sqlm_write_test", false)
	tabID := "tab-paging-1"
	runKey := fmt.Sprintf("exec-once-%d", time.Now().UnixNano())

	// Execute stored procedure that inserts 1 audit row into side_effect_log AND returns 35 rows (exceeding MaxMemRows=15 -> spills!)
	execResp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "p-1", ConnectionID: conn.ID, Database: "sqlm_write_test",
		SQL: fmt.Sprintf("CALL sp_side_effect_rows('%s', 35)", runKey), RunMode: "all", PageSize: 10,
	})
	if err != nil || execResp.Error != nil {
		t.Fatalf("execute side-effect procedure failed: err=%v resp=%+v", err, execResp)
	}
	st0 := execResp.Statements[0]
	if !st0.IsResultSet || st0.TotalBuffered != 35 || len(st0.Rows) != 10 || !st0.HasMorePages {
		t.Fatalf("unexpected page 1 metadata: %+v", st0)
	}

	// Fetch Page 2, Page 3, Page 4 from snapshot
	p2, err := h.qrySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: st0.ResultID, Page: 2, PageSize: 10})
	if err != nil || len(p2.Rows) != 10 || p2.Rows[0][0].Text != "11" {
		t.Fatalf("unexpected page 2: err=%v p2=%+v", err, p2)
	}
	p4, err := h.qrySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: st0.ResultID, Page: 4, PageSize: 10})
	if err != nil || len(p4.Rows) != 5 || p4.Rows[4][0].Text != "35" || p4.HasMorePages {
		t.Fatalf("unexpected page 4: err=%v p4=%+v", err, p4)
	}

	// Verify side_effect_log in real MySQL 8.4 has EXACTLY 1 row for runKey!
	verifyResp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-verify", ExecID: "v-1", ConnectionID: conn.ID, Database: "sqlm_write_test",
		SQL: fmt.Sprintf("SELECT COUNT(*) FROM side_effect_log WHERE run_key = '%s'", runKey), RunMode: "all",
	})
	if err != nil || verifyResp.Error != nil {
		t.Fatalf("verify side_effect_log failed: %v %+v", err, verifyResp)
	}
	if verifyResp.Statements[0].Rows[0][0].Text != "1" {
		t.Fatalf("SQL was re-executed during pagination! side_effect_log count=%s, want 1", verifyResp.Statements[0].Rows[0][0].Text)
	}
	_ = h.qrySvc.CloseTab("tab-verify")

	// Expire snapshot -> FetchResultPage MUST return ERR_RESULT_EXPIRED and NEVER re-run SQL
	h.nowMu.Lock()
	h.now = h.now.Add(15 * time.Minute)
	h.nowMu.Unlock()

	_, expErr := h.qrySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: st0.ResultID, Page: 2, PageSize: 10})
	if expErr == nil {
		t.Fatal("expected ERR_RESULT_EXPIRED after TTL")
	}
	if ae, ok := expErr.(*dto.AppError); !ok || ae.Code != dto.ErrResultExpired {
		t.Fatalf("expected ERR_RESULT_EXPIRED, got %+v", expErr)
	}

	// Live BrowseTable on sqlm_demo.events (1,250 rows) — completely separate path
	b1, err := h.qrySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Table: "events", Page: 1, PageSize: 50,
	})
	if err != nil || len(b1.Rows) != 50 || !b1.HasMore || !b1.StableOrderUsed || b1.TotalRows != 1250 {
		t.Fatalf("unexpected BrowseTable page 1: err=%v resp=%+v", err, b1)
	}
	b2, err := h.qrySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Table: "events", Page: 2, PageSize: 50,
	})
	if err != nil || len(b2.Rows) != 50 || b2.Rows[0][0].Text != "51" {
		t.Fatalf("unexpected BrowseTable page 2: err=%v firstRow=%+v", err, b2.Rows[0])
	}

	// Verify TableEditability assessment for MyISAM vs InnoDB no-PK
	detailMyISAM, err := h.schSvc.GetTableDetail(ctx, conn.ID, "sqlm_demo", "", "legacy_logs")
	if err != nil || detailMyISAM.Editability.BatchTransactional {
		t.Fatalf("MyISAM table must have BatchTransactional=false, got err=%v edit=%+v", err, detailMyISAM.Editability)
	}
	detailNoPK, err := h.schSvc.GetTableDetail(ctx, conn.ID, "sqlm_demo", "", "no_pk_notes")
	if err != nil || detailNoPK.Editability.RowIdentity != dto.RowIdentityNone {
		t.Fatalf("no_pk_notes must have RowIdentity=none, got err=%v edit=%+v", err, detailNoPK.Editability)
	}

	// CloseTab must release BOTH physical connection and snapshots!
	if err := h.qrySvc.CloseTab(tabID); err != nil {
		t.Fatalf("CloseTab failed: %v", err)
	}
	if h.sessMgr.ActiveTabCount() != 0 {
		t.Fatalf("expected 0 active tabs after CloseTab, got %d", h.sessMgr.ActiveTabCount())
	}
	if h.resMgr.ActiveCountForTab(tabID) != 0 {
		t.Fatalf("expected 0 cached snapshots for closed tab, got %d", h.resMgr.ActiveCountForTab(tabID))
	}
}

// Acceptance Criterion #3 (Real MySQL 8.4):
// 1) Exact fidelity for BIGINT, UNSIGNED BIGINT, DECIMAL(38,10), NULL vs empty string "", JSON, BLOB.
// 2) Real MySQL read-only account (sqlm_ro) rejects editor writes at the database layer (Error 1142 -> ERR_READ_ONLY_DENIED).
// 3) Independent writable DB + real TCP drop after autocommit INSERT and after explicit COMMIT -> returns ERR_COMMIT_OUTCOME_UNKNOWN, no auto-retry!
func TestMySQL84_Criterion3_FidelityReadOnlyAndCommitOutcomeUnknown(t *testing.T) {
	requireRealMySQL(t)
	ctx := context.Background()

	// Part A: Fidelity + Real Read-Only User
	h := newRealMySQLHarness(t, nil)
	roConn := h.saveMySQLConn(t, "mysql84-ro-dbuser", "sqlm_ro", "sqlm_ro_pw", "sqlm_demo", false) // clientRO=false to test REAL DB privilege rejection!

	fidResp, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-fid", ExecID: "fid-1", ConnectionID: roConn.ID, Database: "sqlm_demo",
		SQL: "SELECT id, snowflake_id, signed_min, amount, note, metadata, raw_bytes FROM ledger_entries ORDER BY id DESC",
		RunMode: "all",
	})
	if err != nil || fidResp.Error != nil {
		t.Fatalf("query ledger_entries failed: err=%v resp=%+v", err, fidResp)
	}
	rows := fidResp.Statements[0].Rows
	if len(rows) != 3 {
		t.Fatalf("expected 3 ledger rows, got %d", len(rows))
	}
	// Row 0: UNSIGNED BIGINT max, > JS MAX_SAFE_INTEGER, signed BIGINT min, DECIMAL(38,10), NULL note, binary blob
	if rows[0][0].Kind != dto.KindBigInt || rows[0][0].Text != "18446744073709551615" {
		t.Fatalf("UNSIGNED BIGINT fidelity broken: %+v", rows[0][0])
	}
	if rows[0][1].Kind != dto.KindBigInt || rows[0][1].Text != "9007199254740993" {
		t.Fatalf("BIGINT > 2^53-1 fidelity broken: %+v", rows[0][1])
	}
	if rows[0][2].Kind != dto.KindBigInt || rows[0][2].Text != "-9223372036854775808" {
		t.Fatalf("BIGINT min fidelity broken: %+v", rows[0][2])
	}
	if rows[0][3].Kind != dto.KindDecimal || rows[0][3].Text != "1234567890123456789012345678.1234567890" {
		t.Fatalf("DECIMAL(38,10) fidelity broken: %+v", rows[0][3])
	}
	if rows[0][4].Kind != dto.KindNull {
		t.Fatalf("expected SQL NULL to have KindNull, got %+v", rows[0][4])
	}
	if rows[0][6].Kind != dto.KindBinary || rows[0][6].StreamRef == "" || rows[0][6].ByteLength != 16 {
		t.Fatalf("expected KindBinary with StreamRef and ByteLength=16, got %+v", rows[0][6])
	}
	if blob, ok := h.qrySvc.GetStreamBlob(rows[0][6].StreamRef); !ok || len(blob.Data) != 16 {
		t.Fatalf("failed to retrieve offloaded BLOB via StreamRef: ok=%v", ok)
	}

	// Row 1: negative small DECIMAL + EMPTY STRING '' strictly distinct from NULL!
	if rows[1][3].Kind != dto.KindDecimal || rows[1][3].Text != "-0.0000000001" {
		t.Fatalf("negative DECIMAL fidelity broken: %+v", rows[1][3])
	}
	if rows[1][4].Kind != dto.KindString || rows[1][4].Text != "" {
		t.Fatalf("expected empty string '' to be KindString with Text=='', got %+v", rows[1][4])
	}
	if rows[2][4].Kind != dto.KindString || rows[2][4].Text != "normal text 中文 🚀" {
		t.Fatalf("UTF-8MB4 Chinese/emoji fidelity broken: got %+v", rows[2][4])
	}

	// Real database read-only user attempting write from editor MUST be rejected by MySQL 8.4 itself!
	writeAttempt, err := h.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-fid", ExecID: "ro-write", ConnectionID: roConn.ID, Database: "sqlm_demo",
		SQL: "INSERT INTO customers (name) VALUES ('should_be_rejected')", RunMode: "all",
	})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if writeAttempt.Error == nil || writeAttempt.Error.Code != dto.ErrReadOnlyDenied {
		t.Fatalf("expected real MySQL read-only user rejection with ERR_READ_ONLY_DENIED, got %+v", writeAttempt)
	}
	t.Logf("Verified real MySQL read-only account rejected write: %s", writeAttempt.Error.Error())

	// Part B: Autocommit Write & Explicit COMMIT confirmation lost on REAL MySQL 8.4 via fault-injecting TCP wrapper!
	var dropNextRead atomic.Bool
	faultDialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		rawConn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &dropOnReadAfterWriteConn{Conn: rawConn, armDrop: &dropNextRead}, nil
	}

	hFault := newRealMySQLHarness(t, faultDialer)
	rwConn := hFault.saveMySQLConn(t, "mysql84-fault-rw", "sqlm_rw", "sqlm_rw_pw", "sqlm_write_test", false)

	// Sub-case B1: Autocommit INSERT confirmation lost after bytes sent to MySQL 8.4
	tagAuto := fmt.Sprintf("autocommit-drop-%d", time.Now().UnixNano())
	if _, err := hFault.qrySvc.OpenTabSession(ctx, "tab-auto-drop", rwConn.ID, "sqlm_write_test", ""); err != nil {
		t.Fatalf("open tab-auto-drop: %v", err)
	}
	dropNextRead.Store(true)
	autoResp, err := hFault.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-auto-drop", ExecID: "auto-1", ConnectionID: rwConn.ID, Database: "sqlm_write_test",
		SQL: fmt.Sprintf("INSERT INTO write_probe (tag) VALUES ('%s')", tagAuto), RunMode: "all",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if autoResp.Error == nil || autoResp.Error.Code != dto.ErrCommitOutcomeUnknown || autoResp.Error.RetrySafe {
		t.Fatalf("expected ERR_COMMIT_OUTCOME_UNKNOWN (RetrySafe=false) on autocommit write drop, got %+v", autoResp)
	}
	if autoResp.SessionState != dto.SessionCommitUnknown {
		t.Fatalf("expected SessionCommitUnknown, got %s", autoResp.SessionState)
	}

	// Sub-case B2: Explicit BEGIN -> INSERT -> COMMIT confirmation lost after COMMIT bytes sent to MySQL 8.4
	dropNextRead.Store(false)
	tagCommit := fmt.Sprintf("commit-drop-%d", time.Now().UnixNano())
	if _, err := hFault.qrySvc.OpenTabSession(ctx, "tab-commit-drop", rwConn.ID, "sqlm_write_test", ""); err != nil {
		t.Fatalf("open tab-commit-drop: %v", err)
	}
	prepResp, err := hFault.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-commit-drop", ExecID: "tx-prep", ConnectionID: rwConn.ID, Database: "sqlm_write_test",
		SQL: fmt.Sprintf("BEGIN; INSERT INTO write_probe (tag) VALUES ('%s');", tagCommit), RunMode: "all",
	})
	if err != nil || prepResp.Error != nil || prepResp.SessionState != dto.SessionAliveInTx {
		t.Fatalf("tx prep failed: err=%v resp=%+v", err, prepResp)
	}

	dropNextRead.Store(true)
	commitResp, err := hFault.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-commit-drop", ExecID: "tx-commit", ConnectionID: rwConn.ID, Database: "sqlm_write_test",
		SQL: "COMMIT", RunMode: "all",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if commitResp.Error == nil || commitResp.Error.Code != dto.ErrCommitOutcomeUnknown || commitResp.Error.RetrySafe {
		t.Fatalf("expected ERR_COMMIT_OUTCOME_UNKNOWN (RetrySafe=false) on explicit COMMIT drop, got %+v", commitResp)
	}
	if commitResp.SessionState != dto.SessionCommitUnknown {
		t.Fatalf("expected SessionCommitUnknown after COMMIT drop, got %s", commitResp.SessionState)
	}
	// Verify subsequent execution on same tab is blocked until explicit reconnect (no auto-retry!)
	blockedResp, _ := hFault.qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-commit-drop", ExecID: "after-unknown", ConnectionID: rwConn.ID, Database: "sqlm_write_test",
		SQL: "SELECT 1", RunMode: "all",
	})
	if blockedResp.Error == nil || blockedResp.Error.Code != dto.ErrCommitOutcomeUnknown {
		t.Fatalf("expected blocked execution while SessionCommitUnknown, got %+v", blockedResp)
	}
}

// dropOnReadAfterWriteConn lets the armed Write reach real MySQL 8.4, then closes the socket
// on the immediately following Read so client confirmation is lost cleanly and deterministically.
type dropOnReadAfterWriteConn struct {
	net.Conn
	armDrop       *atomic.Bool
	writtenArmed  atomic.Bool
}

func (d *dropOnReadAfterWriteConn) Write(p []byte) (int, error) {
	n, err := d.Conn.Write(p)
	if err == nil && n > 0 && d.armDrop.CompareAndSwap(true, false) {
		d.writtenArmed.Store(true)
	}
	return n, err
}

func (d *dropOnReadAfterWriteConn) Read(p []byte) (int, error) {
	if d.writtenArmed.CompareAndSwap(true, false) {
		// Brief pause so MySQL 8.4 receives and processes the packet before TCP teardown.
		time.Sleep(40 * time.Millisecond)
		_ = d.Conn.Close()
		return 0, io.EOF
	}
	return d.Conn.Read(p)
}
