package mysql_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"hesper/internal/driver"
	"hesper/internal/driver/mysql"
	"hesper/internal/dto"
	"hesper/internal/resultbuf"
	"hesper/internal/service"
	"hesper/internal/session"
)

type mariaDropAfterWriteConn struct {
	net.Conn
	shouldDrop func() bool
	dropped    bool
	mu         sync.Mutex
}

func (c *mariaDropAfterWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	if err == nil && n > 0 && c.shouldDrop != nil && c.shouldDrop() {
		c.dropped = true
		_ = c.Conn.Close()
	}
	c.mu.Unlock()
	return n, err
}

func (c *mariaDropAfterWriteConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	wasDropped := c.dropped
	c.mu.Unlock()
	if wasDropped {
		return 0, io.ErrUnexpectedEOF
	}
	return c.Conn.Read(p)
}

func mariadbTestHostPort() (string, int) {
	if addr := os.Getenv("SQLM_MARIADB_ADDR"); addr != "" {
		h, p, err := net.SplitHostPort(addr)
		if err == nil {
			if port, convErr := strconv.Atoi(p); convErr == nil {
				return h, port
			}
		}
	}
	return "127.0.0.1", 13307
}

func requireMariaDB114(t *testing.T) (string, int) {
	t.Helper()
	host, port := mariadbTestHostPort()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 1500*time.Millisecond)
	if err != nil {
		t.Skipf("real MariaDB 11.4 not reachable at %s:%d (%v); run `docker compose -f docker-compose.test.yml up -d mariadb`", host, port, err)
	}
	_ = c.Close()
	return host, port
}

func newMariaDBHarness(t *testing.T, configs map[string]driver.ResolvedConfig, resOpts resultbuf.Options) (
	*session.Manager,
	*resultbuf.Manager,
	*service.SchemaService,
	*service.QueryService,
) {
	t.Helper()
	reg := driver.NewRegistry()
	reg.Register(mysql.NewOpener(dto.EngineMariaDB))

	resolver := func(_ context.Context, connID string, overrideDB string) (driver.ResolvedConfig, error) {
		cfg, ok := configs[connID]
		if !ok {
			return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrNotFound, "unknown test connection", connID)
		}
		if overrideDB != "" {
			cfg.Database = overrideDB
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

func TestMariaDB114_Gate1_FullAcceptance(t *testing.T) {
	host, port := requireMariaDB114(t)
	ctx := context.Background()

	var dropMu sync.Mutex
	dropAfterWrite := false

	configs := map[string]driver.ResolvedConfig{
		"maria-rw": {
			ConnectionID: "maria-rw",
			Engine:       dto.EngineMariaDB,
			Host:         host,
			Port:         port,
			Username:     "sqlm_rw",
			Password:     "sqlm_rw_pw",
			Database:     "sqlm_demo",
		},
		"maria-ro-user": {
			ConnectionID: "maria-ro-user",
			Engine:       dto.EngineMariaDB,
			Host:         host,
			Port:         port,
			Username:     "sqlm_ro",
			Password:     "sqlm_ro_pw",
			Database:     "sqlm_demo",
			ReadOnly:     false, // Verify real DB privileges reject writes even without client guardrail
		},
		"maria-write-drop": {
			ConnectionID: "maria-write-drop",
			Engine:       dto.EngineMariaDB,
			Host:         host,
			Port:         port,
			Username:     "sqlm_rw",
			Password:     "sqlm_rw_pw",
			Database:     "sqlm_write_test",
			DialContext: func(dctx context.Context, network, addr string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 5 * time.Second}
				raw, err := d.DialContext(dctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &mariaDropAfterWriteConn{
					Conn: raw,
					shouldDrop: func() bool {
						dropMu.Lock()
						defer dropMu.Unlock()
						return dropAfterWrite
					},
				}, nil
			},
		},
	}

	sessMgr, resMgr, schemaSvc, querySvc := newMariaDBHarness(t, configs, resultbuf.Options{
		MaxMemRows:  40,
		MaxMemBytes: 16 * 1024,
		IdleTTL:     400 * time.Millisecond,
	})

	// 1. Real server detection + MariaDB SEQUENCE support + Table list separation
	var fact driver.ServerFact
	var caps dto.Capabilities
	err := sessMgr.WithMetaConn(ctx, "maria-rw", "sqlm_demo", func(conn driver.Conn) error {
		var ferr error
		fact, ferr = conn.ServerFact(ctx)
		if ferr != nil {
			return ferr
		}
		caps = conn.Capabilities(fact.ServerVersion)
		return nil
	})
	if err != nil {
		t.Fatalf("Connect real MariaDB failed: %v", err)
	}
	t.Logf("Connected real MariaDB: type=%s version=%s supportsSequences=%v", fact.DetectedType, fact.ServerVersion, caps.SupportsSequences)
	if fact.DetectedType != dto.EngineMariaDB {
		t.Fatalf("expected EngineMariaDB, got %s", fact.DetectedType)
	}
	if !caps.SupportsSequences {
		t.Fatalf("expected SupportsSequences=true on MariaDB")
	}

	seqs, err := schemaSvc.ListSequences(ctx, "maria-rw", "sqlm_demo", "")
	if err != nil {
		t.Fatalf("ListSequences: %v", err)
	}
	foundOrderSeq := false
	for _, s := range seqs {
		if s.Name == "order_seq" && s.Kind == "sequence" {
			foundOrderSeq = true
		}
	}
	if !foundOrderSeq {
		t.Fatalf("expected order_seq in MariaDB ListSequences, got %+v", seqs)
	}

	tables, err := schemaSvc.ListTables(ctx, "maria-rw", "sqlm_demo", "")
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	for _, tbl := range tables {
		if tbl.Name == "order_seq" {
			t.Fatalf("sequence order_seq must NOT appear in BASE TABLE list")
		}
	}

	// 2. Criterion 1: Bypass KILL QUERY cancel + session/tx/temp table preservation + late-cancel race
	tabID := "maria-tab-c1"
	info, err := querySvc.OpenTabSession(ctx, tabID, "maria-rw", "sqlm_demo", "")
	if err != nil {
		t.Fatalf("OpenTabSession: %v", err)
	}
	origConnID := info.ServerConnID

	setupResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "m-setup", ConnectionID: "maria-rw",
		SQL: "CREATE TEMPORARY TABLE tmp_maria_keep (v VARCHAR(32)); INSERT INTO tmp_maria_keep VALUES ('alive'); BEGIN;",
	})
	if err != nil || setupResp.Error != nil {
		t.Fatalf("setup tx/temp failed: err=%v respErr=%v", err, setupResp.Error)
	}
	if setupResp.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected SessionAliveInTx, got %s", setupResp.SessionState)
	}

	var slowResp *dto.ExecuteSQLResponse
	var slowErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowResp, slowErr = querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID: tabID, ExecID: "m-slow-A", ConnectionID: "maria-rw",
			SQL: `SELECT COUNT(*) FROM events e1 CROSS JOIN events e2 CROSS JOIN events e3 WHERE SHA2(CONCAT(e1.id, e2.id, e3.id), 256) != ''`,
		})
	}()
	time.Sleep(250 * time.Millisecond)

	cancelRes := querySvc.CancelExecution(ctx, tabID, "m-slow-A")
	wg.Wait()
	if slowErr != nil || slowResp.Error == nil || slowResp.Error.Code != dto.ErrStatementCanceled {
		t.Fatalf("expected ERR_STATEMENT_CANCELED on MariaDB KILL QUERY, got err=%v resp=%+v cancel=%+v", slowErr, slowResp, cancelRes)
	}
	if slowResp.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected tx preserved after cancel, got %s", slowResp.SessionState)
	}

	verifyResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "m-verify", ConnectionID: "maria-rw",
		SQL: "SELECT CONNECTION_ID(), (SELECT v FROM tmp_maria_keep LIMIT 1); ROLLBACK;",
	})
	if err != nil || verifyResp.Error != nil {
		t.Fatalf("verify after cancel failed: %v / %v", err, verifyResp.Error)
	}
	if verifyResp.Statements[0].Rows[0][0].Text != origConnID || verifyResp.Statements[0].Rows[0][1].Text != "alive" {
		t.Fatalf("connection or temp table lost: got %+v want connID=%s", verifyResp.Statements[0].Rows[0], origConnID)
	}

	// Late-cancel race (3 rounds): delayed cancel for A must NEVER kill queued B
	for round := 0; round < 3; round++ {
		execA := "m-race-A-" + strconv.Itoa(round)
		execB := "m-race-B-" + strconv.Itoa(round)
		var wgRace sync.WaitGroup
		wgRace.Add(3)
		var respB *dto.ExecuteSQLResponse

		go func() {
			defer wgRace.Done()
			_, _ = querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				TabID: tabID, ExecID: execA, ConnectionID: "maria-rw",
				SQL: "SELECT COUNT(*) FROM events e1 CROSS JOIN (SELECT 1 UNION SELECT 2) t",
			})
		}()
		go func() {
			defer wgRace.Done()
			time.Sleep(10 * time.Millisecond)
			_ = querySvc.CancelExecution(ctx, tabID, execA)
		}()
		go func() {
			defer wgRace.Done()
			time.Sleep(12 * time.Millisecond)
			respB, _ = querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				TabID: tabID, ExecID: execB, ConnectionID: "maria-rw",
				SQL: "SELECT 'B_OK', COUNT(*) FROM events",
			})
		}()
		wgRace.Wait()
		if respB == nil || respB.Error != nil {
			t.Fatalf("round %d: queued B failed under late-cancel race: %+v", round, respB)
		}
		if respB.Statements[0].Rows[0][0].Text != "B_OK" {
			t.Fatalf("round %d: unexpected B result: %+v", round, respB.Statements[0].Rows)
		}
	}

	// 3. Criterion 2: MariaDB native INSERT ... RETURNING * Execute-Once + Snapshot Spill/Expiry + Live Browse + CloseTab
	retTag := "maria-ret-" + strconv.FormatInt(time.Now().UnixNano()%1000000, 10)
	retResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "m-returning", ConnectionID: "maria-rw",
		Database: "sqlm_write_test",
		SQL:      "USE sqlm_write_test; INSERT INTO write_probe (tag) VALUES ('" + retTag + "') RETURNING id, tag;",
		PageSize: 10,
	})
	if err != nil || retResp.Error != nil {
		t.Fatalf("MariaDB INSERT ... RETURNING failed: %v / %v", err, retResp.Error)
	}
	if len(retResp.Statements) != 2 || !retResp.Statements[1].IsResultSet || retResp.Statements[1].ResultID == "" {
		t.Fatalf("expected result set from MariaDB INSERT ... RETURNING, got %+v", retResp.Statements)
	}
	p1, err := querySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: retResp.Statements[1].ResultID, Page: 1, PageSize: 10})
	if err != nil || len(p1.Rows) != 1 || p1.Rows[0][1].Text != retTag {
		t.Fatalf("FetchResultPage on RETURNING snapshot failed: err=%v p1=%+v", err, p1)
	}
	cntResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "m-ret-cnt", ConnectionID: "maria-rw",
		SQL: "SELECT COUNT(*) FROM sqlm_write_test.write_probe WHERE tag = '" + retTag + "'",
	})
	if err != nil || cntResp.Statements[0].Rows[0][0].Text != "1" {
		t.Fatalf("expected strictly 1 inserted row from INSERT ... RETURNING, got %+v", cntResp)
	}

	// Multi-page snapshot spill + expiry -> ERR_RESULT_EXPIRED
	evResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "m-spill", ConnectionID: "maria-rw",
		SQL: "SELECT id, category, message FROM sqlm_demo.events ORDER BY id", PageSize: 60,
	})
	if err != nil || evResp.Error != nil {
		t.Fatalf("events query failed: %v / %v", err, evResp.Error)
	}
	evResID := evResp.Statements[0].ResultID
	p2, err := querySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: evResID, Page: 2, PageSize: 60})
	if err != nil || len(p2.Rows) != 60 || p2.Rows[0][0].Text != "61" {
		t.Fatalf("page 2 snapshot failed: err=%v p2=%+v", err, p2)
	}
	time.Sleep(550 * time.Millisecond)
	_, expErr := querySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: evResID, Page: 3, PageSize: 60})
	if expErr == nil {
		t.Fatalf("expected ERR_RESULT_EXPIRED after IdleTTL")
	}

	// Live BrowseTable + Editability (InnoDB PK vs MyISAM vs no-PK)
	browse, err := querySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: "maria-rw", Database: "sqlm_demo", Table: "events", Page: 2, PageSize: 50,
	})
	if err != nil || len(browse.Rows) != 50 || browse.TotalRows != 250 || !browse.StableOrderUsed {
		t.Fatalf("MariaDB BrowseTable failed: err=%v browse=%+v", err, browse)
	}
	myisamDet, err := schemaSvc.GetTableDetail(ctx, "maria-rw", "sqlm_demo", "", "legacy_logs")
	if err != nil || myisamDet.Editability.BatchTransactional {
		t.Fatalf("expected MyISAM BatchTransactional=false on MariaDB, got %+v err=%v", myisamDet, err)
	}
	noPkDet, err := schemaSvc.GetTableDetail(ctx, "maria-rw", "sqlm_demo", "", "no_pk_notes")
	if err != nil || noPkDet.Editability.RowIdentity != dto.RowIdentityNone {
		t.Fatalf("expected RowIdentityNone for keyless table on MariaDB, got %+v err=%v", noPkDet, err)
	}

	// CloseTab releases dedicated connection & snapshots
	if err := querySvc.CloseTab(tabID); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if sessMgr.ActiveTabCount() != 0 || resMgr.ActiveCount() != 0 {
		t.Fatalf("CloseTab leaked resources: tabs=%d snaps=%d", sessMgr.ActiveTabCount(), resMgr.ActiveCount())
	}

	// 4. Criterion 3: Data fidelity + real read-only account rejection + TCP drop CommitOutcomeUnknown
	fidTab := "maria-tab-fid"
	fidResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: fidTab, ExecID: "m-fid", ConnectionID: "maria-rw", Database: "sqlm_demo",
		SQL: "SELECT id, snowflake_id, signed_min, amount, note, raw_bytes FROM ledger_entries ORDER BY id DESC",
	})
	if err != nil || fidResp.Error != nil {
		t.Fatalf("fidelity query failed: %v / %v", err, fidResp.Error)
	}
	r0 := fidResp.Statements[0].Rows[0]
	r1 := fidResp.Statements[0].Rows[1]
	r2 := fidResp.Statements[0].Rows[2]
	if r0[0].Text != "18446744073709551615" || r0[1].Text != "9007199254740993" || r0[2].Text != "-9223372036854775808" || r0[3].Text != "1234567890123456789012345678.1234567890" {
		t.Fatalf("MariaDB boundary numeric fidelity mismatch: %+v", r0)
	}
	if r0[4].Kind != dto.KindNull {
		t.Fatalf("expected NULL, got %+v", r0[4])
	}
	if r1[4].Kind != dto.KindString || r1[4].Text != "" {
		t.Fatalf("expected empty string, got %+v", r1[4])
	}
	if r2[4].Text != "MariaDB 中文 🚀" {
		t.Fatalf("UTF8MB4 mismatch: got %q", r2[4].Text)
	}
	if r0[5].Kind != dto.KindBinary || r0[5].StreamRef == "" {
		t.Fatalf("expected BLOB StreamRef, got %+v", r0[5])
	}
	blob, ok := querySvc.GetStreamBlob(r0[5].StreamRef)
	if !ok || len(blob.Data) != 16 {
		t.Fatalf("expected 16-byte PNG header blob, got ok=%v blob=%+v", ok, blob)
	}
	_ = querySvc.CloseTab(fidTab)

	// Real read-only user rejection
	roTab := "maria-tab-ro"
	roResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: roTab, ExecID: "m-ro-write", ConnectionID: "maria-ro-user", Database: "sqlm_demo",
		SQL: "INSERT INTO customers (name) VALUES ('should-fail')",
	})
	if err != nil || roResp.Error == nil || roResp.Error.Code != dto.ErrReadOnlyDenied {
		t.Fatalf("expected real MariaDB sqlm_ro user rejected with ERR_READ_ONLY_DENIED, got err=%v resp=%+v", err, roResp)
	}
	t.Logf("Verified real MariaDB read-only user rejected write: %s", roResp.Error.Error())
	_ = querySvc.CloseTab(roTab)

	// Real TCP drop on autocommit INSERT -> ERR_COMMIT_OUTCOME_UNKNOWN
	dropTab := "maria-tab-drop"
	if _, err := querySvc.OpenTabSession(ctx, dropTab, "maria-write-drop", "sqlm_write_test", ""); err != nil {
		t.Fatalf("OpenTabSession dropTab: %v", err)
	}
	dropMu.Lock()
	dropAfterWrite = true
	dropMu.Unlock()
	dropResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: dropTab, ExecID: "m-drop-auto", ConnectionID: "maria-write-drop",
		SQL: "INSERT INTO write_probe (tag) VALUES ('maria-drop-autocommit')",
	})
	dropMu.Lock()
	dropAfterWrite = false
	dropMu.Unlock()
	if err != nil || dropResp.Error == nil || dropResp.Error.Code != dto.ErrCommitOutcomeUnknown || dropResp.Error.RetrySafe {
		t.Fatalf("expected ERR_COMMIT_OUTCOME_UNKNOWN (RetrySafe=false) on autocommit TCP drop, got err=%v resp=%+v", err, dropResp)
	}
	if dropResp.SessionState != dto.SessionCommitUnknown {
		t.Fatalf("expected SessionCommitUnknown, got %s", dropResp.SessionState)
	}
	_ = querySvc.CloseTab(dropTab)
}
