package service_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hesper/internal/driver"
	"hesper/internal/dto"
	"hesper/internal/entity"
	"hesper/internal/repository"
	"hesper/internal/resultbuf"
	"hesper/internal/service"
	"hesper/internal/session"
	"hesper/internal/sqlsplit"
)

// pgSimOpener simulates PostgreSQL session-per-database semantics in pure unit tests:
//   - Tracks live open connections per database.
//   - Rejects plain DROP DATABASE with SQLSTATE 55006 if any other connection is open on that DB,
//     or if the executing connection itself is connected to that DB.
//   - Allows injecting custom Exec errors (e.g. mid-tx error or 55006).
//   - Allows native PG DROP DATABASE ... WITH (FORCE).
//   - Rejects connections to dropped databases with SQLSTATE 3D000.
type pgSimOpener struct {
	mu            sync.Mutex
	serverVersion string
	dbs           map[string]bool
	openPerDB     map[string]int
	execLog       []string
	customExecErr func(sql string) *dto.AppError
}

func newPGSimOpener(serverVersion string, dbs ...string) *pgSimOpener {
	m := make(map[string]bool, len(dbs))
	for _, db := range dbs {
		m[db] = true
	}
	return &pgSimOpener{
		serverVersion: serverVersion,
		dbs:           m,
		openPerDB:     make(map[string]int),
	}
}

func (o *pgSimOpener) Type() dto.EngineType { return dto.EnginePostgres }

func (o *pgSimOpener) Open(_ context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	db := cfg.Database
	if db == "" {
		db = "postgres"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.dbs[db] {
		return nil, &dto.AppError{
			Code:     dto.ErrInternal,
			Message:  `PostgreSQL 错误 (3D000): database "` + db + `" does not exist`,
			SQLState: "3D000",
		}
	}
	o.openPerDB[db]++
	return &pgSimConn{
		opener:   o,
		dbName:   db,
		schema:   "public",
		readOnly: cfg.ReadOnly,
	}, nil
}

func (o *pgSimOpener) openCount(db string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.openPerDB[db]
}

type pgSimConn struct {
	mu       sync.Mutex
	opener   *pgSimOpener
	dbName   string
	schema   string
	readOnly bool
	closed   bool
}

func (c *pgSimConn) Ping(_ context.Context) error { return nil }

func (c *pgSimConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	db := c.dbName
	c.mu.Unlock()

	c.opener.mu.Lock()
	if c.opener.openPerDB[db] > 0 {
		c.opener.openPerDB[db]--
	}
	c.opener.mu.Unlock()
	return nil
}

func (c *pgSimConn) ServerFact(_ context.Context) (driver.ServerFact, error) {
	return driver.ServerFact{
		DetectedType:  dto.EnginePostgres,
		ServerVersion: c.opener.serverVersion,
		CurrentDB:     c.dbName,
		CurrentSchema: c.schema,
		ServerConnID:  "1001",
	}, nil
}

func (c *pgSimConn) Dialect() dto.EngineType { return dto.EnginePostgres }

func (c *pgSimConn) Capabilities(_ string) dto.Capabilities {
	return dto.Capabilities{
		HasDatabases:       true,
		HasSchemas:         true,
		SwitchDBReconnects: true,
		DDLTransactional:   true,
		SupportsViews:      true,
		SupportsSequences:  true,
	}
}

func (c *pgSimConn) ResetWireTracker()              {}
func (c *pgSimConn) WriteSentWithoutResponse() bool { return false }

func (c *pgSimConn) ClassifyError(err error, _ bool) *dto.AppError {
	if err == nil {
		return nil
	}
	if ae, ok := err.(*dto.AppError); ok {
		return ae
	}
	return dto.NewAppError(dto.ErrInternal, err.Error(), "")
}

func (c *pgSimConn) Query(_ context.Context, stmt driver.BoundStatement) (driver.RowStream, error) {
	c.opener.mu.Lock()
	c.opener.execLog = append(c.opener.execLog, stmt.SQL)
	fn := c.opener.customExecErr
	c.opener.mu.Unlock()
	if fn != nil {
		if err := fn(stmt.SQL); err != nil {
			return nil, err
		}
	}
	return &pgSimStream{}, nil
}

func (c *pgSimConn) Exec(_ context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	c.opener.mu.Lock()
	defer c.opener.mu.Unlock()
	c.opener.execLog = append(c.opener.execLog, stmt.SQL)

	if c.opener.customExecErr != nil {
		if err := c.opener.customExecErr(stmt.SQL); err != nil {
			return driver.ExecFact{}, err
		}
	}

	ddl := sqlsplit.InspectDDL(stmt.SQL, sqlsplit.DialectPostgres)
	if ddl.IsDropDB && ddl.TargetName != "" {
		if c.dbName == ddl.TargetName {
			return driver.ExecFact{}, &dto.AppError{
				Code:     dto.ErrInternal,
				Message:  "PostgreSQL 错误 (55006): cannot drop the currently open database",
				SQLState: "55006",
			}
		}
		if !ddl.HasForce && c.opener.openPerDB[ddl.TargetName] > 0 {
			return driver.ExecFact{}, &dto.AppError{
				Code:     dto.ErrInternal,
				Message:  `PostgreSQL 错误 (55006): database "` + ddl.TargetName + `" is being accessed by other users`,
				Detail:   "There are 2 other sessions using the database.",
				SQLState: "55006",
			}
		}
		delete(c.opener.dbs, ddl.TargetName)
	}
	return driver.ExecFact{AffectedRows: 0}, nil
}

// MetaInspector methods for ListDatabases fallback test
func (c *pgSimConn) ListDatabases(_ context.Context) ([]dto.DatabaseInfo, error) {
	c.opener.mu.Lock()
	defer c.opener.mu.Unlock()
	out := make([]dto.DatabaseInfo, 0, len(c.opener.dbs))
	for name := range c.opener.dbs {
		out = append(out, dto.DatabaseInfo{Name: name})
	}
	return out, nil
}
func (c *pgSimConn) ListSchemas(_ context.Context, _ string) ([]string, error) {
	return []string{"public"}, nil
}
func (c *pgSimConn) ListTables(_ context.Context, _, _ string) ([]dto.TableSummary, error) {
	return nil, nil
}
func (c *pgSimConn) ListViews(_ context.Context, _, _ string) ([]dto.ObjectSummary, error) {
	return nil, nil
}
func (c *pgSimConn) GetTableDetail(_ context.Context, _, _, _ string) (*dto.TableDetail, error) {
	return nil, nil
}
func (c *pgSimConn) GetTableDDL(_ context.Context, _, _, _ string) (string, error) { return "", nil }
func (c *pgSimConn) InspectTableEditability(_ context.Context, _, _, _ string, _ bool) (dto.TableEditability, error) {
	return dto.TableEditability{}, nil
}
func (c *pgSimConn) FetchCompletionCatalog(_ context.Context, _, _ string) (*dto.CompletionCatalog, error) {
	return nil, nil
}

type pgSimStream struct{}

func (s *pgSimStream) Columns() []dto.ColumnMeta {
	return []dto.ColumnMeta{{Name: "ok", Kind: dto.KindBool}}
}
func (s *pgSimStream) Next(_ driver.BlobRegistrar) ([]dto.CellValue, bool, error) {
	return nil, false, nil
}
func (s *pgSimStream) Close() error { return nil }

// mysqlSimOpener simulates MySQL unknown-database (1049) & RENAME TABLE behavior in pure unit tests.
type mysqlSimOpener struct {
	mu  sync.Mutex
	dbs map[string]bool
}

func newMySQLSimOpener(dbs ...string) *mysqlSimOpener {
	m := map[string]bool{"information_schema": true}
	for _, db := range dbs {
		m[db] = true
	}
	return &mysqlSimOpener{dbs: m}
}

func (o *mysqlSimOpener) Type() dto.EngineType { return dto.EngineMySQL }

func (o *mysqlSimOpener) Open(_ context.Context, cfg driver.ResolvedConfig) (driver.Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cfg.Database != "" && !o.dbs[cfg.Database] {
		return nil, &dto.AppError{
			Code:     dto.ErrInternal,
			Message:  "数据库错误 (1049): Unknown database '" + cfg.Database + "'",
			Detail:   "Unknown database '" + cfg.Database + "'",
			SQLState: "42000",
		}
	}
	return &mysqlSimConn{opener: o, dbName: cfg.Database}, nil
}

type mysqlSimConn struct {
	opener *mysqlSimOpener
	dbName string
}

func (c *mysqlSimConn) Ping(_ context.Context) error { return nil }
func (c *mysqlSimConn) Close() error                 { return nil }
func (c *mysqlSimConn) Dialect() dto.EngineType      { return dto.EngineMySQL }
func (c *mysqlSimConn) ResetWireTracker()            {}
func (c *mysqlSimConn) WriteSentWithoutResponse() bool {
	return false
}
func (c *mysqlSimConn) ServerFact(_ context.Context) (driver.ServerFact, error) {
	return driver.ServerFact{
		DetectedType:  dto.EngineMySQL,
		ServerVersion: "8.4.0",
		CurrentDB:     c.dbName,
		ServerConnID:  "42",
	}, nil
}
func (c *mysqlSimConn) Capabilities(_ string) dto.Capabilities {
	return dto.Capabilities{HasDatabases: true, SupportsViews: true}
}
func (c *mysqlSimConn) ClassifyError(err error, _ bool) *dto.AppError {
	if ae, ok := err.(*dto.AppError); ok {
		return ae
	}
	return dto.NewAppError(dto.ErrInternal, err.Error(), "")
}
func (c *mysqlSimConn) Query(_ context.Context, _ driver.BoundStatement) (driver.RowStream, error) {
	return &pgSimStream{}, nil
}
func (c *mysqlSimConn) Exec(_ context.Context, stmt driver.BoundStatement) (driver.ExecFact, error) {
	ddl := sqlsplit.InspectDDL(stmt.SQL, sqlsplit.DialectMySQL)
	if ddl.IsDropDB && ddl.TargetName != "" {
		c.opener.mu.Lock()
		delete(c.opener.dbs, ddl.TargetName)
		c.opener.mu.Unlock()
	}
	return driver.ExecFact{}, nil
}
func (c *mysqlSimConn) ListDatabases(_ context.Context) ([]dto.DatabaseInfo, error) {
	c.opener.mu.Lock()
	defer c.opener.mu.Unlock()
	out := make([]dto.DatabaseInfo, 0, len(c.opener.dbs))
	for name := range c.opener.dbs {
		out = append(out, dto.DatabaseInfo{Name: name})
	}
	return out, nil
}
func (c *mysqlSimConn) ListSchemas(_ context.Context, _ string) ([]string, error) { return nil, nil }
func (c *mysqlSimConn) ListTables(_ context.Context, _, _ string) ([]dto.TableSummary, error) {
	return nil, nil
}
func (c *mysqlSimConn) ListViews(_ context.Context, _, _ string) ([]dto.ObjectSummary, error) {
	return nil, nil
}
func (c *mysqlSimConn) GetTableDetail(_ context.Context, _, _, _ string) (*dto.TableDetail, error) {
	return nil, nil
}
func (c *mysqlSimConn) GetTableDDL(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}
func (c *mysqlSimConn) InspectTableEditability(_ context.Context, _, _, _ string, _ bool) (dto.TableEditability, error) {
	return dto.TableEditability{}, nil
}
func (c *mysqlSimConn) FetchCompletionCatalog(_ context.Context, _, _ string) (*dto.CompletionCatalog, error) {
	return nil, nil
}

func newPGSimFixture(t *testing.T, serverVersion, defaultDB string, dbs ...string) (*service.QueryService, *service.SchemaService, *session.Manager, *pgSimOpener) {
	t.Helper()
	ctx := context.Background()
	tmpDir := t.TempDir()

	repo, err := repository.OpenLocalDB(filepath.Join(tmpDir, "app.db"))
	if err != nil {
		t.Fatalf("open local repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	for _, c := range []entity.ConnectionEntity{
		{ID: "pg-dev", Name: "PG Dev", Driver: string(dto.EnginePostgres), EnvColor: "green", ConfigJSON: "{}"},
		{ID: "pg-ro", Name: "PG RO", Driver: string(dto.EnginePostgres), EnvColor: "green", ReadOnly: true, ConfigJSON: "{}"},
	} {
		if err := repo.UpsertConnection(ctx, c); err != nil {
			t.Fatalf("save conn %s: %v", c.ID, err)
		}
	}

	opener := newPGSimOpener(serverVersion, dbs...)
	reg := driver.NewRegistry()
	reg.Register(opener)

	sessMgr := session.NewManager(reg, func(_ context.Context, connID, overrideDB string) (driver.ResolvedConfig, error) {
		db := overrideDB
		if db == "" {
			db = defaultDB
		}
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EnginePostgres,
			Database:     db,
			ReadOnly:     connID == "pg-ro",
		}, nil
	})
	t.Cleanup(sessMgr.CloseAll)

	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: filepath.Join(tmpDir, "results")})
	t.Cleanup(resMgr.CloseAll)

	return service.NewQueryService(sessMgr, resMgr, repo), service.NewSchemaService(sessMgr), sessMgr, opener
}

func TestPGDropDatabase_AutoReleasesTrackAMetaPoolAndFallsBackListDatabases(t *testing.T) {
	ctx := context.Background()
	// Even when defaultDB itself is the DB being dropped, Track A pool must be evicted and ListDatabases must fall back to postgres!
	qSvc, sSvc, _, _ := newPGSimFixture(t, "16.4", "blue_m2", "postgres", "blue_m2")

	// Simulate user clicking & expanding blue_m2 in Explorer (warms both default "" and explicit "blue_m2" metaPools).
	if _, err := sSvc.ListDatabases(ctx, "pg-dev"); err != nil {
		t.Fatalf("initial ListDatabases failed: %v", err)
	}
	if _, err := sSvc.ListSchemas(ctx, "pg-dev", "blue_m2"); err != nil {
		t.Fatalf("ListSchemas blue_m2 failed: %v", err)
	}

	// Execute DROP DATABASE from a query tab connected to postgres: must succeed on the FIRST attempt!
	resp, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID:        "tab-1",
		ExecID:       "e1",
		ConnectionID: "pg-dev",
		Database:     "postgres",
		SQL:          `DROP DATABASE "blue_m2";`,
		RunMode:      "all",
	})
	if err != nil {
		t.Fatalf("ExecuteSQL error: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("expected DROP DATABASE to succeed after auto-evicting Track A metaPools, got error: %+v", resp.Error)
	}
	if resp.SchemaChanges == nil || !resp.SchemaChanges.DatabasesChanged || len(resp.SchemaChanges.DroppedDatabases) != 1 || resp.SchemaChanges.DroppedDatabases[0] != "blue_m2" {
		t.Fatalf("unexpected SchemaChanges: %+v", resp.SchemaChanges)
	}

	// Subsequent ListDatabases must recover from 3D000 on dropped defaultDB by falling back to "postgres".
	dbs, err := sSvc.ListDatabases(ctx, "pg-dev")
	if err != nil {
		t.Fatalf("ListDatabases after dropping defaultDB failed: %v", err)
	}
	if len(dbs) != 1 || dbs[0].Name != "postgres" {
		t.Fatalf("expected only postgres remaining, got %+v", dbs)
	}
}

func TestPGDropDatabase_AmbiguousTargetNeverEvictsWrongDB(t *testing.T) {
	ctx := context.Background()
	// Innocent DB "u" exists alongside Unicode-escaped target!
	qSvc, sSvc, _, opener := newPGSimFixture(t, "16.4", "postgres", "postgres", "u", "foo")

	// Warm Track A metaPool on innocent DB "u".
	if _, err := sSvc.ListSchemas(ctx, "pg-dev", "u"); err != nil {
		t.Fatalf("warm u metaPool: %v", err)
	}
	if opener.openCount("u") != 1 {
		t.Fatalf("expected 1 open Track A conn on u, got %d", opener.openCount("u"))
	}

	// Simulate PG returning 55006 for DROP DATABASE U&"foo"
	opener.customExecErr = func(sql string) *dto.AppError {
		return &dto.AppError{
			Code:     dto.ErrInternal,
			Message:  `PostgreSQL 错误 (55006): database "foo" is being accessed by other users`,
			Detail:   "There are 1 other session using the database.",
			SQLState: "55006",
		}
	}

	resp, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-u", ExecID: "e1", ConnectionID: "pg-dev", Database: "postgres",
		SQL: `DROP DATABASE U&"foo";`, RunMode: "all",
	})
	if err != nil {
		t.Fatalf("ExecuteSQL error: %v", err)
	}
	// MUST NOT have evicted innocent DB "u"'s metaPool!
	if opener.openCount("u") != 1 {
		t.Fatalf("ambiguous U&\"foo\" must NOT close innocent DB 'u' metaPool, openCount(u)=%d", opener.openCount("u"))
	}
	// Original PG error must be returned verbatim.
	if resp.Error == nil || resp.Error.SQLState != "55006" {
		t.Fatalf("expected original SQLSTATE 55006 error, got %+v", resp.Error)
	}
}

func TestPGDropDatabase_OtherTabHeldReturnsRaw55006WithoutDisconnecting(t *testing.T) {
	ctx := context.Background()

	t.Run("evicts_track_a_only_and_leaves_other_tabs_intact", func(t *testing.T) {
		qSvc, sSvc, sessMgr, opener := newPGSimFixture(t, "16.4", "postgres", "postgres", "blue_m2")

		// Warm Track A metaPool on blue_m2 AND open two Track B query tabs (idle + in-tx).
		if _, err := sSvc.ListSchemas(ctx, "pg-dev", "blue_m2"); err != nil {
			t.Fatalf("warm blue_m2 metaPool: %v", err)
		}
		if _, err := qSvc.OpenTabSession(ctx, "tab-idle", "pg-dev", "blue_m2", "public"); err != nil {
			t.Fatalf("open tab-idle: %v", err)
		}
		if _, err := qSvc.OpenTabSession(ctx, "tab-tx", "pg-dev", "blue_m2", "public"); err != nil {
			t.Fatalf("open tab-tx: %v", err)
		}
		if _, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID: "tab-tx", ExecID: "e-tx", ConnectionID: "pg-dev", Database: "blue_m2", SQL: "BEGIN;", RunMode: "all",
		}); err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if opener.openCount("blue_m2") != 3 {
			t.Fatalf("expected 3 open conns on blue_m2 (1 Track A + 2 Track B), got %d", opener.openCount("blue_m2"))
		}

		// Plain DROP DATABASE evicts Track A metaPool ONLY, never disconnects other Track B tabs,
		// and returns PostgreSQL's original SQLSTATE 55006 error verbatim.
		resp, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID: "tab-main", ExecID: "e-drop", ConnectionID: "pg-dev", Database: "postgres", SQL: "DROP DATABASE blue_m2;", RunMode: "all",
		})
		if err != nil {
			t.Fatalf("ExecuteSQL error: %v", err)
		}
		if resp.Error == nil || resp.Error.SQLState != "55006" {
			t.Fatalf("expected raw SQLSTATE 55006 error, got %+v", resp.Error)
		}
		if resp.SchemaChanges != nil {
			t.Fatalf("failed DROP DATABASE must not emit SchemaChanges, got %+v", resp.SchemaChanges)
		}
		if opener.openCount("blue_m2") != 2 {
			t.Fatalf("expected Track A metaPool evicted leaving only 2 Track B tabs, got %d", opener.openCount("blue_m2"))
		}
		if tabIdle, _ := sessMgr.GetTabSession("tab-idle"); tabIdle.State() != dto.SessionAliveIdle {
			t.Fatalf("tab-idle must remain SessionAliveIdle, got %s", tabIdle.State())
		}
		if tabTx, _ := sessMgr.GetTabSession("tab-tx"); tabTx.State() != dto.SessionAliveInTx {
			t.Fatalf("tab-tx must remain SessionAliveInTx, got %s", tabTx.State())
		}
	})

	t.Run("dropping_current_tab_db_returns_raw_55006", func(t *testing.T) {
		qSvc, _, _, _ := newPGSimFixture(t, "16.4", "postgres", "postgres", "self_db")
		resp, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			TabID: "tab-self", ExecID: "e1", ConnectionID: "pg-dev", Database: "self_db", SQL: "DROP DATABASE self_db;", RunMode: "all",
		})
		if err != nil {
			t.Fatalf("ExecuteSQL error: %v", err)
		}
		if resp.Error == nil || resp.Error.SQLState != "55006" || !strings.Contains(resp.Error.Message, "cannot drop the currently open database") {
			t.Fatalf("expected raw 55006 cannot drop currently open database error, got %+v", resp.Error)
		}
	})
}

func TestPGTransactionalDDLAndSavepointRollback(t *testing.T) {
	ctx := context.Background()
	qSvc, _, _, opener := newPGSimFixture(t, "16.4", "postgres", "postgres")

	// 1. BEGIN + CREATE TABLE + SAVEPOINT + ROLLBACK TO SAVEPOINT -> stays InTx & keeps pending DDL until COMMIT!
	r1, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-tx", ExecID: "e1", ConnectionID: "pg-dev", Database: "postgres", Schema: "public",
		SQL: "BEGIN; CREATE TABLE keep_tbl (id int); SAVEPOINT sp1; CREATE TABLE scratch_tbl (id int); ROLLBACK TO SAVEPOINT sp1;", RunMode: "all",
	})
	if err != nil || r1.Error != nil {
		t.Fatalf("savepoint batch failed: err=%v respErr=%+v", err, r1.Error)
	}
	if r1.SessionState != dto.SessionAliveInTx {
		t.Fatalf("ROLLBACK TO SAVEPOINT must keep SessionAliveInTx, got %s", r1.SessionState)
	}
	if r1.SchemaChanges != nil {
		t.Fatalf("expected no SchemaChanges before COMMIT, got %+v", r1.SchemaChanges)
	}

	// 2. COMMIT -> transitions to idle AND flushes pending DDL!
	r2, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-tx", ExecID: "e2", ConnectionID: "pg-dev", Database: "postgres", Schema: "public",
		SQL: "COMMIT;", RunMode: "all",
	})
	if err != nil || r2.Error != nil {
		t.Fatalf("COMMIT failed: err=%v respErr=%+v", err, r2.Error)
	}
	if r2.SessionState != dto.SessionAliveIdle {
		t.Fatalf("expected SessionAliveIdle after COMMIT, got %s", r2.SessionState)
	}
	if r2.SchemaChanges == nil || len(r2.SchemaChanges.ChangedObjects) != 1 || r2.SchemaChanges.ChangedObjects[0].Database != "postgres" || r2.SchemaChanges.ChangedObjects[0].Schema != "public" {
		t.Fatalf("expected ChangedObjects=[postgres.public] on COMMIT after ROLLBACK TO SAVEPOINT, got %+v", r2.SchemaChanges)
	}

	// 3. Error inside tx (FailedTx) recovered by ROLLBACK TO sp1 -> restores InTx and COMMIT still flushes pre-savepoint DDL!
	opener.customExecErr = func(sql string) *dto.AppError {
		if strings.Contains(sql, "BOOM") {
			return &dto.AppError{Code: dto.ErrInternal, Message: "division by zero", SQLState: "22012"}
		}
		return nil
	}
	r3, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-tx", ExecID: "e3", ConnectionID: "pg-dev", Database: "postgres", Schema: "public",
		SQL: "BEGIN; CREATE TABLE survived_tbl (id int); SAVEPOINT sp2; SELECT BOOM;", RunMode: "all",
	})
	if err != nil || r3.Error == nil || r3.SessionState != dto.SessionAliveFailedTx {
		t.Fatalf("expected FailedTx after mid-tx error, err=%v state=%s", err, r3.SessionState)
	}
	opener.customExecErr = nil
	r4, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-tx", ExecID: "e4", ConnectionID: "pg-dev", Database: "postgres", Schema: "public",
		SQL: "ROLLBACK TO sp2;", RunMode: "all",
	})
	if err != nil || r4.Error != nil || r4.SessionState != dto.SessionAliveInTx {
		t.Fatalf("expected ROLLBACK TO sp2 to recover SessionAliveInTx, err=%v respErr=%+v state=%s", err, r4.Error, r4.SessionState)
	}
	r5, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-tx", ExecID: "e5", ConnectionID: "pg-dev", Database: "postgres", Schema: "public",
		SQL: "COMMIT;", RunMode: "all",
	})
	if err != nil || r5.Error != nil || r5.SchemaChanges == nil || len(r5.SchemaChanges.ChangedObjects) != 1 {
		t.Fatalf("expected COMMIT after savepoint recovery to emit SchemaChanges, err=%v resp=%+v", err, r5)
	}

	// 4. Full ROLLBACK discards pending DDL!
	r6, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-tx", ExecID: "e6", ConnectionID: "pg-dev", Database: "postgres", Schema: "public",
		SQL: "BEGIN; CREATE TABLE rolled_back_tbl (id int); ROLLBACK;", RunMode: "all",
	})
	if err != nil || r6.Error != nil || r6.SessionState != dto.SessionAliveIdle {
		t.Fatalf("full ROLLBACK failed: err=%v respErr=%+v state=%s", err, r6.Error, r6.SessionState)
	}
	if r6.SchemaChanges != nil {
		t.Fatalf("expected no SchemaChanges on full ROLLBACK, got %+v", r6.SchemaChanges)
	}
}

func TestMySQLCrossDBRenameTableAndDroppedDefaultDBListDatabasesFallback(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	repo, err := repository.OpenLocalDB(filepath.Join(tmpDir, "app.db"))
	if err != nil {
		t.Fatalf("open local repo: %v", err)
	}
	defer func() { _ = repo.Close() }()

	if err := repo.UpsertConnection(ctx, entity.ConnectionEntity{
		ID: "mysql-1", Name: "MySQL Dev", Driver: string(dto.EngineMySQL), EnvColor: "green", ConfigJSON: "{}",
	}); err != nil {
		t.Fatalf("save mysql conn: %v", err)
	}

	opener := newMySQLSimOpener("shop", "archive")
	reg := driver.NewRegistry()
	reg.Register(opener)

	sessMgr := session.NewManager(reg, func(_ context.Context, connID, overrideDB string) (driver.ResolvedConfig, error) {
		db := overrideDB
		if db == "" {
			// Connection's configured defaultDatabase is "shop".
			db = "shop"
		}
		return driver.ResolvedConfig{
			ConnectionID: connID,
			Engine:       dto.EngineMySQL,
			Database:     db,
		}, nil
	})
	defer sessMgr.CloseAll()

	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: filepath.Join(tmpDir, "results")})
	defer resMgr.CloseAll()

	qSvc := service.NewQueryService(sessMgr, resMgr, repo)
	sSvc := service.NewSchemaService(sessMgr)

	// 1. Cross-database RENAME TABLE reports BOTH source (shop) and destination (archive) in ChangedObjects!
	renameResp, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-m1", ExecID: "e1", ConnectionID: "mysql-1", Database: "shop",
		SQL: "RENAME TABLE shop.orders TO archive.orders_2025;", RunMode: "all",
	})
	if err != nil || renameResp.Error != nil {
		t.Fatalf("RENAME TABLE failed: err=%v respErr=%+v", err, renameResp.Error)
	}
	if renameResp.SchemaChanges == nil || len(renameResp.SchemaChanges.ChangedObjects) != 2 {
		t.Fatalf("expected 2 ChangedObjects (shop & archive), got %+v", renameResp.SchemaChanges)
	}
	gotDBs := map[string]bool{}
	for _, sc := range renameResp.SchemaChanges.ChangedObjects {
		gotDBs[sc.Database] = true
	}
	if !gotDBs["shop"] || !gotDBs["archive"] {
		t.Fatalf("expected both shop and archive in ChangedObjects, got %+v", renameResp.SchemaChanges.ChangedObjects)
	}

	// 2. Drop the configured defaultDatabase ("shop") and verify ListDatabases falls back to information_schema!
	if _, err := sSvc.ListDatabases(ctx, "mysql-1"); err != nil {
		t.Fatalf("initial ListDatabases failed: %v", err)
	}
	dropResp, err := qSvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		TabID: "tab-m1", ExecID: "e2", ConnectionID: "mysql-1", Database: "archive",
		SQL: "DROP DATABASE `shop`;", RunMode: "all",
	})
	if err != nil || dropResp.Error != nil {
		t.Fatalf("DROP DATABASE shop failed: err=%v respErr=%+v", err, dropResp.Error)
	}

	dbs, err := sSvc.ListDatabases(ctx, "mysql-1")
	if err != nil {
		t.Fatalf("MySQL ListDatabases after dropping defaultDatabase failed: %v", err)
	}
	foundArchive := false
	for _, d := range dbs {
		if d.Name == "shop" {
			t.Fatalf("dropped DB shop should not appear in ListDatabases: %+v", dbs)
		}
		if d.Name == "archive" {
			foundArchive = true
		}
	}
	if !foundArchive {
		t.Fatalf("expected archive in remaining DBs, got %+v", dbs)
	}
}
