package service_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	_ "modernc.org/sqlite"

	"hesper/internal/driver"
	"hesper/internal/driver/mysql"
	"hesper/internal/driver/postgres"
	"hesper/internal/driver/sqlite"
	"hesper/internal/dto"
	"hesper/internal/repository"
	"hesper/internal/resultbuf"
	"hesper/internal/secret"
	"hesper/internal/service"
	"hesper/internal/session"
)

func newQueryCompoundTestStack(t *testing.T) (*service.ConnectionService, *service.QueryService, *session.Manager) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	repo, err := repository.OpenLocalDB(dbPath)
	if err != nil {
		t.Fatalf("open local db: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	reg := driver.NewRegistry()
	reg.Register(mysql.NewOpener(dto.EngineMySQL))
	reg.Register(mysql.NewOpener(dto.EngineMariaDB))
	reg.Register(postgres.NewOpener())
	reg.Register(sqlite.NewOpener())

	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: filepath.Join(dir, "results")})
	t.Cleanup(func() { resMgr.CloseAll() })

	connSvc := service.NewConnectionService(repo, secret.NewFileOnlyStore(dir), nil, resMgr)
	sessMgr := session.NewManager(reg, connSvc.ResolveConfig)
	connSvc.SetSessionManager(sessMgr)
	t.Cleanup(func() { sessMgr.CloseAll() })

	querySvc := service.NewQueryService(sessMgr, resMgr, repo)
	return connSvc, querySvc, sessMgr
}

func TestRealSQLite354TriggerExecutionAndUnclosedGuard(t *testing.T) {
	ctx := context.Background()
	sqliteFile := filepath.Join(t.TempDir(), "trigger_test.db")
	rawDB, err := sql.Open("sqlite", sqliteFile)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })

	for _, ddl := range []string{
		`CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE audit_log (customer_id INTEGER)`,
		`CREATE TABLE stats (n INTEGER NOT NULL)`,
		`INSERT INTO stats (n) VALUES (0)`,
	} {
		if _, err := rawDB.Exec(ddl); err != nil {
			t.Fatalf("setup %q: %v", ddl, err)
		}
	}

	connSvc, querySvc, _ := newQueryCompoundTestStack(t)
	conn, err := connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name:     "SQLite Trigger Test",
		Driver:   dto.EngineSQLite,
		EnvColor: "green",
		Params:   dto.ConnectionParams{FilePath: sqliteFile},
	})
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	// 1. Deleted closing END: Ctrl+Enter on inner UPDATE MUST fail with ErrInvalidArgument and NEVER execute UPDATE!
	unclosedSQL := `CREATE TRIGGER trg_audit
AFTER INSERT ON customers
BEGIN
  INSERT INTO audit_log(customer_id) VALUES (new.id);
  UPDATE stats SET n = n + 1;
`
	updOffset := strings.Index(unclosedSQL, "UPDATE stats")
	unclosedResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		ConnectionID: conn.ID,
		TabID:        "tab-sq-trig",
		SQL:          unclosedSQL,
		RunMode:      "current",
		CursorOffset: updOffset,
	})
	if err != nil {
		t.Fatalf("ExecuteSQL transport err: %v", err)
	}
	if unclosedResp.Error == nil || unclosedResp.Error.Code != dto.ErrInvalidArgument {
		t.Fatalf("expected ErrInvalidArgument for unclosed trigger, got %+v", unclosedResp)
	}
	var statN int
	if err := rawDB.QueryRow(`SELECT n FROM stats`).Scan(&statN); err != nil {
		t.Fatalf("query stats: %v", err)
	}
	if statN != 0 {
		t.Fatalf("inner UPDATE MUST NOT have executed when closing END was missing, got stats.n=%d", statN)
	}

	// 2. Complete trigger followed by SELECT 1: Ctrl+Enter with cursor on inner UPDATE creates the whole trigger!
	wantTriggerDef := `CREATE TRIGGER trg_audit
AFTER INSERT ON customers
BEGIN
  INSERT INTO audit_log(customer_id) VALUES (new.id);
  UPDATE stats SET n = n + 1;
END`
	fullSQL := wantTriggerDef + ";\nSELECT 1;"
	updOffset2 := strings.Index(fullSQL, "UPDATE stats")
	createResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		ConnectionID: conn.ID,
		TabID:        "tab-sq-trig",
		SQL:          fullSQL,
		RunMode:      "current",
		CursorOffset: updOffset2,
	})
	if err != nil || createResp.Error != nil || len(createResp.Statements) != 1 {
		t.Fatalf("expected complete trigger creation, got resp=%+v err=%v", createResp, err)
	}
	if createResp.Statements[0].SQL != wantTriggerDef {
		t.Fatalf("expected Statements[0].SQL == full trigger definition, got %q", createResp.Statements[0].SQL)
	}
	// Trigger was DEFINED, not fired: stats.n must still be 0!
	if err := rawDB.QueryRow(`SELECT n FROM stats`).Scan(&statN); err != nil {
		t.Fatalf("query stats after trigger create: %v", err)
	}
	if statN != 0 {
		t.Fatalf("expected stats.n == 0 after CREATE TRIGGER, got %d", statN)
	}

	// Verify cursor on SELECT 1 executes ONLY SELECT 1.
	selOffset := strings.Index(fullSQL, "SELECT 1")
	selResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
		ConnectionID: conn.ID,
		TabID:        "tab-sq-trig",
		SQL:          fullSQL,
		RunMode:      "current",
		CursorOffset: selOffset,
	})
	if err != nil || selResp.Error != nil || len(selResp.Statements) != 1 || !selResp.Statements[0].IsResultSet {
		t.Fatalf("expected trailing SELECT 1 to run independently, got resp=%+v err=%v", selResp, err)
	}
	if selResp.Statements[0].SQL != "SELECT 1" {
		t.Fatalf("expected Statements[0].SQL == %q, got %q", "SELECT 1", selResp.Statements[0].SQL)
	}

	// Now insert a customer -> trigger fires and increments stats.n to 1 & logs audit row!
	if _, err := rawDB.Exec(`INSERT INTO customers (id, name) VALUES (10, 'Alice')`); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	var auditCnt int
	if err := rawDB.QueryRow(`SELECT n FROM stats`).Scan(&statN); err != nil {
		t.Fatalf("query stats after insert: %v", err)
	}
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE customer_id = 10`).Scan(&auditCnt); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if statN != 1 || auditCnt != 1 {
		t.Fatalf("expected trigger to fire once (stats.n=1, auditCnt=1), got stats.n=%d auditCnt=%d", statN, auditCnt)
	}

	// 3. EXPLAIN / EXPLAIN QUERY PLAN CREATE TRIGGER: Ctrl+Enter on inner UPDATE MUST return explain rows and NEVER execute UPDATE!
	for _, prefix := range []string{"EXPLAIN", "EXPLAIN QUERY PLAN"} {
		wantExpDef := fmt.Sprintf(`%s CREATE TRIGGER trg_explain_only
AFTER INSERT ON customers
BEGIN
  INSERT INTO audit_log(customer_id) VALUES (new.id);
  UPDATE stats SET n = n + 100;
END`, prefix)
		expSQL := wantExpDef + ";\nSELECT 1;"
		expResp, expErr := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			ConnectionID: conn.ID,
			TabID:        "tab-sq-trig",
			SQL:          expSQL,
			RunMode:      "current",
			CursorOffset: strings.Index(expSQL, "UPDATE stats"),
		})
		if expErr != nil || expResp.Error != nil || len(expResp.Statements) != 1 || !expResp.Statements[0].IsResultSet {
			t.Fatalf("%s CREATE TRIGGER failed: resp=%+v err=%v", prefix, expResp, expErr)
		}
		if expResp.Statements[0].SQL != wantExpDef {
			t.Fatalf("%s CREATE TRIGGER expected full SQL %q, got %q", prefix, wantExpDef, expResp.Statements[0].SQL)
		}
		if err := rawDB.QueryRow(`SELECT n FROM stats`).Scan(&statN); err != nil {
			t.Fatalf("query stats after %s CREATE TRIGGER: %v", prefix, err)
		}
		if statN != 1 {
			t.Fatalf("%s CREATE TRIGGER MUST NOT execute inner UPDATE, got stats.n=%d", prefix, statN)
		}
	}
}

func TestRealDBCompoundStatementsAndAndChain(t *testing.T) {
	_ = requireRealDBsAndCerts(t)
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)

	// --- 1. PostgreSQL 16: BEGIN ATOMIC function + END/ROLLBACK AND CHAIN session state ---
	t.Run("Postgres16_BeginAtomic_And_EndAndChain", func(t *testing.T) {
		var pgTarget realDBTarget
		for _, tgt := range realDBTargets() {
			if tgt.engine == dto.EnginePostgres {
				pgTarget = tgt
				break
			}
		}
		adminConn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:sqlm_pg_pw@%s:%d/sqlm_demo?sslmode=disable", pgTarget.host, pgTarget.port))
		if err != nil {
			t.Fatalf("connect pg superuser: %v", err)
		}
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = adminConn.Close(cctx)
		})

		schemaName := "cs_pg_" + suffix
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, derr := adminConn.Exec(cctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schemaName)); derr != nil {
				t.Errorf("cleanup DROP SCHEMA %s failed: %v", schemaName, derr)
			}
		})
		// Register query stack AFTER fixture-drop cleanup so LIFO closes all query tabs BEFORE DROP SCHEMA!
		connSvc, querySvc, _ := newQueryCompoundTestStack(t)

		for _, q := range []string{
			fmt.Sprintf(`CREATE SCHEMA %s`, schemaName),
			fmt.Sprintf(`CREATE TABLE %s.items (id INT PRIMARY KEY, tag TEXT)`, schemaName),
			fmt.Sprintf(`GRANT ALL PRIVILEGES ON SCHEMA %s TO sqlm_rw`, schemaName),
			fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA %s TO sqlm_rw`, schemaName),
		} {
			if _, err := adminConn.Exec(ctx, q); err != nil {
				t.Fatalf("pg setup %q: %v", q, err)
			}
		}

		conn, err := connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
			Name:     "PG Compound",
			Driver:   dto.EnginePostgres,
			EnvColor: "green",
			Password: pgTarget.rwPass,
			Params: dto.ConnectionParams{
				Host:            pgTarget.host,
				Port:            pgTarget.port,
				Username:        pgTarget.rwUser,
				DefaultDatabase: "sqlm_demo",
			},
		})
		if err != nil {
			t.Fatalf("save pg conn: %v", err)
		}

		// BEGIN ATOMIC function containing 'SELECT 1 AS end;' BEFORE inner INSERT:
		// Ctrl+Enter with cursor on inner INSERT MUST still execute the entire function (0 rows inserted!).
		wantFnDef := fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.add_item(v INT) RETURNS INT
LANGUAGE SQL
BEGIN ATOMIC
  SELECT 1 AS end;
  INSERT INTO %s.items (id, tag) VALUES (v, 'from_fn');
  SELECT CASE WHEN v > 0 THEN v ELSE 0 END;
END`, schemaName, schemaName)
		fnSQL := wantFnDef + ";\nSELECT 1;"
		insPos := strings.Index(fnSQL, "INSERT INTO")
		fnResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			ConnectionID: conn.ID,
			TabID:        "tab-pg-cs",
			SQL:          fnSQL,
			RunMode:      "current",
			CursorOffset: insPos,
		})
		if err != nil || fnResp.Error != nil || len(fnResp.Statements) != 1 {
			t.Fatalf("create PG BEGIN ATOMIC function failed: resp=%+v err=%v", fnResp, err)
		}
		if fnResp.Statements[0].SQL != wantFnDef {
			t.Fatalf("expected PG BEGIN ATOMIC Statements[0].SQL == full function definition, got %q", fnResp.Statements[0].SQL)
		}

		// Verify function works and table had 0 rows before invoking it.
		var cnt int
		if err := adminConn.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.items`, schemaName)).Scan(&cnt); err != nil {
			t.Fatalf("count before call: %v", err)
		}
		if cnt != 0 {
			t.Fatalf("expected 0 rows before calling function, got %d", cnt)
		}

		// Transaction chaining: BEGIN -> insert 1 -> END AND CHAIN (stays InTx!) -> insert 2 ->
		// ROLLBACK AND CHAIN (rolls back 2, stays InTx!) -> insert 3 -> END WORK AND NO CHAIN (Idle!).
		runStmt := func(sqlText string, wantState dto.SessionState) {
			t.Helper()
			r, rerr := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				ConnectionID: conn.ID, TabID: "tab-pg-cs", SQL: sqlText, RunMode: "all",
			})
			if rerr != nil || r.Error != nil {
				t.Fatalf("exec %q failed: resp=%+v err=%v", sqlText, r, rerr)
			}
			if r.SessionState != wantState {
				t.Fatalf("after %q expected SessionState=%s, got %s", sqlText, wantState, r.SessionState)
			}
		}
		idleChainResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
			ConnectionID: conn.ID, TabID: "tab-pg-cs", SQL: "END AND CHAIN;", RunMode: "all",
		})
		if err != nil || idleChainResp.Error == nil || idleChainResp.SessionState != dto.SessionAliveIdle {
			t.Fatalf("expected PG idle END AND CHAIN to error (25P01) while staying Idle, got resp=%+v err=%v", idleChainResp, err)
		}
		runStmt("BEGIN;", dto.SessionAliveInTx)
		runStmt(fmt.Sprintf("INSERT INTO %s.items (id, tag) VALUES (1, 'c1');", schemaName), dto.SessionAliveInTx)
		runStmt("END AND CHAIN;", dto.SessionAliveInTx)
		runStmt(fmt.Sprintf("INSERT INTO %s.items (id, tag) VALUES (2, 'rb2');", schemaName), dto.SessionAliveInTx)
		runStmt("ROLLBACK AND CHAIN;", dto.SessionAliveInTx)
		runStmt(fmt.Sprintf("INSERT INTO %s.items (id, tag) VALUES (3, 'c3');", schemaName), dto.SessionAliveInTx)
		runStmt("END WORK AND NO CHAIN;", dto.SessionAliveIdle)

		var ids []int
		rows, err := adminConn.Query(ctx, fmt.Sprintf(`SELECT id FROM %s.items ORDER BY id`, schemaName))
		if err != nil {
			t.Fatalf("query items: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan id: %v", err)
			}
			ids = append(ids, id)
		}
		if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
			t.Fatalf("expected committed rows [1 3] across END AND CHAIN / ROLLBACK AND CHAIN, got %v", ids)
		}
	})

	// --- 2. MySQL 8.4 & MariaDB 11.4: PROCEDURE without DELIMITER, executable comments, t.end, anonymous blocks, COMMIT AND CHAIN ---
	for _, target := range realDBTargets() {
		if target.engine != dto.EngineMySQL && target.engine != dto.EngineMariaDB {
			continue
		}
		target := target
		t.Run(target.name+"_Compound_And_CommitAndChain", func(t *testing.T) {
			adminDB, err := sql.Open("mysql", fmt.Sprintf("root:sqlm_root_pw@tcp(%s:%d)/sqlm_demo?parseTime=false", target.host, target.port))
			if err != nil {
				t.Fatalf("open root db: %v", err)
			}
			// Register Close FIRST, then bounded DROP, then query stack so LIFO runs:
			// 1. close query tabs/sessions -> 2. DROP fixtures -> 3. close adminDB.
			t.Cleanup(func() { _ = adminDB.Close() })

			tbl := "cs_tbl_" + suffix
			procName := "cs_proc_" + suffix
			procExecName := "cs_proc_exec_" + suffix
			procBtName := "cs_proc_bt_" + suffix
			eventName := "cs_ev_" + suffix
			t.Cleanup(func() {
				for _, q := range []string{
					fmt.Sprintf("DROP EVENT IF EXISTS `%s`", eventName),
					fmt.Sprintf("DROP PROCEDURE IF EXISTS `%s`", procBtName),
					fmt.Sprintf("DROP PROCEDURE IF EXISTS `%s`", procExecName),
					fmt.Sprintf("DROP PROCEDURE IF EXISTS `%s`", procName),
					fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tbl),
				} {
					cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_, derr := adminDB.ExecContext(cctx, q)
					cancel()
					if derr != nil {
						t.Errorf("cleanup %s failed: %v", q, derr)
					}
				}
			})
			connSvc, querySvc, _ := newQueryCompoundTestStack(t)

			if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("CREATE TABLE `%s` (id INT PRIMARY KEY, tag VARCHAR(32), `end` INT NOT NULL DEFAULT 0) ENGINE=InnoDB", tbl)); err != nil {
				t.Fatalf("create %s: %v", tbl, err)
			}

			conn, err := connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
				Name:     target.name + " Compound",
				Driver:   target.engine,
				EnvColor: "green",
				Password: target.rwPass,
				Params: dto.ConnectionParams{
					Host:            target.host,
					Port:            target.port,
					Username:        target.rwUser,
					DefaultDatabase: "sqlm_demo",
				},
			})
			if err != nil {
				t.Fatalf("save conn: %v", err)
			}

			// 1. CREATE PROCEDURE without DELIMITER via Ctrl+Enter with cursor on inner UPDATE -> creates procedure cleanly!
			wantProcDef := fmt.Sprintf(`CREATE PROCEDURE %s()
BEGIN
  INSERT INTO %s (id, tag) VALUES (10, 'p');
  UPDATE %s SET tag = 'p_upd' WHERE id = 10;
END`, procName, tbl, tbl)
			procSQL := wantProcDef + ";\nSELECT 1;"
			updPos := strings.Index(procSQL, "UPDATE ")
			pResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				ConnectionID: conn.ID,
				TabID:        "tab-my-cs-" + target.name,
				SQL:          procSQL,
				RunMode:      "current",
				CursorOffset: updPos,
			})
			if err != nil || pResp.Error != nil || len(pResp.Statements) != 1 {
				t.Fatalf("create procedure without DELIMITER failed: resp=%+v err=%v", pResp, err)
			}
			if pResp.Statements[0].SQL != wantProcDef {
				t.Fatalf("expected CREATE PROCEDURE Statements[0].SQL == full definition, got %q", pResp.Statements[0].SQL)
			}

			// 1b. CREATE /*!50020 DEFINER=CURRENT_USER()*/ PROCEDURE with qualified 't.end' column BEFORE inner UPDATE!
			wantProcExecDef := fmt.Sprintf(`CREATE /*!50020 DEFINER=CURRENT_USER()*/ PROCEDURE %s()
BEGIN
  SELECT t.end FROM %s t LIMIT 1;
  INSERT INTO %s (id, tag) VALUES (11, 'p2');
  UPDATE %s SET tag = 'p2_upd' WHERE id = 11;
END`, procExecName, tbl, tbl, tbl)
			procExecSQL := wantProcExecDef + ";\nSELECT 1;"
			pExecResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				ConnectionID: conn.ID,
				TabID:        "tab-my-cs-" + target.name,
				SQL:          procExecSQL,
				RunMode:      "current",
				CursorOffset: strings.Index(procExecSQL, "UPDATE "),
			})
			if err != nil || pExecResp.Error != nil || len(pExecResp.Statements) != 1 {
				t.Fatalf("create procedure with executable DEFINER comment and t.end failed: resp=%+v err=%v", pExecResp, err)
			}
			if pExecResp.Statements[0].SQL != wantProcExecDef {
				t.Fatalf("expected executable DEFINER PROCEDURE Statements[0].SQL == full definition, got %q", pExecResp.Statements[0].SQL)
			}

			// 1c. Outermost backtick-quoted block label on CREATE PROCEDURE (`blk`: BEGIN ... END `blk`)!
			wantProcBtDef := fmt.Sprintf("CREATE PROCEDURE %s()\n`blk`: BEGIN\n  INSERT INTO %s (id, tag) VALUES (12, 'bt');\n  UPDATE %s SET tag = 'bt_upd' WHERE id = 12;\nEND `blk`", procBtName, tbl, tbl)
			procBtSQL := wantProcBtDef + ";\nSELECT 1;"
			pBtResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				ConnectionID: conn.ID,
				TabID:        "tab-my-cs-" + target.name,
				SQL:          procBtSQL,
				RunMode:      "current",
				CursorOffset: strings.Index(procBtSQL, "UPDATE "),
			})
			if err != nil || pBtResp.Error != nil || len(pBtResp.Statements) != 1 {
				t.Fatalf("create backtick-labeled procedure failed: resp=%+v err=%v", pBtResp, err)
			}
			if pBtResp.Statements[0].SQL != wantProcBtDef {
				t.Fatalf("expected backtick-labeled PROCEDURE Statements[0].SQL == full definition, got %q", pBtResp.Statements[0].SQL)
			}

			// 1d. CREATE EVENT + ALTER EVENT ... DO BEGIN ... END without DELIMITER!
			createEvSQL := fmt.Sprintf("CREATE EVENT `%s` ON SCHEDULE EVERY 1 DAY DISABLE DO BEGIN INSERT INTO `%s` (id, tag) VALUES (13, 'ev1'); END;", eventName, tbl)
			createEvResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				ConnectionID: conn.ID,
				TabID:        "tab-my-cs-" + target.name,
				SQL:          createEvSQL,
				RunMode:      "all",
			})
			if err != nil || createEvResp.Error != nil || len(createEvResp.Statements) != 1 {
				t.Fatalf("CREATE EVENT ... DO BEGIN ... END failed: resp=%+v err=%v", createEvResp, err)
			}
			wantAlterEvDef := fmt.Sprintf(`ALTER EVENT %s DO
BEGIN
  INSERT INTO %s (id, tag) VALUES (14, 'ev2');
  UPDATE %s SET tag = 'ev2_upd' WHERE id = 14;
END`, eventName, tbl, tbl)
			alterEvSQL := wantAlterEvDef + ";\nSELECT 1;"
			evResp, err := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
				ConnectionID: conn.ID,
				TabID:        "tab-my-cs-" + target.name,
				SQL:          alterEvSQL,
				RunMode:      "current",
				CursorOffset: strings.Index(alterEvSQL, "UPDATE "),
			})
			if err != nil || evResp.Error != nil || len(evResp.Statements) != 1 {
				t.Fatalf("ALTER EVENT ... DO BEGIN ... END failed: resp=%+v err=%v", evResp, err)
			}
			if evResp.Statements[0].SQL != wantAlterEvDef {
				t.Fatalf("expected ALTER EVENT Statements[0].SQL == full definition, got %q", evResp.Statements[0].SQL)
			}

			var cnt int
			if err := adminDB.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`", tbl)).Scan(&cnt); err != nil {
				t.Fatalf("count %s: %v", tbl, err)
			}
			if cnt != 0 {
				t.Fatalf("expected 0 rows after CREATE PROCEDURE / ALTER EVENT definitions, got %d", cnt)
			}

			// 2. Non-BEGIN compound body (IF...END IF) & MariaDB standalone anonymous blocks -> explicitly rejected, 0 rows mutated!
			for _, badSQL := range []string{
				fmt.Sprintf("CREATE PROCEDURE %s_if(IN x INT) IF x > 0 THEN INSERT INTO %s (id, tag) VALUES (99, 'bad'); END IF;", procName, tbl),
				fmt.Sprintf("BEGIN NOT ATOMIC INSERT INTO %s (id, tag) VALUES (99, 'bad'); END;", tbl),
				fmt.Sprintf("IF 1 = 1 THEN INSERT INTO %s (id, tag) VALUES (99, 'bad'); END IF;", tbl),
				fmt.Sprintf("WHILE 0 DO INSERT INTO %s (id, tag) VALUES (99, 'bad'); END WHILE;", tbl),
			} {
				badResp, badErr := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
					ConnectionID: conn.ID,
					TabID:        "tab-my-cs-" + target.name,
					SQL:          badSQL,
					RunMode:      "current",
					CursorOffset: strings.Index(badSQL, "INSERT INTO"),
				})
				if badErr != nil {
					t.Fatalf("unexpected transport err: %v", badErr)
				}
				if badResp.Error == nil || badResp.Error.Code != dto.ErrInvalidArgument {
					t.Fatalf("expected ErrInvalidArgument for %q, got %+v", badSQL, badResp)
				}
			}
			if err := adminDB.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`", tbl)).Scan(&cnt); err != nil {
				t.Fatalf("count %s after rejected blocks: %v", tbl, err)
			}
			if cnt != 0 {
				t.Fatalf("rejected blocks MUST NOT mutate table, got %d rows", cnt)
			}

			// 3. BEGIN -> insert 1 -> COMMIT AND CHAIN (stays InTx!) -> insert 2 -> ROLLBACK (Idle, only row 1 kept!).
			runMy := func(sqlText string, wantState dto.SessionState) {
				t.Helper()
				r, rerr := querySvc.ExecuteSQL(ctx, dto.ExecuteSQLRequest{
					ConnectionID: conn.ID, TabID: "tab-my-cs-" + target.name, SQL: sqlText, RunMode: "all",
				})
				if rerr != nil || r.Error != nil {
					t.Fatalf("exec %q failed: resp=%+v err=%v", sqlText, r, rerr)
				}
				if r.SessionState != wantState {
					t.Fatalf("after %q expected SessionState=%s, got %s", sqlText, wantState, r.SessionState)
				}
			}
			runMy("BEGIN;", dto.SessionAliveInTx)
			runMy(fmt.Sprintf("INSERT INTO `%s` (id, tag) VALUES (1, 'c1');", tbl), dto.SessionAliveInTx)
			runMy("COMMIT AND CHAIN;", dto.SessionAliveInTx)
			runMy(fmt.Sprintf("INSERT INTO `%s` (id, tag) VALUES (2, 'rb2');", tbl), dto.SessionAliveInTx)
			runMy("ROLLBACK;", dto.SessionAliveIdle)

			if err := adminDB.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`", tbl)).Scan(&cnt); err != nil {
				t.Fatalf("count %s after COMMIT AND CHAIN: %v", tbl, err)
			}
			if cnt != 1 {
				t.Fatalf("expected exactly 1 committed row after COMMIT AND CHAIN + ROLLBACK, got %d", cnt)
			}
		})
	}
}
