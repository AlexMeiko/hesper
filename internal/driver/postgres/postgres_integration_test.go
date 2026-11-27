package postgres_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"hesper/internal/driver"
	"hesper/internal/driver/postgres"
	"hesper/internal/dto"
	"hesper/internal/repository"
	"hesper/internal/resultbuf"
	"hesper/internal/secret"
	"hesper/internal/service"
	"hesper/internal/session"
)

func pgHostPort() (string, int) {
	host := os.Getenv("SQLM_TEST_PG_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 15432
	if p := os.Getenv("SQLM_TEST_PG_PORT"); p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}
	return host, port
}

func requireRealPG(t *testing.T) {
	t.Helper()
	host, port := pgHostPort()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		if os.Getenv("SQLM_REQUIRE_REAL_DB") == "1" {
			t.Fatalf("real PostgreSQL 16 not reachable at %s:%d (SQLM_REQUIRE_REAL_DB=1): %v", host, port, err)
		}
		t.Skipf("real PostgreSQL 16 not reachable at %s:%d: %v", host, port, err)
	}
	_ = c.Close()
}

// Verifies that our common interfaces (driver, session, service) do NOT hardcode MySQL assumptions:
// 1. SwitchDatabase reconnects to a new PG backend PID (and blocks switching while inside a tx).
// 2. 3-level Database -> Schema -> Table + optional SequenceInspector + optional SchemaSwitcher.
// 3. CancelRequest in PG transaction transitions to AliveFailedTx (25P02) and keeps connection alive.
// 4. Execute-Once pagination on INSERT ... RETURNING * inserts ONLY ONCE.
// 5. Real PG read-only account (sqlm_ro) rejects editor writes at DB layer; BIGINT/NUMERIC/NULL/'' fidelity preserved.
func TestPostgres16_AbstractionNeutralityAndAcceptance(t *testing.T) {
	requireRealPG(t)
	ctx := context.Background()
	dir := t.TempDir()

	repo, err := repository.OpenLocalDB(dir + "/app.db")
	if err != nil {
		t.Fatalf("open app.db: %v", err)
	}
	defer repo.Close()

	sec := secret.NewFileOnlyStore(dir)
	reg := driver.NewRegistry()
	reg.Register(postgres.NewOpener())
	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: dir + "/results"})
	defer resMgr.CloseAll()

	connSvc := service.NewConnectionService(repo, sec, nil, resMgr)
	sessMgr := session.NewManager(reg, connSvc.ResolveConfig)
	defer sessMgr.CloseAll()
	connSvc.SetSessionManager(sessMgr)

	schSvc := service.NewSchemaService(sessMgr)
	qrySvc := service.NewQueryService(sessMgr, resMgr, repo)

	host, port := pgHostPort()
	rwConn, err := connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name: "pg16-rw", Driver: dto.EnginePostgres, Password: "sqlm_rw_pw", UpdatePassword: true,
		Params: dto.ConnectionParams{Host: host, Port: port, Username: "sqlm_rw", DefaultDatabase: "sqlm_demo"},
	})
	if err != nil {
		t.Fatalf("save pg rw conn: %v", err)
	}

	cRes, err := connSvc.Connect(ctx, rwConn.ID)
	if err != nil {
		t.Fatalf("connect pg16: %v", err)
	}
	t.Logf("Connected real PostgreSQL: version=%s switchDBReconnects=%v hasSchemas=%v",
		cRes.ServerVersion, cRes.Capabilities.SwitchDBReconnects, cRes.Capabilities.HasSchemas)
	if !cRes.Capabilities.SwitchDBReconnects || !cRes.Capabilities.HasSchemas || !cRes.Capabilities.SupportsSequences {
		t.Fatalf("unexpected PG capabilities: %+v", cRes.Capabilities)
	}

	// 1) 3-level Database -> Schema -> Table + Sequences across TWO different databases (sqlm_demo & sqlm_alt)
	schemas, err := schSvc.ListSchemas(ctx, rwConn.ID, "sqlm_demo")
	if err != nil || len(schemas) < 2 {
		t.Fatalf("expected >=2 schemas in sqlm_demo, got err=%v schemas=%v", err, schemas)
	}
	analyticsTables, err := schSvc.ListTables(ctx, rwConn.ID, "sqlm_demo", "analytics")
	if err != nil || len(analyticsTables) != 1 || analyticsTables[0].Name != "metrics" {
		t.Fatalf("expected analytics.metrics, got err=%v tables=%+v", err, analyticsTables)
	}
	seqs, err := schSvc.ListSequences(ctx, rwConn.ID, "sqlm_demo", "public")
	if err != nil || len(seqs) == 0 {
		t.Fatalf("expected public.order_seq sequence, got err=%v seqs=%+v", err, seqs)
	}

	// 2) SwitchTabDatabase in PostgreSQL MUST reconnect to a new backend PID & refuse while in tx!
	tabID := "pg-tab-1"
	info1, err := qrySvc.OpenTabSession(ctx, tabID, rwConn.ID, "sqlm_demo", "analytics")
	if err != nil {
		t.Fatalf("open pg tab: %v", err)
	}
	// Verify optional SchemaSwitcher set search_path to analytics so unqualified `metrics` works!
	mResp, err := qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "pg-sch", ConnectionID: rwConn.ID, Database: "sqlm_demo", Schema: "analytics",
		SQL: "SELECT COUNT(*) FROM metrics", RunMode: "all",
	})
	if err != nil || mResp.Error != nil || mResp.Statements[0].Rows[0][0].Text != "150" {
		t.Fatalf("expected search_path=analytics count=150, got err=%v resp=%+v", err, mResp)
	}

	// Begin a transaction -> switching database MUST be rejected rather than silently dropping tx!
	_, _ = qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "pg-begin", ConnectionID: rwConn.ID, Database: "sqlm_demo",
		SQL: "BEGIN", RunMode: "all",
	})
	if _, switchErr := qrySvc.SwitchTabDatabase(ctx, tabID, "sqlm_alt"); switchErr == nil {
		t.Fatal("expected SwitchTabDatabase in PG to be refused while inside an active transaction")
	}
	_, _ = qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "pg-rb", ConnectionID: rwConn.ID, Database: "sqlm_demo",
		SQL: "ROLLBACK", RunMode: "all",
	})

	// Now switch to sqlm_alt -> must reconnect cleanly with a different backend PID!
	info2, err := qrySvc.SwitchTabDatabase(ctx, tabID, "sqlm_alt")
	if err != nil {
		t.Fatalf("SwitchTabDatabase to sqlm_alt failed: %v", err)
	}
	if info2.CurrentDB != "sqlm_alt" || info2.ServerConnID == info1.ServerConnID {
		t.Fatalf("expected new PG backend PID on sqlm_alt, before=%+v after=%+v", info1, info2)
	}
	altResp, err := qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: tabID, ExecID: "pg-alt", ConnectionID: rwConn.ID, Database: "sqlm_alt",
		SQL: "SELECT title FROM public.alt_items", RunMode: "all",
	})
	if err != nil || altResp.Error != nil || altResp.Statements[0].Rows[0][0].Text != "from sqlm_alt database" {
		t.Fatalf("query in sqlm_alt failed: err=%v resp=%+v", err, altResp)
	}

	// 3) Execute-Once on INSERT ... RETURNING * paged to page 2 -> MUST insert only ONCE!
	tag := fmt.Sprintf("pg-once-%d", time.Now().UnixNano())
	retResp, err := qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "pg-tab-ret", ExecID: "pg-ret-1", ConnectionID: rwConn.ID, Database: "sqlm_demo",
		SQL: fmt.Sprintf("INSERT INTO public.write_probe (tag) SELECT '%s' FROM generate_series(1, 5) RETURNING id, tag", tag),
		RunMode: "all", PageSize: 2,
	})
	if err != nil || retResp.Error != nil || retResp.Statements[0].TotalBuffered != 5 {
		t.Fatalf("PG INSERT...RETURNING failed: err=%v resp=%+v", err, retResp)
	}
	p2, err := qrySvc.FetchResultPage(dto.FetchResultPageRequest{ResultID: retResp.Statements[0].ResultID, Page: 2, PageSize: 2})
	if err != nil || len(p2.Rows) != 2 {
		t.Fatalf("PG snapshot page 2 failed: err=%v p2=%+v", err, p2)
	}
	cntResp, _ := qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "pg-tab-ret", ExecID: "pg-ret-cnt", ConnectionID: rwConn.ID, Database: "sqlm_demo",
		SQL: fmt.Sprintf("SELECT COUNT(*) FROM public.write_probe WHERE tag = '%s'", tag), RunMode: "all",
	})
	if cntResp.Statements[0].Rows[0][0].Text != "5" {
		t.Fatalf("INSERT...RETURNING re-executed! count=%s want 5", cntResp.Statements[0].Rows[0][0].Text)
	}

	// 4) PG CancelRequest inside transaction -> ERR_STATEMENT_CANCELED + SessionAliveFailedTx!
	_, _ = qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "pg-tab-cancel", ExecID: "pg-c0", ConnectionID: rwConn.ID, Database: "sqlm_demo",
		SQL: "BEGIN", RunMode: "all",
	})
	var cancelRes *dto.ExecuteSQLResponse
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cancelRes, _ = qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID: "pg-tab-cancel", ExecID: "pg-slow", ConnectionID: rwConn.ID, Database: "sqlm_demo",
			SQL: "SELECT pg_sleep(10)", RunMode: "all",
		})
	}()
	time.Sleep(250 * time.Millisecond)
	_ = qrySvc.CancelExecution(ctx, "pg-tab-cancel", "pg-slow")
	wg.Wait()
	if cancelRes == nil || cancelRes.Error == nil || cancelRes.Error.Code != dto.ErrStatementCanceled {
		t.Fatalf("expected PG ERR_STATEMENT_CANCELED, got %+v", cancelRes)
	}
	if cancelRes.SessionState != dto.SessionAliveFailedTx {
		t.Fatalf("expected PG SessionAliveFailedTx after cancel in tx, got %s", cancelRes.SessionState)
	}

	// 5) Real PG read-only user + BIGINT/NUMERIC/NULL/'' fidelity
	roConn, err := connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name: "pg16-ro", Driver: dto.EnginePostgres, Password: "sqlm_ro_pw", UpdatePassword: true,
		Params: dto.ConnectionParams{Host: host, Port: port, Username: "sqlm_ro", DefaultDatabase: "sqlm_demo"},
	})
	if err != nil {
		t.Fatalf("save pg ro conn: %v", err)
	}
	fidResp, err := qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "pg-tab-ro", ExecID: "pg-fid", ConnectionID: roConn.ID, Database: "sqlm_demo",
		SQL: "SELECT snowflake_id, balance, note, raw_bytes FROM public.accounts ORDER BY id", RunMode: "all",
	})
	if err != nil || fidResp.Error != nil {
		t.Fatalf("PG fidelity query failed: err=%v resp=%+v", err, fidResp)
	}
	r0 := fidResp.Statements[0].Rows[0]
	r1 := fidResp.Statements[0].Rows[1]
	if r0[0].Text != "9007199254740993" || r0[1].Text != "1234567890123456789012345678.1234567890" || r0[2].Kind != dto.KindNull {
		t.Fatalf("PG row 0 fidelity broken: %+v", r0)
	}
	if r1[2].Kind != dto.KindString || r1[2].Text != "" {
		t.Fatalf("PG empty string vs NULL broken: %+v", r1[2])
	}
	roWrite, _ := qrySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "pg-tab-ro", ExecID: "pg-ro-w", ConnectionID: roConn.ID, Database: "sqlm_demo",
		SQL: "INSERT INTO public.write_probe (tag) VALUES ('denied')", RunMode: "all",
	})
	if roWrite.Error == nil || roWrite.Error.Code != dto.ErrReadOnlyDenied {
		t.Fatalf("expected PG real read-only account rejection with ERR_READ_ONLY_DENIED, got %+v", roWrite)
	}
}
