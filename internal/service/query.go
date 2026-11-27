package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"hesper/internal/driver"
	"hesper/internal/dto"
	"hesper/internal/entity"
	"hesper/internal/repository"
	"hesper/internal/resultbuf"
	"hesper/internal/session"
	"hesper/internal/sqlsplit"
)

// QueryService orchestrates live table browsing (Track A), Execute-Once SQL execution (Track B),
// snapshot pagination (ResultBuffer), cancel, session lifecycle, and query history.
type QueryService struct {
	sessMgr          *session.Manager
	resMgr           *resultbuf.Manager
	repo             *repository.LocalDB
	prepareSSHSecret func(ctx context.Context, connID string, carrier *runtimeSSHSecret)
}

func NewQueryService(sessMgr *session.Manager, resMgr *resultbuf.Manager, repo *repository.LocalDB) *QueryService {
	return &QueryService{
		sessMgr: sessMgr,
		resMgr:  resMgr,
		repo:    repo,
	}
}

// SetSSHSecretPreparer registers ConnectionService.PrepareRuntimeSSHCredential so explicit saveSecret=true
// submissions persist even if an existing healthy TabSession is reused without invoking ResolveConfig.
func (s *QueryService) SetSSHSecretPreparer(fn func(ctx context.Context, connID string, carrier *runtimeSSHSecret)) {
	s.prepareSSHSecret = fn
}

// OpenTabSession establishes a dedicated Track B physical connection for a query tab.
func (s *QueryService) OpenTabSession(ctx context.Context, tabID, connID, db, schema string) (dto.TabSessionInfo, error) {
	return s.openTabSessionInternal(ctx, tabID, connID, db, schema, nil)
}

// OpenTabSessionWithSSHSecret opens a query tab session with a runtime SSH password/passphrase.
func (s *QueryService) OpenTabSessionWithSSHSecret(ctx context.Context, tabID, connID, db, schema, sshSecret string, saveSecret bool) (dto.TabSessionInfo, error) {
	reqCtx, carrier := withRuntimeSSHSecret(ctx, sshSecret, saveSecret)
	return s.openTabSessionInternal(reqCtx, tabID, connID, db, schema, carrier)
}

func (s *QueryService) openTabSessionInternal(ctx context.Context, tabID, connID, db, schema string, carrier *runtimeSSHSecret) (dto.TabSessionInfo, error) {
	if s.prepareSSHSecret != nil {
		s.prepareSSHSecret(ctx, connID, carrier)
	}
	ts, err := s.sessMgr.GetOrCreateTabSession(ctx, tabID, connID, db, schema)
	if err != nil {
		appErr := asAppError(err, dto.ErrConnectionFailed, "建立标签页独立会话失败")
		attachSaveOutcomeToError(appErr, carrier)
		return dto.TabSessionInfo{}, appErr
	}
	info := ts.Info()
	if carrier != nil {
		info.CredentialSaved = carrier.credentialSaved.Load()
		info.SaveWarning = carrier.getSaveWarning()
	}
	return info, nil
}

// SwitchTabDatabase changes the target database for a query tab (in-place USE for MySQL, reconnect for PG).
func (s *QueryService) SwitchTabDatabase(ctx context.Context, tabID, targetDB string) (dto.TabSessionInfo, error) {
	ts, ok := s.sessMgr.GetTabSession(tabID)
	if !ok {
		return dto.TabSessionInfo{}, dto.NewAppError(dto.ErrNotFound, "标签页会话不存在", tabID)
	}
	if err := ts.SwitchDatabase(ctx, targetDB); err != nil {
		return ts.Info(), wrapErr(err, dto.ErrConnectionFailed, "切换数据库失败")
	}
	return ts.Info(), nil
}

// SwitchTabSchema changes the active schema (e.g. PostgreSQL search_path) for a query tab.
func (s *QueryService) SwitchTabSchema(ctx context.Context, tabID, targetSchema string) (dto.TabSessionInfo, error) {
	ts, ok := s.sessMgr.GetTabSession(tabID)
	if !ok {
		return dto.TabSessionInfo{}, dto.NewAppError(dto.ErrNotFound, "标签页会话不存在", tabID)
	}
	if err := ts.SwitchSchema(ctx, targetSchema); err != nil {
		return ts.Info(), wrapErr(err, dto.ErrInternal, "切换 Schema 失败")
	}
	return ts.Info(), nil
}

// ReconnectTabSession explicitly establishes a fresh physical connection after SessionLost or CommitOutcomeUnknown.
func (s *QueryService) ReconnectTabSession(ctx context.Context, tabID, db, schema string) (dto.TabSessionInfo, error) {
	return s.reconnectTabSessionInternal(ctx, tabID, db, schema, nil)
}

// ReconnectTabSessionWithSSHSecret reconnects a tab session with a runtime SSH password/passphrase.
func (s *QueryService) ReconnectTabSessionWithSSHSecret(ctx context.Context, tabID, db, schema, sshSecret string, saveSecret bool) (dto.TabSessionInfo, error) {
	reqCtx, carrier := withRuntimeSSHSecret(ctx, sshSecret, saveSecret)
	return s.reconnectTabSessionInternal(reqCtx, tabID, db, schema, carrier)
}

func (s *QueryService) reconnectTabSessionInternal(ctx context.Context, tabID, db, schema string, carrier *runtimeSSHSecret) (dto.TabSessionInfo, error) {
	ts, ok := s.sessMgr.GetTabSession(tabID)
	if !ok {
		return dto.TabSessionInfo{}, dto.NewAppError(dto.ErrNotFound, "标签页会话不存在", tabID)
	}
	if err := ts.Reconnect(ctx, db, schema); err != nil {
		appErr := asAppError(err, dto.ErrConnectionFailed, "重新连接失败")
		attachSaveOutcomeToError(appErr, carrier)
		return ts.Info(), appErr
	}
	info := ts.Info()
	if carrier != nil {
		info.CredentialSaved = carrier.credentialSaved.Load()
		info.SaveWarning = carrier.getSaveWarning()
	}
	return info, nil
}

// CloseTab releases BOTH the dedicated Track B physical connection AND all Execute-Once snapshots for tabID.
func (s *QueryService) CloseTab(tabID string) error {
	s.resMgr.ReleaseByTab(tabID)
	return s.sessMgr.CloseTab(tabID)
}

func (s *QueryService) GetTabSessionInfo(tabID string) (dto.TabSessionInfo, bool) {
	ts, ok := s.sessMgr.GetTabSession(tabID)
	if !ok {
		return dto.TabSessionInfo{}, false
	}
	return ts.Info(), true
}

// BrowseTable performs LIVE real-time paginated browsing of one table (separate from Execute-Once SQL snapshots).
func (s *QueryService) BrowseTable(ctx context.Context, req dto.BrowseTableRequest) (*dto.BrowseTableResponse, error) {
	if req.ConnectionID == "" || req.Table == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "连接与表名不能为空", "")
	}
	var resp *dto.BrowseTableResponse
	err := s.sessMgr.WithMetaConn(ctx, req.ConnectionID, req.Database, func(conn driver.Conn) error {
		insp, ok := conn.(driver.MetaInspector)
		if !ok {
			return dto.NewAppError(dto.ErrUnsupported, "当前驱动不支持元数据探测", "")
		}
		browser, ok := conn.(driver.TableBrowser)
		if !ok {
			return dto.NewAppError(dto.ErrUnsupported, "当前驱动不支持实时表浏览", "")
		}
		// Resolve connection's client read-only flag from local repository if present.
		clientRO := false
		if s.repo != nil {
			if ent, rerr := s.repo.GetConnection(ctx, req.ConnectionID); rerr == nil && ent != nil {
				clientRO = ent.ReadOnly
			}
		}
		edit, err := insp.InspectTableEditability(ctx, req.Database, req.Schema, req.Table, clientRO)
		if err != nil {
			return err
		}
		resp, err = browser.BrowseTable(ctx, req, edit)
		return err
	})
	if err != nil {
		return nil, wrapErr(err, dto.ErrInternal, "浏览表数据失败")
	}
	return resp, nil
}

// ExecuteSQL executes SQL on the tab's dedicated physical connection (Execute-Once),
// stores row results in ResultBuffer snapshots, and returns page 1 plus ResultIDs for zero-rerun paging.
func (s *QueryService) ExecuteSQL(ctx context.Context, req dto.ExecuteSQLRequest) (*dto.ExecuteSQLResponse, error) {
	if req.TabID == "" || req.ConnectionID == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "TabID 和 ConnectionID 不能为空", "")
	}
	if strings.TrimSpace(req.SQL) == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "SQL 内容不能为空", "")
	}
	execID := req.ExecID
	if execID == "" {
		execID = uuid.NewString()
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 200
	}
	if pageSize > 5000 {
		pageSize = 5000
	}

	ts, err := s.sessMgr.GetOrCreateTabSession(ctx, req.TabID, req.ConnectionID, req.Database, req.Schema)
	if err != nil {
		appErr := asAppError(err, dto.ErrConnectionFailed, "无法获取标签页会话")
		st := dto.SessionLost
		var ep int64
		if cur, ok := s.sessMgr.GetTabSession(req.TabID); ok {
			info := cur.Info()
			st = info.SessionState
			ep = info.SessionEpoch
		}
		if appErr.SessionEpoch == 0 && ep > 0 {
			appErr.SessionEpoch = ep
		}
		return &dto.ExecuteSQLResponse{
			TabID:        req.TabID,
			ExecID:       execID,
			SessionState: st,
			SessionEpoch: ep,
			Error:        appErr,
		}, nil
	}

	connID := ts.ConnectionID()
	dialect := sqlsplit.DialectMySQL
	switch ts.Dialect() {
	case dto.EnginePostgres:
		dialect = sqlsplit.DialectPostgres
	case dto.EngineSQLite:
		dialect = sqlsplit.DialectSQLite
	}

	allStmts := sqlsplit.Split(req.SQL, dialect)
	var stmts []sqlsplit.Statement
	switch req.RunMode {
	case "current":
		if st, ok := sqlsplit.SelectStatementAtCursor(allStmts, req.CursorOffset); ok {
			stmts = []sqlsplit.Statement{st}
		}
	default:
		// "all" or "selection"
		stmts = allStmts
	}
	if len(stmts) == 0 {
		info := ts.Info()
		return &dto.ExecuteSQLResponse{
			TabID:         req.TabID,
			ExecID:        execID,
			CurrentDB:     info.CurrentDB,
			CurrentSchema: info.CurrentSchema,
			SessionState:  info.SessionState,
			SessionEpoch:  info.SessionEpoch,
			Error:         dto.NewAppError(dto.ErrInvalidArgument, "未找到可执行的 SQL 语句", ""),
		}, nil
	}

	// Production high-risk safeguard (docs/03 §4.2): inspect the exact statements that will execute,
	// fail closed if connection safety metadata cannot be read, and bind confirmation to a digest
	// of the exact statements and matched risks using the session's verified ConnectionID.
	if s.repo != nil {
		ent, rerr := s.repo.GetConnection(ctx, connID)
		if rerr != nil || ent == nil {
			detail := "connection metadata not found"
			if rerr != nil {
				detail = rerr.Error()
			}
			info := ts.Info()
			return &dto.ExecuteSQLResponse{
				TabID:         req.TabID,
				ExecID:        execID,
				CurrentDB:     info.CurrentDB,
				CurrentSchema: info.CurrentSchema,
				SessionState:  info.SessionState,
				SessionEpoch:  info.SessionEpoch,
				Error:         dto.NewAppError(dto.ErrInternal, "读取连接安全配置失败，已中止执行", detail),
			}, nil
		}
		if ent.EnvColor == "red" {
			var risks []dto.HighRiskStatement
			for _, st := range stmts {
				for _, m := range sqlsplit.DetectHighRisk(st, dialect) {
					risks = append(risks, dto.HighRiskStatement{
						Kind:  m.Kind,
						Label: m.Label,
						SQL:   m.SQL,
					})
				}
			}
			if len(risks) > 0 {
				expectedToken := computeHighRiskConfirmToken(connID, req.TabID, stmts, risks)
				if req.ConfirmToken == "" || subtle.ConstantTimeCompare([]byte(req.ConfirmToken), []byte(expectedToken)) != 1 {
					info := ts.Info()
					return &dto.ExecuteSQLResponse{
						TabID:              req.TabID,
						ExecID:             execID,
						CurrentDB:          info.CurrentDB,
						CurrentSchema:      info.CurrentSchema,
						SessionState:       info.SessionState,
						SessionEpoch:       info.SessionEpoch,
						RequiresConfirm:    true,
						ConfirmToken:       expectedToken,
						HighRiskStatements: risks,
					}, nil
				}
			}
		}
	}

	// Release previous snapshots for this tab when starting a new execution.
	s.resMgr.ReleaseByTab(req.TabID)

	totalStart := time.Now()
	resp := &dto.ExecuteSQLResponse{
		TabID:      req.TabID,
		ExecID:     execID,
		Statements: make([]dto.StatementResult, 0, len(stmts)),
	}
	changes := newSchemaChangeCollector()

	for idx, stmt := range stmts {
		stmtRes := dto.StatementResult{
			Index:     idx,
			SQL:       stmt.Text,
			StartLine: stmt.StartLine,
		}

		ddl := sqlsplit.InspectDDL(stmt.Text, dialect)
		if ddl.IsDropDB && ddl.TargetName != "" && !ts.ReadOnly() && ts.State() == dto.SessionAliveIdle {
			// Release Hesper's own Track A metadata pool connections targeting this DB so PG DROP DATABASE
			// does not fail with SQLSTATE 55006 just because the user browsed the DB in Explorer.
			s.sessMgr.CloseDatabaseMetaPool(connID, ddl.TargetName)
		}

		var snapID string
		var snapMeta resultbuf.SnapshotMeta
		var hasColumns bool

		outcome := ts.ExecuteStatement(ctx, execID, stmt, func(stream driver.RowStream) error {
			cols := stream.Columns()
			if len(cols) == 0 {
				return nil
			}
			hasColumns = true
			builder := s.resMgr.NewBuilder(req.TabID, connID, cols)
			for {
				row, ok, nextErr := stream.Next(builder.RegisterBlob)
				if nextErr != nil {
					builder.Abort()
					return nextErr
				}
				if !ok {
					break
				}
				keepGoing, appErr := builder.Append(row)
				if appErr != nil {
					builder.Abort()
					return appErr
				}
				if !keepGoing {
					// Hard capacity cap reached — stop reading immediately; ExecuteStatement will close stream.
					break
				}
			}
			var finErr error
			snapID, snapMeta, finErr = builder.Finalize()
			return finErr
		})

		stmtRes.DurationMs = outcome.DurationMs
		stmtRes.AffectedRows = outcome.AffectedRows
		stmtRes.Error = outcome.Error

		if outcome.Error == nil && hasColumns && snapID != "" {
			pageResp, pageErr := s.resMgr.FetchPage(snapID, 1, pageSize)
			if pageErr != nil {
				stmtRes.Error = pageErr
			} else {
				stmtRes.IsResultSet = true
				stmtRes.ResultID = snapID
				stmtRes.Columns = pageResp.Columns
				stmtRes.Rows = pageResp.Rows
				stmtRes.Page = pageResp.Page
				stmtRes.PageSize = pageResp.PageSize
				stmtRes.TotalBuffered = snapMeta.TotalBuffered
				stmtRes.HasMorePages = pageResp.HasMorePages
				stmtRes.TruncatedByLimit = snapMeta.TruncatedByLimit
				stmtRes.LimitReason = snapMeta.LimitReason
			}
		}

		resp.Statements = append(resp.Statements, stmtRes)
		s.recordHistory(ctx, connID, outcome.CurrentDB, stmt.Text, stmtRes)

		if outcome.Error != nil {
			resp.Error = outcome.Error
			break
		}

		// Record structural DDL effects (deferring transactional DDL until COMMIT succeeds).
		if ddl.Scope != sqlsplit.DDLScopeNone {
			if outcome.SessionState == dto.SessionAliveInTx {
				scoped := ddl
				if scoped.TargetDB == "" {
					scoped.TargetDB = outcome.CurrentDB
				}
				if scoped.TargetSchema == "" && ts.Dialect() == dto.EnginePostgres {
					scoped.TargetSchema = outcome.CurrentSchema
				}
				if len(scoped.ExtraObjects) > 0 {
					extras := make([]sqlsplit.DDLObjectTarget, len(scoped.ExtraObjects))
					copy(extras, scoped.ExtraObjects)
					for i := range extras {
						if extras[i].DB == "" {
							extras[i].DB = outcome.CurrentDB
						}
						if extras[i].Schema == "" && ts.Dialect() == dto.EnginePostgres {
							extras[i].Schema = outcome.CurrentSchema
						}
					}
					scoped.ExtraObjects = extras
				}
				ts.RecordPendingTxDDL(scoped)
			} else {
				changes.add(ddl, outcome.CurrentDB, outcome.CurrentSchema, ts.Dialect())
				if ddl.IsDropDB && ddl.TargetName != "" {
					s.sessMgr.CloseDatabaseMetaPool(connID, ddl.TargetName)
				}
			}
		} else if stmt.Kind == sqlsplit.KindCommit {
			for _, pending := range ts.DrainPendingTxDDL() {
				changes.add(pending, outcome.CurrentDB, outcome.CurrentSchema, ts.Dialect())
			}
		} else if stmt.Kind == sqlsplit.KindRollback {
			// Only full ROLLBACK discards pending DDL; ROLLBACK TO SAVEPOINT preserves pre-savepoint DDL.
			ts.ClearPendingTxDDL()
		}
	}

	info := ts.Info()
	resp.CurrentDB = info.CurrentDB
	resp.CurrentSchema = info.CurrentSchema
	resp.SessionState = info.SessionState
	resp.SessionEpoch = info.SessionEpoch
	resp.ExecutedAll = req.RunMode != "selection" && resp.Error == nil && len(resp.Statements) == len(allStmts)
	resp.SchemaChanges = changes.build()
	resp.TotalMs = time.Since(totalStart).Milliseconds()
	return resp, nil
}

type schemaChangeCollector struct {
	databasesChanged bool
	droppedDBSet     map[string]struct{}
	droppedDBs       []string
	changedSchemaSet map[string]struct{}
	changedSchemas   []string
	changedObjSet    map[string]struct{}
	changedObjects   []dto.SchemaObjectScope
}

func newSchemaChangeCollector() *schemaChangeCollector {
	return &schemaChangeCollector{
		droppedDBSet:     make(map[string]struct{}),
		changedSchemaSet: make(map[string]struct{}),
		changedObjSet:    make(map[string]struct{}),
	}
}

func (c *schemaChangeCollector) addObjectScope(db, sch, currentDB, currentSchema string, engine dto.EngineType) {
	if db == "" {
		db = currentDB
	}
	if engine == dto.EnginePostgres {
		if sch == "" {
			sch = currentSchema
		}
	} else {
		sch = ""
	}
	key := db + "\x00" + sch
	if _, exists := c.changedObjSet[key]; !exists {
		c.changedObjSet[key] = struct{}{}
		c.changedObjects = append(c.changedObjects, dto.SchemaObjectScope{
			Database: db,
			Schema:   sch,
		})
	}
}

func (c *schemaChangeCollector) add(act sqlsplit.DDLAction, currentDB, currentSchema string, engine dto.EngineType) {
	switch act.Scope {
	case sqlsplit.DDLScopeDatabase:
		c.databasesChanged = true
		if act.IsDropDB && act.TargetName != "" {
			if _, exists := c.droppedDBSet[act.TargetName]; !exists {
				c.droppedDBSet[act.TargetName] = struct{}{}
				c.droppedDBs = append(c.droppedDBs, act.TargetName)
			}
		}
	case sqlsplit.DDLScopeSchema:
		db := act.TargetDB
		if db == "" {
			db = currentDB
		}
		if db != "" {
			if _, exists := c.changedSchemaSet[db]; !exists {
				c.changedSchemaSet[db] = struct{}{}
				c.changedSchemas = append(c.changedSchemas, db)
			}
		}
	case sqlsplit.DDLScopeObject:
		c.addObjectScope(act.TargetDB, act.TargetSchema, currentDB, currentSchema, engine)
		for _, extra := range act.ExtraObjects {
			c.addObjectScope(extra.DB, extra.Schema, currentDB, currentSchema, engine)
		}
	}
}

func (c *schemaChangeCollector) build() *dto.SchemaChanges {
	if !c.databasesChanged && len(c.changedSchemas) == 0 && len(c.changedObjects) == 0 {
		return nil
	}
	return &dto.SchemaChanges{
		DatabasesChanged: c.databasesChanged,
		DroppedDatabases: c.droppedDBs,
		ChangedSchemas:   c.changedSchemas,
		ChangedObjects:   c.changedObjects,
	}
}

func computeHighRiskConfirmToken(connID, tabID string, stmts []sqlsplit.Statement, risks []dto.HighRiskStatement) string {
	h := sha256.New()
	writePart := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	writePart(connID)
	writePart(tabID)
	_, _ = fmt.Fprintf(h, "stmts=%d|", len(stmts))
	for _, st := range stmts {
		writePart(st.Text)
	}
	_, _ = fmt.Fprintf(h, "risks=%d|", len(risks))
	for _, r := range risks {
		writePart(r.Kind)
		writePart(r.SQL)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FetchResultPage slices a subsequent page from an existing Execute-Once snapshot WITHOUT re-running SQL.
func (s *QueryService) FetchResultPage(req dto.FetchResultPageRequest) (*dto.FetchResultPageResponse, error) {
	resp, appErr := s.resMgr.FetchPage(req.ResultID, req.Page, req.PageSize)
	if appErr != nil {
		return nil, appErr
	}
	return resp, nil
}

// CancelExecution cancels a specific running execID via bypass channel with cancelFence protection.
func (s *QueryService) CancelExecution(ctx context.Context, tabID, execID string) dto.CancelExecutionResponse {
	ts, ok := s.sessMgr.GetTabSession(tabID)
	if !ok {
		return dto.CancelExecutionResponse{
			TabID:        tabID,
			ExecID:       execID,
			Status:       "noop",
			SessionState: dto.SessionLost,
			Message:      "标签页会话不存在或已关闭",
		}
	}
	return ts.CancelExecution(ctx, execID)
}

// ForceAbortSession tears down an unresponsive tab session and marks it SessionLost.
func (s *QueryService) ForceAbortSession(tabID string) dto.TabSessionInfo {
	ts, ok := s.sessMgr.GetTabSession(tabID)
	if !ok {
		return dto.TabSessionInfo{TabID: tabID, SessionState: dto.SessionLost}
	}
	return ts.ForceAbort()
}

// GetStreamBlob fetches an offloaded binary or oversized text cell payload for /stream/{ref}.
func (s *QueryService) GetStreamBlob(ref string) (*resultbuf.StreamBlob, bool) {
	return s.resMgr.GetBlob(ref)
}

// ListHistory returns recent SQL executions from local app.db.
func (s *QueryService) ListHistory(ctx context.Context, connID string, limit int) ([]dto.QueryHistoryItem, error) {
	if s.repo == nil {
		return nil, nil
	}
	ents, err := s.repo.ListHistory(ctx, connID, limit)
	if err != nil {
		return nil, dto.NewAppError(dto.ErrInternal, "读取查询历史失败", err.Error())
	}
	out := make([]dto.QueryHistoryItem, 0, len(ents))
	for _, e := range ents {
		out = append(out, dto.QueryHistoryItem{
			ID:           e.ID,
			ConnectionID: e.ConnectionID,
			DatabaseName: e.DatabaseName,
			SQLText:      e.SQLText,
			Status:       e.Status,
			DurationMs:   e.DurationMs,
			AffectedRows: e.AffectedRows,
			ErrorCode:    e.ErrorCode,
			ExecutedAt:   e.ExecutedAt,
		})
	}
	return out, nil
}

func (s *QueryService) recordHistory(ctx context.Context, connID, db, sqlText string, res dto.StatementResult) {
	if s.repo == nil {
		return
	}
	status := "ok"
	errCode := ""
	if res.Error != nil {
		errCode = string(res.Error.Code)
		switch res.Error.Code {
		case dto.ErrStatementCanceled:
			status = "canceled"
		case dto.ErrCommitOutcomeUnknown:
			status = "unknown"
		default:
			status = "error"
		}
	}
	_ = s.repo.AppendHistory(ctx, entity.QueryHistoryEntity{
		ID:           uuid.NewString(),
		ConnectionID: connID,
		DatabaseName: db,
		SQLText:      sqlText,
		Status:       status,
		DurationMs:   res.DurationMs,
		AffectedRows: res.AffectedRows,
		ErrorCode:    errCode,
		ExecutedAt:   time.Now().UnixMilli(),
	})
}
