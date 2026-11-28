package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"

	"hesper/internal/changeset"
	"hesper/internal/driver"
	mysqldrv "hesper/internal/driver/mysql"
	pgdrv "hesper/internal/driver/postgres"
	"hesper/internal/dto"
	"hesper/internal/repository"
	"hesper/internal/resultbuf"
	"hesper/internal/secret"
	"hesper/internal/service"
	"hesper/internal/session"
	"hesper/internal/tunnel"
)

type realGridStack struct {
	connSvc   *service.ConnectionService
	schemaSvc *service.SchemaService
	querySvc  *service.QueryService
	changeSvc *service.ChangeService
	sessMgr   *session.Manager
	tunnelMgr *tunnel.Manager
	planStore *changeset.PlanStore
	repo      *repository.LocalDB
}

func newRealGridStack(t *testing.T) *realGridStack {
	t.Helper()
	dir := t.TempDir()
	localDB, err := repository.OpenLocalDB(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatalf("open local app.db: %v", err)
	}
	t.Cleanup(func() { _ = localDB.Close() })

	reg := driver.NewRegistry()
	reg.Register(mysqldrv.NewOpener(dto.EngineMySQL))
	reg.Register(mysqldrv.NewOpener(dto.EngineMariaDB))
	reg.Register(pgdrv.NewOpener())

	resMgr := resultbuf.NewManager(resultbuf.Options{SpillDir: filepath.Join(dir, "spill")})
	t.Cleanup(func() { resMgr.CloseAll() })

	connSvc := service.NewConnectionService(localDB, secret.NewStore(dir), nil, resMgr)
	sessMgr := session.NewManager(reg, connSvc.ResolveConfig)
	tunnelMgr := tunnel.NewManager(localDB, sessMgr.MarkConnectionLost)
	connSvc.SetSessionManager(sessMgr)
	connSvc.SetTunnelManager(tunnelMgr)
	t.Cleanup(func() {
		sessMgr.CloseAll()
		tunnelMgr.CloseAll()
	})

	planStore := changeset.NewPlanStore(changeset.PlanStoreOptions{})
	changeSvc := service.NewChangeService(sessMgr, localDB, planStore)
	changeSvc.SetSSHSecretPreparer(connSvc.PrepareRuntimeSSHCredential)

	querySvc := service.NewQueryService(sessMgr, resMgr, localDB)
	querySvc.SetSSHSecretPreparer(connSvc.PrepareRuntimeSSHCredential)

	return &realGridStack{
		connSvc:   connSvc,
		schemaSvc: service.NewSchemaService(sessMgr),
		querySvc:  querySvc,
		changeSvc: changeSvc,
		sessMgr:   sessMgr,
		tunnelMgr: tunnelMgr,
		planStore: planStore,
		repo:      localDB,
	}
}

type proxyAction int

const (
	proxyActionNone proxyAction = iota
	proxyActionDropCommit
	proxyActionStallCommit
	proxyActionStallRollback
)

// commitDropProxy forwards TCP traffic to a real database and, when armed, intercepts either:
//   - COMMIT drop: forwards COMMIT, sends 1 partial response byte, and drops the client TCP socket.
//   - COMMIT stall: forwards COMMIT, invokes onTrigger, and stalls server responses while keeping the socket open.
//   - ROLLBACK stall: forwards ROLLBACK and stalls server responses while keeping the socket open.
type commitDropProxy struct {
	host          string
	port          int
	backend       string
	ln            net.Listener
	mu            sync.Mutex
	action        proxyAction
	onTrigger     func()
	activeConn    []net.Conn
	stallTimedOut atomic.Bool
}

func startCommitDropProxy(t *testing.T, backendAddr string) *commitDropProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	px := &commitDropProxy{
		host:    h,
		port:    p,
		backend: backendAddr,
		ln:      ln,
	}
	t.Cleanup(func() { px.Close() })

	go func() {
		for {
			clientConn, err := ln.Accept()
			if err != nil {
				return
			}
			go px.handleConn(clientConn)
		}
	}()
	return px
}

func (p *commitDropProxy) ArmNextCommit() {
	p.ArmNextCommitWith(nil)
}

func (p *commitDropProxy) ArmNextCommitWith(fn func()) {
	p.mu.Lock()
	p.action = proxyActionDropCommit
	p.onTrigger = fn
	p.mu.Unlock()
}

func (p *commitDropProxy) ArmNextCommitStall(fn func()) {
	p.stallTimedOut.Store(false)
	p.mu.Lock()
	p.action = proxyActionStallCommit
	p.onTrigger = fn
	p.mu.Unlock()
}

func (p *commitDropProxy) ArmNextRollbackStall() {
	p.stallTimedOut.Store(false)
	p.mu.Lock()
	p.action = proxyActionStallRollback
	p.onTrigger = nil
	p.mu.Unlock()
}

func (p *commitDropProxy) StallTimedOut() bool {
	return p.stallTimedOut.Load()
}

func (p *commitDropProxy) consumeAction(chunk []byte) (proxyAction, func()) {
	isCommit := isWireCommitPacket(chunk)
	isRollback := isWireRollbackPacket(chunk)
	if !isCommit && !isRollback {
		return proxyActionNone, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.action {
	case proxyActionDropCommit, proxyActionStallCommit:
		if !isCommit {
			return proxyActionNone, nil
		}
		act := p.action
		cb := p.onTrigger
		p.action = proxyActionNone
		p.onTrigger = nil
		return act, cb
	case proxyActionStallRollback:
		if !isRollback {
			return proxyActionNone, nil
		}
		p.action = proxyActionNone
		p.onTrigger = nil
		return proxyActionStallRollback, nil
	default:
		return proxyActionNone, nil
	}
}

func (p *commitDropProxy) Close() {
	_ = p.ln.Close()
	p.mu.Lock()
	for _, c := range p.activeConn {
		_ = c.Close()
	}
	p.activeConn = nil
	p.mu.Unlock()
}

func (p *commitDropProxy) track(c net.Conn) {
	p.mu.Lock()
	p.activeConn = append(p.activeConn, c)
	p.mu.Unlock()
}

func isWireCommitPacket(chunk []byte) bool {
	// MySQL/MariaDB COM_QUERY "COMMIT": 3-byte len (0x07 0x00 0x00), 1-byte seq (0x00), 0x03 ('COM_QUERY'), "COMMIT"
	if bytes.Contains(bytes.ToUpper(chunk), []byte("\x03COMMIT")) {
		return true
	}
	// PostgreSQL Simple Query 'Q' or Parse 'P' sending "commit" / "COMMIT" terminated by NUL byte
	up := bytes.ToUpper(chunk)
	if bytes.Contains(up, []byte("COMMIT\x00")) || bytes.Contains(up, []byte("COMMIT;\x00")) {
		return true
	}
	return false
}

func isWireRollbackPacket(chunk []byte) bool {
	if bytes.Contains(bytes.ToUpper(chunk), []byte("\x03ROLLBACK")) {
		return true
	}
	up := bytes.ToUpper(chunk)
	if bytes.Contains(up, []byte("ROLLBACK\x00")) || bytes.Contains(up, []byte("ROLLBACK;\x00")) {
		return true
	}
	return false
}

func (p *commitDropProxy) handleConn(clientConn net.Conn) {
	serverConn, err := net.DialTimeout("tcp", p.backend, 5*time.Second)
	if err != nil {
		_ = clientConn.Close()
		return
	}
	p.track(clientConn)
	p.track(serverConn)

	dropCh := make(chan struct{}, 1)
	var stalling atomic.Bool
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = clientConn.Close()
			_ = serverConn.Close()
		})
	}

	// Client -> Server
	go func() {
		defer closeBoth()
		buf := make([]byte, 16*1024)
		for {
			n, rerr := clientConn.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				act, cb := p.consumeAction(chunk)
				if act == proxyActionStallCommit || act == proxyActionStallRollback {
					// Enter blackhole mode BEFORE forwarding command to backend so fast OK replies cannot leak.
					stalling.Store(true)
				}
				if _, werr := serverConn.Write(chunk); werr != nil {
					return
				}
				if act == proxyActionDropCommit {
					if cb != nil {
						cb()
					}
					select {
					case dropCh <- struct{}{}:
					default:
					}
					return
				}
				if act == proxyActionStallCommit || act == proxyActionStallRollback {
					if cb != nil {
						cb()
					}
					// Independent 3s watchdog: if client cancellation/timeout fails to unblock on its own,
					// force-close the socket and set stallTimedOut so the test fails fast instead of hanging!
					_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
					for {
						if _, derr := clientConn.Read(buf); derr != nil {
							var ne net.Error
							if errors.As(derr, &ne) && ne.Timeout() {
								p.stallTimedOut.Store(true)
							}
							return
						}
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Server -> Client
	buf := make([]byte, 16*1024)
	for {
		select {
		case <-dropCh:
			// Read 1 byte from server (or synthesize 1 partial header byte) and immediately close socket!
			_ = serverConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			one := make([]byte, 1)
			if n, _ := serverConn.Read(one); n == 1 {
				_, _ = clientConn.Write(one)
			} else {
				_, _ = clientConn.Write([]byte{0x01})
			}
			closeBoth()
			return
		default:
		}
		_ = serverConn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		n, rerr := serverConn.Read(buf)
		if n > 0 {
			if stalling.Load() {
				// Swallow server reply while keeping clientConn open to simulate a hung server.
				continue
			}
			select {
			case <-dropCh:
				// Write ONLY 1 partial byte of the server response, then drop immediately!
				_, _ = clientConn.Write(buf[:1])
				closeBoth()
				return
			default:
				if _, werr := clientConn.Write(buf[:n]); werr != nil {
					closeBoth()
					return
				}
			}
		}
		if rerr != nil {
			var ne net.Error
			if errors.As(rerr, &ne) && ne.Timeout() {
				continue
			}
			if !errors.Is(rerr, io.EOF) {
				closeBoth()
				return
			}
			closeBoth()
			return
		}
	}
}

func TestRealDBGridWrite_MySQL84AndMariaDB114(t *testing.T) {
	certDir := requireRealDBsAndCerts(t)

	for _, target := range realDBTargets() {
		if target.engine != dto.EngineMySQL && target.engine != dto.EngineMariaDB {
			continue
		}
		target := target
		t.Run(target.name, func(t *testing.T) {
			ctx := context.Background()
			stack := newRealGridStack(t)

			dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/sqlm_write_test?parseTime=false&multiStatements=false",
				target.rwUser, target.rwPass, target.host, target.port)
			adminDB, err := sql.Open("mysql", dsn)
			if err != nil {
				t.Fatalf("open admin mysql: %v", err)
			}
			// Register Close FIRST so LIFO t.Cleanup executes all DROP statements BEFORE closing adminDB!
			t.Cleanup(func() { _ = adminDB.Close() })

			ciCollation := "utf8mb4_0900_ai_ci"
			if target.engine == dto.EngineMariaDB {
				ciCollation = "utf8mb4_general_ci"
			}

			suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
			tblMain := "p2a_main_" + suffix
			tblAudit := "p2a_audit_" + suffix
			trgName := "trg_p2a_" + suffix
			tblMyISAM := "p2a_myisam_" + suffix
			tblSwap := "p2a_swap_" + suffix
			tblFuncIdx := "p2a_funcidx_" + suffix

			t.Cleanup(func() {
				for _, q := range []string{
					fmt.Sprintf("DROP TRIGGER IF EXISTS `%s`", trgName),
					fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tblMain),
					fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tblAudit),
					fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tblMyISAM),
					fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tblSwap),
					fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tblFuncIdx),
				} {
					if _, derr := adminDB.Exec(q); derr != nil {
						t.Errorf("cleanup %q failed: %v", q, derr)
					}
				}
			})

			// Create test tables & AFTER UPDATE trigger (including latin1 column with 'é' to verify cross-charset safety).
			ddls := []string{
				fmt.Sprintf("CREATE TABLE `%s` (cnt INT NOT NULL) ENGINE=InnoDB", tblAudit),
				fmt.Sprintf("INSERT INTO `%s` VALUES (0)", tblAudit),
				fmt.Sprintf(`CREATE TABLE %s (
					id BIGINT PRIMARY KEY,
					big_u BIGINT UNSIGNED NULL,
					ci_name VARCHAR(64) COLLATE %s NULL,
					bin_code VARCHAR(64) COLLATE utf8mb4_bin NULL,
					latin1_txt VARCHAR(64) CHARACTER SET latin1 COLLATE latin1_swedish_ci NULL,
					note VARCHAR(128) NULL,
					amt DECIMAL(38,10) NULL,
					v_gen VARCHAR(128) GENERATED ALWAYS AS (CONCAT('v:', IFNULL(ci_name, ''))) VIRTUAL,
					s_gen INT GENERATED ALWAYS AS (CHAR_LENGTH(IFNULL(ci_name, ''))) STORED
				) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`, "`"+tblMain+"`", ciCollation),
				fmt.Sprintf("CREATE TRIGGER `%s` AFTER UPDATE ON `%s` FOR EACH ROW UPDATE `%s` SET cnt = cnt + 1", trgName, tblMain, tblAudit),
				fmt.Sprintf(`INSERT INTO %s (id, big_u, ci_name, bin_code, latin1_txt, note, amt) VALUES
					(9223372036854775807, 18446744073709551615, 'Alice', 'a', 'é', NULL, '1234567890123456789012345678.1234567890')`, "`"+tblMain+"`"),
				fmt.Sprintf("CREATE TABLE `%s` (id INT PRIMARY KEY, val VARCHAR(32)) ENGINE=MyISAM", tblMyISAM),
				fmt.Sprintf("CREATE TABLE `%s` (id INT PRIMARY KEY, code VARCHAR(32) NOT NULL UNIQUE) ENGINE=InnoDB", tblSwap),
				fmt.Sprintf("INSERT INTO `%s` (id, code) VALUES (1, 'a'), (2, 'b')", tblSwap),
			}
			for _, q := range ddls {
				if _, err := adminDB.ExecContext(ctx, q); err != nil {
					t.Fatalf("setup ddl %q failed: %v", q, err)
				}
			}

			// Functional / expression unique index on MySQL 8.4 (MariaDB uses generated column syntax, MySQL 8 uses ((LOWER(name)))).
			if target.engine == dto.EngineMySQL {
				if _, err := adminDB.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s (
					name VARCHAR(64) NOT NULL,
					val VARCHAR(64) NULL,
					UNIQUE KEY uq_lower_name ((LOWER(name)))
				) ENGINE=InnoDB`, "`"+tblFuncIdx+"`")); err != nil {
					t.Fatalf("create functional index table: %v", err)
				}
			}

			conn, err := stack.connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
				Name:     target.name + " Write",
				Driver:   target.engine,
				EnvColor: "green",
				Password: target.rwPass,
				Params: dto.ConnectionParams{
					Host:            target.host,
					Port:            target.port,
					Username:        target.rwUser,
					DefaultDatabase: "sqlm_write_test",
				},
			})
			if err != nil {
				t.Fatalf("save conn: %v", err)
			}

			// 1. Verify VIRTUAL / STORED generated columns + high-precision BIGINT / BIGINT UNSIGNED / DECIMAL(38,10).
			detail, err := stack.schemaSvc.GetTableDetail(ctx, conn.ID, "sqlm_write_test", "", tblMain)
			if err != nil {
				t.Fatalf("GetTableDetail: %v", err)
			}
			cols := make(map[string]dto.ColumnDetail, len(detail.Columns))
			for _, c := range detail.Columns {
				cols[c.Name] = c
			}
			if cols["v_gen"].GeneratedKind != "virtual" || cols["v_gen"].Insertable || cols["v_gen"].Updatable {
				t.Fatalf("v_gen metadata mismatch: %+v", cols["v_gen"])
			}
			if cols["s_gen"].GeneratedKind != "stored" || cols["s_gen"].Insertable || cols["s_gen"].Updatable {
				t.Fatalf("s_gen metadata mismatch: %+v", cols["s_gen"])
			}

			// Explicit write to generated col rejected; omitted INSERT with high-precision values succeeds.
			_, err = stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Inserts: []dto.GridRowInsert{{RowKey: "bad-gen", Values: map[string]dto.NewCellValue{"v_gen": {Mode: dto.CellInputLiteral, Text: "x"}}}},
			})
			assertErrCode(t, err, dto.ErrInvalidArgument)

			prevIns, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Inserts: []dto.GridRowInsert{{
					RowKey: "ins-prec",
					Values: map[string]dto.NewCellValue{
						"id":       {Mode: dto.CellInputLiteral, Text: "9007199254740993"},
						"big_u":    {Mode: dto.CellInputLiteral, Text: "18446744073709551614"},
						"ci_name":  {Mode: dto.CellInputLiteral, Text: "Bob"},
						"bin_code": {Mode: dto.CellInputLiteral, Text: "b"},
						"amt":      {Mode: dto.CellInputLiteral, Text: "9876543210987654321098765432.0123456789"},
					},
				}},
			})
			if err != nil {
				t.Fatalf("preview insert high-precision: %v", err)
			}
			resIns, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevIns.PlanID})
			if err != nil || resIns.Outcome != dto.GridApplyCommitted {
				t.Fatalf("apply insert high-precision failed: resp=%+v err=%v", resIns, err)
			}
			var gotID, gotBigU, gotAmt string
			if err := adminDB.QueryRowContext(ctx, fmt.Sprintf("SELECT id, big_u, amt FROM `%s` WHERE id = 9007199254740993", tblMain)).Scan(&gotID, &gotBigU, &gotAmt); err != nil {
				t.Fatalf("read back high-precision row: %v", err)
			}
			if gotID != "9007199254740993" || gotBigU != "18446744073709551614" || gotAmt != "9876543210987654321098765432.0123456789" {
				t.Fatalf("high-precision mismatch: id=%s big_u=%s amt=%s", gotID, gotBigU, gotAmt)
			}

			keyMax := map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "9223372036854775807"}}

			// 2. No-op edit emits 0 write statements and leaves AFTER UPDATE trigger count == 0.
			prevNoop, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Updates: []dto.GridRowUpdate{{
					RowKey:         "noop-row",
					KeyValues:      keyMax,
					OriginalValues: map[string]dto.OriginalCellValue{"ci_name": {Kind: dto.KindString, Text: "Alice"}, "note": {Kind: dto.KindNull}},
					NewValues:      map[string]dto.NewCellValue{"ci_name": {Mode: dto.CellInputLiteral, Text: "Alice"}, "note": {Mode: dto.CellInputNull}},
				}},
			})
			if err != nil || len(prevNoop.Steps) != 0 {
				t.Fatalf("expected 0 steps for no-op, got %d err=%v", len(prevNoop.Steps), err)
			}
			resNoop, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevNoop.PlanID})
			if err != nil || resNoop.Outcome != dto.GridApplyCommitted || resNoop.AppliedCount != 0 {
				t.Fatalf("apply no-op failed: resp=%+v err=%v", resNoop, err)
			}
			var trgCnt int
			if err := adminDB.QueryRowContext(ctx, fmt.Sprintf("SELECT cnt FROM `%s`", tblAudit)).Scan(&trgCnt); err != nil {
				t.Fatalf("query audit trigger count: %v", err)
			}
			if trgCnt != 0 {
				t.Fatalf("expected AFTER UPDATE trigger count == 0 for no-op, got %d", trgCnt)
			}

			// 3. NULL original -> value update succeeds via IS NULL predicate.
			prevNull, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Updates: []dto.GridRowUpdate{{
					RowKey:         "null-row",
					KeyValues:      keyMax,
					OriginalValues: map[string]dto.OriginalCellValue{"note": {Kind: dto.KindNull}},
					NewValues:      map[string]dto.NewCellValue{"note": {Mode: dto.CellInputLiteral, Text: "filled"}},
				}},
			})
			if err != nil {
				t.Fatalf("preview NULL -> value: %v", err)
			}
			resNull, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevNull.PlanID})
			if err != nil || resNull.Outcome != dto.GridApplyCommitted {
				t.Fatalf("apply NULL -> value failed: resp=%+v err=%v", resNull, err)
			}

			// 4. Case-insensitive collation ('Alice' -> 'alice'), utf8mb4_bin PAD SPACE ('a' -> 'a '),
			//    AND cross-charset latin1 ('é' unchanged -> succeeds without false conflict; concurrent 'é' -> 'É' -> conflicts)!
			prevLatin1OK, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Updates: []dto.GridRowUpdate{{
					RowKey:         "latin1-ok",
					KeyValues:      keyMax,
					OriginalValues: map[string]dto.OriginalCellValue{"latin1_txt": {Kind: dto.KindString, Text: "é"}},
					NewValues:      map[string]dto.NewCellValue{"latin1_txt": {Mode: dto.CellInputLiteral, Text: "é_ok"}},
				}},
			})
			if err != nil {
				t.Fatalf("preview latin1_txt update: %v", err)
			}
			resLatin1OK, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevLatin1OK.PlanID})
			if err != nil || resLatin1OK.Outcome != dto.GridApplyCommitted || resLatin1OK.AppliedCount != 1 {
				t.Fatalf("latin1 'é' update MUST NOT false-conflict across utf8mb4 connection, got resp=%+v err=%v", resLatin1OK, err)
			}

			prevLatin1Conflict, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Updates: []dto.GridRowUpdate{{
					RowKey:         "latin1-conflict",
					KeyValues:      keyMax,
					OriginalValues: map[string]dto.OriginalCellValue{"latin1_txt": {Kind: dto.KindString, Text: "é_ok"}},
					NewValues:      map[string]dto.NewCellValue{"latin1_txt": {Mode: dto.CellInputLiteral, Text: "é_next"}},
				}},
			})
			if err != nil {
				t.Fatalf("preview latin1 conflict: %v", err)
			}
			if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("UPDATE `%s` SET latin1_txt = 'É_ok' WHERE id = 9223372036854775807", tblMain)); err != nil {
				t.Fatalf("concurrent latin1 case update: %v", err)
			}
			respLatin1Conflict, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevLatin1Conflict.PlanID})
			assertApplyOutcome(t, respLatin1Conflict, err, dto.GridApplyNotCommitted, dto.ErrConcurrencyConflict)

			prevCI, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Updates: []dto.GridRowUpdate{{
					RowKey:         "ci-conflict",
					KeyValues:      keyMax,
					OriginalValues: map[string]dto.OriginalCellValue{"ci_name": {Kind: dto.KindString, Text: "Alice"}},
					NewValues:      map[string]dto.NewCellValue{"ci_name": {Mode: dto.CellInputLiteral, Text: "Alicia"}},
				}},
			})
			if err != nil {
				t.Fatalf("preview ci_name: %v", err)
			}
			if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("UPDATE `%s` SET ci_name = 'alice' WHERE id = 9223372036854775807", tblMain)); err != nil {
				t.Fatalf("concurrent ci update: %v", err)
			}
			respCI, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevCI.PlanID})
			assertApplyOutcome(t, respCI, err, dto.GridApplyNotCommitted, dto.ErrConcurrencyConflict)
			if respCI.ConflictRowKey != "ci-conflict" {
				t.Fatalf("expected ConflictRowKey=ci-conflict, got %q", respCI.ConflictRowKey)
			}

			prevPad, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Updates: []dto.GridRowUpdate{{
					RowKey:         "pad-conflict",
					KeyValues:      keyMax,
					OriginalValues: map[string]dto.OriginalCellValue{"bin_code": {Kind: dto.KindString, Text: "a"}},
					NewValues:      map[string]dto.NewCellValue{"bin_code": {Mode: dto.CellInputLiteral, Text: "z"}},
				}},
			})
			if err != nil {
				t.Fatalf("preview bin_code: %v", err)
			}
			// Concurrent session changes 'a' -> 'a ' (utf8mb4_bin PAD SPACE considers 'a' = 'a ', CONVERT+BINARY catches it!).
			if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("UPDATE `%s` SET bin_code = 'a ' WHERE id = 9223372036854775807", tblMain)); err != nil {
				t.Fatalf("concurrent pad space update: %v", err)
			}
			respPad, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevPad.PlanID})
			assertApplyOutcome(t, respPad, err, dto.GridApplyNotCommitted, dto.ErrConcurrencyConflict)
			if respPad.ConflictRowKey != "pad-conflict" {
				t.Fatalf("expected ConflictRowKey=pad-conflict, got %q", respPad.ConflictRowKey)
			}

			// 5. MyISAM table with PK -> ErrBatchNonTx.
			_, err = stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMyISAM,
				Inserts: []dto.GridRowInsert{{RowKey: "m1", Values: map[string]dto.NewCellValue{"id": {Mode: dto.CellInputLiteral, Text: "1"}}}},
			})
			assertErrCode(t, err, dto.ErrBatchNonTx)

			// 6. MySQL 8.4 functional/expression unique index: GetTableDetail & InspectWriteSchema succeed and do NOT mark stable key.
			if target.engine == dto.EngineMySQL {
				fDetail, err := stack.schemaSvc.GetTableDetail(ctx, conn.ID, "sqlm_write_test", "", tblFuncIdx)
				if err != nil {
					t.Fatalf("GetTableDetail on functional index table failed: %v", err)
				}
				if fDetail.Editability.RowIdentity != dto.RowIdentityNone {
					t.Fatalf("functional unique index must NOT be treated as stable key, got %+v", fDetail.Editability)
				}
			}

			// 7. Two-row unique-key swap fails with duplicate error and rolls back ENTIRE batch!
			prevSwap, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblSwap,
				Updates: []dto.GridRowUpdate{
					{
						RowKey:         "swap-1",
						KeyValues:      map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1"}},
						OriginalValues: map[string]dto.OriginalCellValue{"code": {Kind: dto.KindString, Text: "a"}},
						NewValues:      map[string]dto.NewCellValue{"code": {Mode: dto.CellInputLiteral, Text: "b"}},
					},
					{
						RowKey:         "swap-2",
						KeyValues:      map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "2"}},
						OriginalValues: map[string]dto.OriginalCellValue{"code": {Kind: dto.KindString, Text: "b"}},
						NewValues:      map[string]dto.NewCellValue{"code": {Mode: dto.CellInputLiteral, Text: "a"}},
					},
				},
			})
			if err != nil {
				t.Fatalf("preview swap: %v", err)
			}
			swapResp, swapErr := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevSwap.PlanID})
			assertApplyOutcome(t, swapResp, swapErr, dto.GridApplyNotCommitted, "")
			if swapResp.Error == nil {
				t.Fatalf("expected structured duplicate error on swapResp, got %+v", swapResp)
			}
			var c1, c2 string
			if err := adminDB.QueryRowContext(ctx, fmt.Sprintf("SELECT code FROM `%s` WHERE id = 1", tblSwap)).Scan(&c1); err != nil {
				t.Fatalf("query swap id=1: %v", err)
			}
			if err := adminDB.QueryRowContext(ctx, fmt.Sprintf("SELECT code FROM `%s` WHERE id = 2", tblSwap)).Scan(&c2); err != nil {
				t.Fatalf("query swap id=2: %v", err)
			}
			if c1 != "a" || c2 != "b" {
				t.Fatalf("swap rollback failed: id=1(%q) id=2(%q)", c1, c2)
			}

			// 8. TOCTOU SchemaFingerprint: ALTER DECIMAL(38,10) -> DECIMAL(38,2) after Preview MUST fail ErrSchemaChanged!
			prevDrift, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: conn.ID, Database: "sqlm_write_test", Table: tblMain,
				Inserts: []dto.GridRowInsert{{
					RowKey: "drift-1",
					Values: map[string]dto.NewCellValue{
						"id":  {Mode: dto.CellInputLiteral, Text: "777"},
						"amt": {Mode: dto.CellInputLiteral, Text: "123.1234567890"},
					},
				}},
			})
			if err != nil {
				t.Fatalf("preview drift: %v", err)
			}
			if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("ALTER TABLE `%s` MODIFY COLUMN amt DECIMAL(38,2) NULL", tblMain)); err != nil {
				t.Fatalf("ALTER DECIMAL(38,10)->(38,2) failed: %v", err)
			}
			driftResp, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevDrift.PlanID})
			assertApplyOutcome(t, driftResp, err, dto.GridApplyNotCommitted, dto.ErrSchemaChanged)

			// 9. COMMIT partial-response disconnect via real proxy -> GridApplyUnknown (RetrySafe=false) & anti-replay!
			px := startCommitDropProxy(t, net.JoinHostPort(target.host, strconv.Itoa(target.port)))
			proxyConn, err := stack.connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
				Name:     target.name + " Proxy",
				Driver:   target.engine,
				EnvColor: "green",
				Password: target.rwPass,
				Params: dto.ConnectionParams{
					Host:            px.host,
					Port:            px.port,
					Username:        target.rwUser,
					DefaultDatabase: "sqlm_write_test",
				},
			})
			if err != nil {
				t.Fatalf("save proxy conn: %v", err)
			}
			prevDrop, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: proxyConn.ID, Database: "sqlm_write_test", Table: tblSwap,
				Inserts: []dto.GridRowInsert{{
					RowKey: "drop-1",
					Values: map[string]dto.NewCellValue{
						"id":   {Mode: dto.CellInputLiteral, Text: "99"},
						"code": {Mode: dto.CellInputLiteral, Text: "partial_drop_row"},
					},
				}},
			})
			if err != nil {
				t.Fatalf("preview drop: %v", err)
			}
			px.ArmNextCommit()
			dropResp, dropErr := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: proxyConn.ID, PlanID: prevDrop.PlanID})
			assertApplyOutcome(t, dropResp, dropErr, dto.GridApplyUnknown, dto.ErrCommitOutcomeUnknown)
			if dropResp.Error.RetrySafe {
				t.Fatalf("expected RetrySafe=false on partial-response COMMIT drop, got %+v", dropResp.Error)
			}

			// 9b. Hung COMMIT on direct connection MUST unblock promptly on CloseConnection() with GridApplyUnknown,
			//     and hung ROLLBACK MUST unblock promptly when rollback ctx times out!
			prevStallCommit, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
				ConnectionID: proxyConn.ID, Database: "sqlm_write_test", Table: tblSwap,
				Inserts: []dto.GridRowInsert{{
					RowKey: "stall-commit-1",
					Values: map[string]dto.NewCellValue{
						"id":   {Mode: dto.CellInputLiteral, Text: "98"},
						"code": {Mode: dto.CellInputLiteral, Text: "stalled_commit_row"},
					},
				}},
			})
			if err != nil {
				t.Fatalf("preview stall commit: %v", err)
			}
			px.ArmNextCommitStall(func() {
				time.AfterFunc(100*time.Millisecond, func() {
					stack.sessMgr.CloseConnection(proxyConn.ID)
				})
			})
			stallStart := time.Now()
			stallResp, stallErr := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{
				ConnectionID: proxyConn.ID,
				PlanID:       prevStallCommit.PlanID,
			})
			if px.StallTimedOut() {
				t.Fatalf("proxy watchdog fired after 3s: hung MySQL COMMIT failed to unblock on CloseConnection()")
			}
			if elapsed := time.Since(stallStart); elapsed > 2*time.Second {
				t.Fatalf("hung MySQL COMMIT did not unblock promptly on CloseConnection(): took %v", elapsed)
			}
			assertApplyOutcome(t, stallResp, stallErr, dto.GridApplyUnknown, dto.ErrCommitOutcomeUnknown)
			if stallResp.Error.RetrySafe {
				t.Fatalf("expected RetrySafe=false on canceled hung COMMIT after bytes sent, got %+v", stallResp.Error)
			}

			// Hung ROLLBACK bounded by rollback ctx deadline:
			rbLease, err := stack.sessMgr.AcquireOperation(ctx, proxyConn.ID, "sqlm_write_test", nil)
			if err != nil {
				t.Fatalf("acquire op for rollback stall test: %v", err)
			}
			rbTx, err := rbLease.Conn().(driver.TableWriteStarter).BeginTableWrite(rbLease.Context(), driver.WriteTarget{
				Database: "sqlm_write_test",
				Table:    tblSwap,
			}, false)
			if err != nil {
				rbLease.Release()
				t.Fatalf("BeginTableWrite for rollback stall test: %v", err)
			}
			px.ArmNextRollbackStall()
			rbShortCtx, rbCancel := context.WithTimeout(ctx, 200*time.Millisecond)
			rbStart := time.Now()
			rbErr := rbTx.Rollback(rbShortCtx)
			rbCancel()
			rbLease.Release()
			if px.StallTimedOut() {
				t.Fatalf("proxy watchdog fired after 3s: hung MySQL ROLLBACK failed to respect ctx deadline")
			}
			if rbErr == nil {
				t.Fatalf("expected hung ROLLBACK to fail with deadline timeout, got nil")
			}
			if elapsed := time.Since(rbStart); elapsed > 2*time.Second {
				t.Fatalf("hung MySQL ROLLBACK did not respect ctx timeout: took %v", elapsed)
			}

			// 9c. MySQL/MariaDB Rollback(canceledCtx) leaves tx retryable AND Commit(canceledCtx) rolls back
			//     releasing InnoDB row locks immediately while the lease stays open!
			lockLease, err := stack.sessMgr.AcquireOperation(ctx, conn.ID, "sqlm_write_test", nil)
			if err != nil {
				t.Fatalf("acquire op for mysql tx cancel-cleanup test: %v", err)
			}
			defer lockLease.Release()
			canceledCtx, cancelFn := context.WithCancel(ctx)
			cancelFn()

			txRetryRB, err := lockLease.Conn().(driver.TableWriteStarter).BeginTableWrite(lockLease.Context(), driver.WriteTarget{
				Database: "sqlm_write_test",
				Table:    tblSwap,
			}, false)
			if err != nil {
				t.Fatalf("BeginTableWrite for Rollback retry: %v", err)
			}
			if _, err := txRetryRB.Exec(ctx, driver.BoundStatement{
				SQL:  fmt.Sprintf("UPDATE `%s` SET code = 'locked_rb' WHERE id = 1", tblSwap),
				Args: nil,
			}); err != nil {
				t.Fatalf("Exec inside txRetryRB: %v", err)
			}
			if err := txRetryRB.Rollback(canceledCtx); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected MySQL Rollback(canceledCtx) to return context.Canceled, got %v", err)
			}
			// Retry with valid ctx MUST actually roll back (not silently return nil due to premature done=true).
			if err := txRetryRB.Rollback(ctx); err != nil {
				t.Fatalf("retry MySQL Rollback(ctx) failed: %v", err)
			}
			// Prove row id=1 lock is truly released while lockLease is STILL open!
			probeCtx1, probeCancel1 := context.WithTimeout(ctx, 2*time.Second)
			_, err = adminDB.ExecContext(probeCtx1, fmt.Sprintf("UPDATE `%s` SET code = 'a' WHERE id = 1", tblSwap))
			probeCancel1()
			if err != nil {
				t.Fatalf("row id=1 still locked after retry Rollback(ctx) while lease open: %v", err)
			}

			// Pre-canceled Commit(canceledCtx) MUST roll back row lock and return NotCommitted while lease stays open!
			txPreCancelCommit, err := lockLease.Conn().(driver.TableWriteStarter).BeginTableWrite(lockLease.Context(), driver.WriteTarget{
				Database: "sqlm_write_test",
				Table:    tblSwap,
			}, false)
			if err != nil {
				t.Fatalf("BeginTableWrite for pre-canceled Commit: %v", err)
			}
			if _, err := txPreCancelCommit.Exec(ctx, driver.BoundStatement{
				SQL: fmt.Sprintf("UPDATE `%s` SET code = 'locked_commit' WHERE id = 1", tblSwap),
			}); err != nil {
				t.Fatalf("Exec inside txPreCancelCommit: %v", err)
			}
			preCancelRes := txPreCancelCommit.Commit(canceledCtx)
			if preCancelRes.Outcome != driver.CommitOutcomeNotCommitted {
				t.Fatalf("expected MySQL pre-canceled Commit to return NotCommitted, got %+v", preCancelRes)
			}
			probeCtx2, probeCancel2 := context.WithTimeout(ctx, 2*time.Second)
			_, err = adminDB.ExecContext(probeCtx2, fmt.Sprintf("UPDATE `%s` SET code = 'a' WHERE id = 1", tblSwap))
			probeCancel2()
			if err != nil {
				t.Fatalf("row id=1 still locked after pre-canceled Commit while lease open: %v", err)
			}

			// 10. SSH + TLS grid write + SSH tunnel drop on COMMIT -> Unknown (MySQL 8.4).
			if target.engine == dto.EngineMySQL {
				sshSrv := startForwardingSSHServer(t, "s3cr3t", map[string]string{
					fmt.Sprintf("%s:%d", target.remoteAliasHost, target.remoteAliasPort): net.JoinHostPort(px.host, strconv.Itoa(px.port)),
				})
				sshTLSConn, err := stack.connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
					Name:              target.name + " SSH+TLS",
					Driver:            target.engine,
					EnvColor:          "green",
					Password:          target.rwPass,
					UpdatePassword:    true,
					SSHPassword:       "s3cr3t",
					UpdateSSHPassword: true,
					Params: dto.ConnectionParams{
						Host:            target.remoteAliasHost,
						Port:            target.remoteAliasPort,
						Username:        target.rwUser,
						DefaultDatabase: "sqlm_write_test",
						TLS: dto.TLSConfig{
							Mode:       "verify-ca",
							CACertPath: filepath.Join(certDir, "ca.crt"),
						},
						SSH: &dto.SSHConfig{
							Enabled:        true,
							Host:           sshSrv.host,
							Port:           sshSrv.port,
							Username:       "ops",
							AuthMethod:     "password",
							SaveCredential: true,
						},
					},
				})
				if err != nil {
					t.Fatalf("save ssh+tls conn: %v", err)
				}
				if err := stack.connSvc.TrustSSHHostKey(ctx, tunnel.NormalizeHostPort(sshSrv.host, sshSrv.port), sshSrv.algorithm, sshSrv.fingerprint); err != nil {
					t.Fatalf("trust ssh host key: %v", err)
				}
				prevSSH, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
					ConnectionID: sshTLSConn.ID, Database: "sqlm_write_test", Table: tblSwap,
					Inserts: []dto.GridRowInsert{{
						RowKey: "ssh-tls-1",
						Values: map[string]dto.NewCellValue{
							"id":   {Mode: dto.CellInputLiteral, Text: "101"},
							"code": {Mode: dto.CellInputLiteral, Text: "via_ssh_tls"},
						},
					}},
				})
				if err != nil {
					t.Fatalf("preview ssh+tls: %v", err)
				}
				resSSH, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: sshTLSConn.ID, PlanID: prevSSH.PlanID})
				if err != nil || resSSH.Outcome != dto.GridApplyCommitted {
					t.Fatalf("apply ssh+tls failed: resp=%+v err=%v", resSSH, err)
				}

				// Deterministic SSH tunnel kill on COMMIT -> GridApplyUnknown (RetrySafe=false).
				sshPlainConn, err := stack.connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
					Name:              target.name + " SSH Drop",
					Driver:            target.engine,
					EnvColor:          "green",
					Password:          target.rwPass,
					UpdatePassword:    true,
					SSHPassword:       "s3cr3t",
					UpdateSSHPassword: true,
					Params: dto.ConnectionParams{
						Host:            target.remoteAliasHost,
						Port:            target.remoteAliasPort,
						Username:        target.rwUser,
						DefaultDatabase: "sqlm_write_test",
						SSH: &dto.SSHConfig{
							Enabled:        true,
							Host:           sshSrv.host,
							Port:           sshSrv.port,
							Username:       "ops",
							AuthMethod:     "password",
							SaveCredential: true,
						},
					},
				})
				if err != nil {
					t.Fatalf("save ssh drop conn: %v", err)
				}
				prevSSHDrop, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
					ConnectionID: sshPlainConn.ID, Database: "sqlm_write_test", Table: tblSwap,
					Inserts: []dto.GridRowInsert{{
						RowKey: "ssh-drop-1",
						Values: map[string]dto.NewCellValue{
							"id":   {Mode: dto.CellInputLiteral, Text: "102"},
							"code": {Mode: dto.CellInputLiteral, Text: "via_ssh_drop"},
						},
					}},
				})
				if err != nil {
					t.Fatalf("preview ssh drop: %v", err)
				}
				px.ArmNextCommitWith(func() { sshSrv.KillAllConnections() })
				sshDropResp, sshDropErr := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{
					ConnectionID: sshPlainConn.ID,
					PlanID:       prevSSHDrop.PlanID,
				})
				assertApplyOutcome(t, sshDropResp, sshDropErr, dto.GridApplyUnknown, dto.ErrCommitOutcomeUnknown)
				if sshDropResp.Error.RetrySafe {
					t.Fatalf("expected RetrySafe=false on MySQL SSH tunnel drop during COMMIT, got %+v", sshDropResp.Error)
				}
			}
		})
	}
}

func TestRealDBGridWrite_Postgres16(t *testing.T) {
	certDir := requireRealDBsAndCerts(t)
	ctx := context.Background()

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
	// Register Close FIRST so LIFO t.Cleanup executes DROP SCHEMA BEFORE closing adminConn!
	t.Cleanup(func() { _ = adminConn.Close(context.Background()) })

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	schemaName := "p2a_pg_" + suffix
	t.Cleanup(func() {
		if _, derr := adminConn.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schemaName)); derr != nil {
			t.Errorf("cleanup DROP SCHEMA %s failed: %v", schemaName, derr)
		}
	})

	// Create isolated schema with INCLUDE keys, partial/expression unique indexes, json/jsonb,
	// non-deterministic ICU collation, IDENTITY ALWAYS/BY DEFAULT, STORED generated col,
	// DEFERRABLE unique constraint, BEFORE INSERT RETURN NULL trigger, and DEFERRABLE sleep trigger.
	setupSQLs := []string{
		fmt.Sprintf(`CREATE SCHEMA %s`, schemaName),
		fmt.Sprintf(`CREATE COLLATION %s.ci_icu (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`, schemaName),
		fmt.Sprintf(`CREATE TABLE %s.audit_cnt (cnt INT NOT NULL)`, schemaName),
		fmt.Sprintf(`INSERT INTO %s.audit_cnt VALUES (0)`, schemaName),
		fmt.Sprintf(`CREATE TABLE %s.t_main (
			id BIGINT GENERATED ALWAYS AS IDENTITY,
			extra TEXT NOT NULL DEFAULT 'inc',
			ci_name TEXT COLLATE %s.ci_icu NULL,
			j_raw JSON NULL,
			j_bin JSONB NULL,
			amt NUMERIC(38,10) NULL,
			s_gen TEXT GENERATED ALWAYS AS (COALESCE(extra, '') || '_gen') STORED,
			CONSTRAINT pk_t_main PRIMARY KEY (id) INCLUDE (extra)
		)`, schemaName, schemaName),
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.fn_audit() RETURNS trigger AS $$
			BEGIN UPDATE %s.audit_cnt SET cnt = cnt + 1; RETURN NEW; END;
		$$ LANGUAGE plpgsql`, schemaName, schemaName),
		fmt.Sprintf(`CREATE TRIGGER trg_main_upd AFTER UPDATE ON %s.t_main FOR EACH ROW EXECUTE FUNCTION %s.fn_audit()`, schemaName, schemaName),
		fmt.Sprintf(`INSERT INTO %s.t_main (extra, ci_name, j_raw, j_bin, amt) VALUES
			('e1', 'Alice', '{"a": 1, "b": 2}', '{"x":1}', 1234567890123456789012345678.1234567890)`, schemaName),

		fmt.Sprintf(`CREATE TABLE %s.t_uniq_inc (
			code TEXT NOT NULL,
			extra TEXT NULL,
			val TEXT NULL
		)`, schemaName),
		fmt.Sprintf(`CREATE UNIQUE INDEX uq_code_inc ON %s.t_uniq_inc (code) INCLUDE (extra)`, schemaName),

		fmt.Sprintf(`CREATE TABLE %s.t_partial_expr (
			code TEXT NOT NULL,
			active BOOLEAN NOT NULL DEFAULT true,
			val TEXT NULL
		)`, schemaName),
		fmt.Sprintf(`CREATE UNIQUE INDEX uq_partial ON %s.t_partial_expr (code) WHERE active`, schemaName),
		fmt.Sprintf(`CREATE UNIQUE INDEX uq_expr ON %s.t_partial_expr ((lower(code)))`, schemaName),

		fmt.Sprintf(`CREATE TABLE %s.t_deferred (
			id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			code TEXT NOT NULL,
			CONSTRAINT uq_def_code UNIQUE (code) DEFERRABLE INITIALLY DEFERRED
		)`, schemaName),
		fmt.Sprintf(`INSERT INTO %s.t_deferred (code) VALUES ('dup_key')`, schemaName),

		// Table with BEFORE INSERT trigger returning NULL when tag = 'skip_me' (0 affected rows).
		fmt.Sprintf(`CREATE TABLE %s.t_skip_insert (
			id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			tag TEXT NOT NULL
		)`, schemaName),
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.fn_skip_insert() RETURNS trigger AS $$
			BEGIN IF NEW.tag = 'skip_me' THEN RETURN NULL; END IF; RETURN NEW; END;
		$$ LANGUAGE plpgsql`, schemaName),
		fmt.Sprintf(`CREATE TRIGGER trg_skip_insert BEFORE INSERT ON %s.t_skip_insert
			FOR EACH ROW EXECUTE FUNCTION %s.fn_skip_insert()`, schemaName, schemaName),

		// Table with DEFERRABLE INITIALLY DEFERRED trigger that pauses 1.5s inside COMMIT
		// so the test can deterministically drop the encrypted SSH+mTLS tunnel mid-COMMIT!
		fmt.Sprintf(`CREATE TABLE %s.t_slow_commit (
			id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			tag TEXT NOT NULL
		)`, schemaName),
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.fn_slow_commit() RETURNS trigger AS $$
			BEGIN PERFORM pg_sleep(1.5); RETURN NEW; END;
		$$ LANGUAGE plpgsql`, schemaName),
		fmt.Sprintf(`CREATE CONSTRAINT TRIGGER trg_slow_commit AFTER INSERT ON %s.t_slow_commit
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s.fn_slow_commit()`, schemaName, schemaName),

		fmt.Sprintf(`GRANT ALL PRIVILEGES ON SCHEMA %s TO sqlm_rw, sqlm_mtls`, schemaName),
		fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA %s TO sqlm_rw, sqlm_mtls`, schemaName),
		fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA %s TO sqlm_rw, sqlm_mtls`, schemaName),
	}
	for _, q := range setupSQLs {
		if _, err := adminConn.Exec(ctx, q); err != nil {
			t.Fatalf("pg setup %q failed: %v", q, err)
		}
	}

	stack := newRealGridStack(t)
	conn, err := stack.connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name:     "PG16 Write",
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

	// 1. Verify PRIMARY KEY (...) INCLUDE (extra), UNIQUE (...) INCLUDE (extra), and partial/expression exclusions.
	mainDetail, err := stack.schemaSvc.GetTableDetail(ctx, conn.ID, "sqlm_demo", schemaName, "t_main")
	if err != nil {
		t.Fatalf("GetTableDetail t_main: %v", err)
	}
	if mainDetail.Editability.RowIdentity != dto.RowIdentityPrimaryKey || len(mainDetail.Editability.KeyColumns) != 1 || mainDetail.Editability.KeyColumns[0] != "id" {
		t.Fatalf("expected PK KeyColumns=[id] excluding INCLUDE(extra), got %+v", mainDetail.Editability)
	}
	colMap := make(map[string]dto.ColumnDetail, len(mainDetail.Columns))
	for _, c := range mainDetail.Columns {
		colMap[c.Name] = c
	}
	if colMap["id"].IdentityKind != "always" || colMap["id"].Insertable || colMap["id"].Updatable {
		t.Fatalf("IDENTITY ALWAYS metadata mismatch: %+v", colMap["id"])
	}
	if colMap["s_gen"].GeneratedKind != "stored" || colMap["s_gen"].Insertable || colMap["s_gen"].Updatable {
		t.Fatalf("STORED generated metadata mismatch: %+v", colMap["s_gen"])
	}

	uniqIncDetail, err := stack.schemaSvc.GetTableDetail(ctx, conn.ID, "sqlm_demo", schemaName, "t_uniq_inc")
	if err != nil {
		t.Fatalf("GetTableDetail t_uniq_inc: %v", err)
	}
	if uniqIncDetail.Editability.RowIdentity != dto.RowIdentityUniqueNotNull || len(uniqIncDetail.Editability.KeyColumns) != 1 || uniqIncDetail.Editability.KeyColumns[0] != "code" {
		t.Fatalf("expected UNIQUE NOT NULL KeyColumns=[code] excluding INCLUDE(extra), got %+v", uniqIncDetail.Editability)
	}

	partExprDetail, err := stack.schemaSvc.GetTableDetail(ctx, conn.ID, "sqlm_demo", schemaName, "t_partial_expr")
	if err != nil {
		t.Fatalf("GetTableDetail t_partial_expr: %v", err)
	}
	if partExprDetail.Editability.RowIdentity != dto.RowIdentityNone {
		t.Fatalf("partial and expression unique indexes MUST NOT qualify as stable key, got %+v", partExprDetail.Editability)
	}

	_, err = stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_main",
		Inserts: []dto.GridRowInsert{{RowKey: "bad-id", Values: map[string]dto.NewCellValue{"id": {Mode: dto.CellInputLiteral, Text: "99"}}}},
	})
	assertErrCode(t, err, dto.ErrInvalidArgument)

	_, err = stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_main",
		Inserts: []dto.GridRowInsert{{RowKey: "bad-gen", Values: map[string]dto.NewCellValue{"s_gen": {Mode: dto.CellInputLiteral, Text: "x"}}}},
	})
	assertErrCode(t, err, dto.ErrInvalidArgument)

	// 2. Verify json (::text), jsonb (= $n::jsonb), No-op trigger, and non-deterministic ICU collation conflict.
	browse, err := stack.querySvc.BrowseTable(ctx, dto.BrowseTableRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_main", Page: 1, PageSize: 20,
	})
	if err != nil || len(browse.Rows) != 1 {
		t.Fatalf("BrowseTable t_main: err=%v rows=%d", err, len(browse.Rows))
	}
	idxOf := make(map[string]int, len(browse.Columns))
	for i, c := range browse.Columns {
		idxOf[c.Name] = i
	}
	if browse.Columns[idxOf["j_raw"]].DatabaseType != "JSON" || browse.Columns[idxOf["j_bin"]].DatabaseType != "JSONB" {
		t.Fatalf("expected distinct JSON vs JSONB DatabaseType, got %s / %s",
			browse.Columns[idxOf["j_raw"]].DatabaseType, browse.Columns[idxOf["j_bin"]].DatabaseType)
	}
	r0 := browse.Rows[0]
	key1 := map[string]dto.OriginalCellValue{"id": cellToOrig(r0[idxOf["id"]])}

	prevNoop, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_main",
		Updates: []dto.GridRowUpdate{{
			RowKey:         "pg-noop",
			KeyValues:      key1,
			OriginalValues: map[string]dto.OriginalCellValue{"ci_name": cellToOrig(r0[idxOf["ci_name"]])},
			NewValues:      map[string]dto.NewCellValue{"ci_name": {Mode: dto.CellInputLiteral, Text: r0[idxOf["ci_name"]].Text}},
		}},
	})
	if err != nil || len(prevNoop.Steps) != 0 {
		t.Fatalf("expected 0 steps for PG no-op, got %d err=%v", len(prevNoop.Steps), err)
	}
	_, _ = stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevNoop.PlanID})
	var pgTrgCnt int
	if err := adminConn.QueryRow(ctx, fmt.Sprintf(`SELECT cnt FROM %s.audit_cnt`, schemaName)).Scan(&pgTrgCnt); err != nil {
		t.Fatalf("query PG audit_cnt: %v", err)
	}
	if pgTrgCnt != 0 {
		t.Fatalf("expected PG trigger count == 0 on no-op, got %d", pgTrgCnt)
	}

	prevJSON, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_main",
		Updates: []dto.GridRowUpdate{{
			RowKey:    "pg-json-ok",
			KeyValues: key1,
			OriginalValues: map[string]dto.OriginalCellValue{
				"j_raw": cellToOrig(r0[idxOf["j_raw"]]),
				"j_bin": cellToOrig(r0[idxOf["j_bin"]]),
			},
			NewValues: map[string]dto.NewCellValue{
				"j_raw": {Mode: dto.CellInputLiteral, Text: `{"a":2}`},
				"j_bin": {Mode: dto.CellInputLiteral, Text: `{"x":2}`},
			},
		}},
	})
	if err != nil {
		t.Fatalf("preview json/jsonb update: %v", err)
	}
	resJSON, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevJSON.PlanID})
	if err != nil || resJSON.Outcome != dto.GridApplyCommitted {
		t.Fatalf("apply json/jsonb update failed: resp=%+v err=%v", resJSON, err)
	}

	prevICU, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_main",
		Updates: []dto.GridRowUpdate{{
			RowKey:         "pg-icu-conflict",
			KeyValues:      key1,
			OriginalValues: map[string]dto.OriginalCellValue{"ci_name": {Kind: dto.KindString, Text: "Alice"}},
			NewValues:      map[string]dto.NewCellValue{"ci_name": {Mode: dto.CellInputLiteral, Text: "Alicia"}},
		}},
	})
	if err != nil {
		t.Fatalf("preview ICU update: %v", err)
	}
	if _, err := adminConn.Exec(ctx, fmt.Sprintf(`UPDATE %s.t_main SET ci_name = 'alice' WHERE id = 1`, schemaName)); err != nil {
		t.Fatalf("concurrent PG ICU update: %v", err)
	}
	respICU, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevICU.PlanID})
	assertApplyOutcome(t, respICU, err, dto.GridApplyNotCommitted, dto.ErrConcurrencyConflict)
	if respICU.ConflictRowKey != "pg-icu-conflict" {
		t.Fatalf("expected ConflictRowKey=pg-icu-conflict, got %q", respICU.ConflictRowKey)
	}

	// 2b. BEFORE INSERT trigger returning NULL (AffectedRows == 0) MUST roll back the entire batch!
	prevSkip, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_skip_insert",
		Inserts: []dto.GridRowInsert{
			{RowKey: "ins-keep", Values: map[string]dto.NewCellValue{"tag": {Mode: dto.CellInputLiteral, Text: "keep_me"}}},
			{RowKey: "ins-skip", Values: map[string]dto.NewCellValue{"tag": {Mode: dto.CellInputLiteral, Text: "skip_me"}}},
		},
	})
	if err != nil {
		t.Fatalf("preview skip insert: %v", err)
	}
	respSkip, errSkip := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevSkip.PlanID})
	assertApplyOutcome(t, respSkip, errSkip, dto.GridApplyNotCommitted, dto.ErrTxAborted)
	if respSkip.ConflictRowKey != "ins-skip" {
		t.Fatalf("expected ConflictRowKey=ins-skip, got %q", respSkip.ConflictRowKey)
	}
	var skipTblCnt int
	if err := adminConn.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.t_skip_insert`, schemaName)).Scan(&skipTblCnt); err != nil {
		t.Fatalf("query t_skip_insert count: %v", err)
	}
	if skipTblCnt != 0 {
		t.Fatalf("expected entire batch rolled back (0 rows) when BEFORE INSERT trigger skipped row 2, got %d rows", skipTblCnt)
	}

	// 3. DEFERRABLE INITIALLY DEFERRED unique constraint fires AT COMMIT -> NotCommitted (NOT Unknown!).
	prevDef, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: conn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_deferred",
		Inserts: []dto.GridRowInsert{{
			RowKey: "def-1",
			Values: map[string]dto.NewCellValue{
				"id":   {Mode: dto.CellInputLiteral, Text: "2"},
				"code": {Mode: dto.CellInputLiteral, Text: "dup_key"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("preview deferred insert: %v", err)
	}
	resDef, errDef := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{ConnectionID: conn.ID, PlanID: prevDef.PlanID})
	assertApplyOutcome(t, resDef, errDef, dto.GridApplyNotCommitted, "")
	if resDef.Error == nil || resDef.Error.Code == dto.ErrCommitOutcomeUnknown {
		t.Fatalf("DEFERRABLE constraint violation MUST be NotCommitted with non-Unknown error, got %+v", resDef)
	}

	// 4. SSH + mTLS end-to-end: challenge retry reusing planID + saveSecret without bumping configVer +
	//    exact-once INSERT + deterministic mid-COMMIT SSH+mTLS tunnel drop -> Unknown!
	px := startCommitDropProxy(t, net.JoinHostPort(pgTarget.host, strconv.Itoa(pgTarget.port)))
	sshSrv := startForwardingSSHServer(t, "pg-ssh-pw", map[string]string{
		"db.hesper.test:5432": net.JoinHostPort(px.host, strconv.Itoa(px.port)),
	})

	sshConn, err := stack.connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name:           "PG16 SSH+mTLS",
		Driver:         dto.EnginePostgres,
		EnvColor:       "green",
		Password:       pgTarget.mtlsPass,
		UpdatePassword: true,
		Params: dto.ConnectionParams{
			Host:            "db.hesper.test",
			Port:            5432,
			Username:        pgTarget.mtlsUser,
			DefaultDatabase: "sqlm_demo",
			TLS: dto.TLSConfig{
				Mode:       "verify-full",
				CACertPath: filepath.Join(certDir, "ca.crt"),
				CertPath:   filepath.Join(certDir, "client.crt"),
				KeyPath:    filepath.Join(certDir, "client.key"),
			},
			SSH: &dto.SSHConfig{
				Enabled:        true,
				Host:           sshSrv.host,
				Port:           sshSrv.port,
				Username:       "ops",
				AuthMethod:     "password",
				SaveCredential: false,
			},
		},
	})
	if err != nil {
		t.Fatalf("save pg ssh+mtls conn: %v", err)
	}
	if err := stack.connSvc.TrustSSHHostKey(ctx, tunnel.NormalizeHostPort(sshSrv.host, sshSrv.port), sshSrv.algorithm, sshSrv.fingerprint); err != nil {
		t.Fatalf("trust ssh host key: %v", err)
	}

	prevSSH, err := stack.changeSvc.PreviewGridChangesWithSSHSecret(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: sshConn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_deferred",
		Inserts: []dto.GridRowInsert{{
			RowKey: "ssh-ins-1",
			Values: map[string]dto.NewCellValue{
				"code": {Mode: dto.CellInputLiteral, Text: "ssh_once_only"},
			},
		}},
	}, "pg-ssh-pw", false)
	if err != nil {
		t.Fatalf("preview pg ssh+mtls: %v", err)
	}

	// Close ONLY the pooled SSH tunnel (LifecycleEpoch unchanged) so Apply hits SSHAuthPrompt via error channel.
	stack.tunnelMgr.CloseConnection(sshConn.ID)

	_, errPrompt := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{
		ConnectionID: sshConn.ID,
		PlanID:       prevSSH.PlanID,
	})
	var aePrompt *dto.AppError
	if !errors.As(errPrompt, &aePrompt) || aePrompt.Code != dto.ErrAuthFailed || aePrompt.SSHAuthPrompt == nil || !aePrompt.RetrySafe {
		t.Fatalf("expected ErrAuthFailed + SSHAuthPrompt (RetrySafe=true) on unauthenticated SSH Apply, got %v", errPrompt)
	}

	applySSH, err := stack.changeSvc.ApplyGridChangesWithSSHSecret(ctx, dto.ApplyGridChangesRequest{
		ConnectionID: sshConn.ID,
		PlanID:       prevSSH.PlanID,
	}, "pg-ssh-pw", true)
	if err != nil || applySSH.Outcome != dto.GridApplyCommitted || applySSH.AppliedCount != 1 {
		t.Fatalf("expected SSH challenge retry Apply to succeed, got resp=%+v err=%v", applySSH, err)
	}
	if !applySSH.CredentialSaved {
		t.Fatalf("expected CredentialSaved=true on saveSecret=true submission")
	}

	repeatSSH, err := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{
		ConnectionID: sshConn.ID,
		PlanID:       prevSSH.PlanID,
	})
	if err != nil || repeatSSH.Outcome != dto.GridApplyCommitted || repeatSSH.AppliedCount != 1 {
		t.Fatalf("expected cached Committed on repeat Apply, got resp=%+v err=%v", repeatSSH, err)
	}
	var sshRowCnt int
	if err := adminConn.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.t_deferred WHERE code = 'ssh_once_only'`, schemaName)).Scan(&sshRowCnt); err != nil {
		t.Fatalf("query ssh_once_only count: %v", err)
	}
	if sshRowCnt != 1 {
		t.Fatalf("expected exactly 1 row inserted for ssh_once_only across challenge retry & repeat Apply, got %d", sshRowCnt)
	}

	// 5. Partial-response COMMIT drop on PG plaintext proxy -> GridApplyUnknown (RetrySafe=false).
	pgProxyConn, err := stack.connSvc.SaveConnection(ctx, dto.SaveConnectionRequest{
		Name:     "PG16 Proxy Drop",
		Driver:   dto.EnginePostgres,
		EnvColor: "green",
		Password: pgTarget.rwPass,
		Params: dto.ConnectionParams{
			Host:            px.host,
			Port:            px.port,
			Username:        pgTarget.rwUser,
			DefaultDatabase: "sqlm_demo",
		},
	})
	if err != nil {
		t.Fatalf("save pg proxy conn: %v", err)
	}
	prevPGDrop, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: pgProxyConn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_deferred",
		Inserts: []dto.GridRowInsert{{
			RowKey: "pg-drop-1",
			Values: map[string]dto.NewCellValue{"code": {Mode: dto.CellInputLiteral, Text: "pg_partial_drop"}},
		}},
	})
	if err != nil {
		t.Fatalf("preview pg drop: %v", err)
	}
	px.ArmNextCommit()
	pgDropResp, pgDropErr := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{
		ConnectionID: pgProxyConn.ID,
		PlanID:       prevPGDrop.PlanID,
	})
	assertApplyOutcome(t, pgDropResp, pgDropErr, dto.GridApplyUnknown, dto.ErrCommitOutcomeUnknown)
	if pgDropResp.Error.RetrySafe {
		t.Fatalf("expected RetrySafe=false on PG partial-response COMMIT drop, got %+v", pgDropResp.Error)
	}

	// 6. Real encrypted SSH + mTLS tunnel drop MID-COMMIT (deterministic via DEFERRABLE sleep constraint trigger)!
	prevSlow, err := stack.changeSvc.PreviewGridChanges(ctx, dto.PreviewGridChangesRequest{
		ConnectionID: sshConn.ID, Database: "sqlm_demo", Schema: schemaName, Table: "t_slow_commit",
		Inserts: []dto.GridRowInsert{{
			RowKey: "pg-ssh-mtls-drop",
			Values: map[string]dto.NewCellValue{"tag": {Mode: dto.CellInputLiteral, Text: "mid_commit_drop"}},
		}},
	})
	if err != nil {
		t.Fatalf("preview slow commit over ssh+mtls: %v", err)
	}

	donePoll := make(chan struct{})
	go func() {
		defer close(donePoll)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var active int
			_ = adminConn.QueryRow(context.Background(), `
				SELECT COUNT(*)
				FROM pg_stat_activity
				WHERE usename = 'sqlm_mtls'
				  AND state = 'active'
				  AND lower(trim(query)) LIKE 'commit%'`).Scan(&active)
			if active > 0 {
				sshSrv.KillAllConnections()
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()

	slowResp, slowErr := stack.changeSvc.ApplyGridChanges(ctx, dto.ApplyGridChangesRequest{
		ConnectionID: sshConn.ID,
		PlanID:       prevSlow.PlanID,
	})
	<-donePoll
	assertApplyOutcome(t, slowResp, slowErr, dto.GridApplyUnknown, dto.ErrCommitOutcomeUnknown)
	if slowResp.Error.RetrySafe {
		t.Fatalf("expected RetrySafe=false on SSH+mTLS mid-COMMIT tunnel drop, got %+v", slowResp.Error)
	}
	_ = strings.TrimSpace("")
}
