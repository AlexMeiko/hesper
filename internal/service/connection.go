package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"hesper/internal/driver"
	"hesper/internal/dto"
	"hesper/internal/entity"
	"hesper/internal/repository"
	"hesper/internal/resultbuf"
	"hesper/internal/session"
	"hesper/internal/tunnel"
)

// ConnectionService manages saved connection profiles, credential storage, SSH tunnel wiring,
// and connection lifecycle.
type ConnectionService struct {
	repo      *repository.LocalDB
	secrets   SecretStore
	sessMgr   *session.Manager
	resMgr    *resultbuf.Manager
	tunnelMgr *tunnel.Manager

	// cfgMu guarantees (connection entity, configVer) snapshots are read atomically by ResolveConfig
	// and updated/invalidated atomically by SaveConnection, DeleteConnection, and pre-auth credential saves.
	cfgMu                sync.RWMutex
	configVer            map[string]uint64
	pendingSecretCleanup map[string]map[string]struct{} // orphaned newly-added refs whose rollback Delete failed

	// Optional test hook to inject UpsertConnection failures deterministically; nil uses repo.UpsertConnection.
	upsertHook func(ctx context.Context, ent entity.ConnectionEntity) error
}

func NewConnectionService(
	repo *repository.LocalDB,
	secrets SecretStore,
	sessMgr *session.Manager,
	resMgr *resultbuf.Manager,
) *ConnectionService {
	return &ConnectionService{
		repo:                 repo,
		secrets:              secrets,
		sessMgr:              sessMgr,
		resMgr:               resMgr,
		configVer:            make(map[string]uint64),
		pendingSecretCleanup: make(map[string]map[string]struct{}),
	}
}

// SetSessionManager wires the circular ConfigResolver <-> session.Manager dependency cleanly at bootstrap time.
func (s *ConnectionService) SetSessionManager(mgr *session.Manager) {
	s.sessMgr = mgr
}

// SetTunnelManager wires the pooled SSH tunnel manager.
func (s *ConnectionService) SetTunnelManager(mgr *tunnel.Manager) {
	s.tunnelMgr = mgr
}

func (s *ConnectionService) configVerLocked(connID string) uint64 {
	v := s.configVer[connID]
	if v == 0 {
		return 1
	}
	return v
}

func (s *ConnectionService) bumpConfigVerLocked(connID string) uint64 {
	next := s.configVerLocked(connID) + 1
	s.configVer[connID] = next
	return next
}

func (s *ConnectionService) upsertConnection(ctx context.Context, ent entity.ConnectionEntity) error {
	if s.upsertHook != nil {
		return s.upsertHook(ctx, ent)
	}
	return s.repo.UpsertConnection(ctx, ent)
}

// ResolveConfig builds a decrypted driver.ResolvedConfig for internal/session.
func (s *ConnectionService) ResolveConfig(ctx context.Context, connID string, overrideDB string) (driver.ResolvedConfig, error) {
	s.cfgMu.RLock()
	ent, err := s.repo.GetConnection(ctx, connID)
	cfgVer := s.configVerLocked(connID)
	s.cfgMu.RUnlock()

	if err != nil {
		return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrInternal, "读取连接配置失败", err.Error())
	}
	if ent == nil {
		return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrNotFound, "连接配置不存在", connID)
	}
	var params dto.ConnectionParams
	if err := json.Unmarshal([]byte(ent.ConfigJSON), &params); err != nil {
		return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrInternal, "连接参数解析失败", err.Error())
	}
	var password string
	if ent.SecretRef != "" && s.secrets != nil {
		pw, err := s.secrets.Get(ent.SecretRef)
		if err != nil {
			return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrInternal, "读取连接凭据失败", err.Error())
		}
		password = pw
	}

	if carrier := runtimeSSHSecretFromContext(ctx); carrier != nil && carrier.expectedCfgVer > 0 && cfgVer != carrier.expectedCfgVer {
		// Connection config changed since the plan/challenge was created: reject BEFORE resolveSSHTunnel
		// so the runtime SSH password is never used to authenticate against an edited host (even when saveSecret=false).
		return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrPlanInvalidated, "连接配置已变更，当前变更计划已失效，请重新预览", "")
	}

	var dialCtx func(ctx context.Context, network, addr string) (net.Conn, error)
	var tunnelGen uint64

	if params.SSH != nil && params.SSH.Enabled {
		if s.tunnelMgr == nil {
			return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrTunnelFailed, "SSH 隧道管理器未初始化，无法通过 SSH 隧道连接", "")
		}
		dFn, gen, terr := s.resolveSSHTunnel(ctx, connID, cfgVer, params)
		if terr != nil {
			return driver.ResolvedConfig{}, terr
		}
		dialCtx = dFn
		tunnelGen = gen
	}

	db := params.DefaultDatabase
	if overrideDB != "" {
		db = overrideDB
	}
	return driver.ResolvedConfig{
		ConnectionID:  ent.ID,
		Engine:        dto.EngineType(ent.Driver),
		Host:          params.Host,
		Port:          params.Port,
		Username:      params.Username,
		Password:      password,
		Database:      db,
		FilePath:      params.FilePath,
		ReadOnly:      ent.ReadOnly,
		TLS:           params.TLS,
		TimeoutSec:    params.ConnectTimeoutS,
		ErrorCharset:  params.ErrorCharset,
		DialContext:   dialCtx,
		TunnelGen:     tunnelGen,
		ConfigVersion: cfgVer,
	}, nil
}

func (s *ConnectionService) ListConnections(ctx context.Context) ([]dto.ConnectionDTO, error) {
	ents, err := s.repo.ListConnections(ctx)
	if err != nil {
		return nil, dto.NewAppError(dto.ErrInternal, "读取连接列表失败", err.Error())
	}
	out := make([]dto.ConnectionDTO, 0, len(ents))
	for _, e := range ents {
		out = append(out, s.entityToConnectionDTO(e))
	}
	return out, nil
}

func (s *ConnectionService) SaveConnection(ctx context.Context, req dto.SaveConnectionRequest) (dto.ConnectionDTO, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInvalidArgument, "连接名称不能为空", "")
	}
	if req.Driver == "" {
		req.Driver = dto.EngineMySQL
	}
	if req.EnvColor == "" {
		req.EnvColor = "default"
	}
	if req.Driver == dto.EngineSQLite {
		req.Params.FilePath = strings.TrimSpace(req.Params.FilePath)
		if req.Params.FilePath == "" {
			return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInvalidArgument, "SQLite 数据库文件路径不能为空", "")
		}
		if req.Params.SSH != nil && req.Params.SSH.Enabled {
			return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInvalidArgument, "SQLite 本地连接不支持配置 SSH 隧道", "")
		}
		req.Params.Host = ""
		req.Params.Port = 0
		req.Params.Username = ""
		req.Params.DefaultDatabase = "main"
		req.Params.TLS = dto.TLSConfig{Mode: "disable"}
		req.Params.SSH = nil
	} else {
		if req.Params.Port <= 0 {
			switch req.Driver {
			case dto.EnginePostgres:
				req.Params.Port = 5432
			case dto.EngineMySQL, dto.EngineMariaDB:
				req.Params.Port = 3306
			}
		}
		if tlsErr := driver.ValidateTLSConfig(req.Params.TLS); tlsErr != nil {
			return dto.ConnectionDTO{}, tlsErr
		}
		req.Params.TLS.Mode = driver.NormalizeTLSMode(req.Params.TLS.Mode)
		req.Params.TLS.CACertPath = strings.TrimSpace(req.Params.TLS.CACertPath)
		req.Params.TLS.CertPath = strings.TrimSpace(req.Params.TLS.CertPath)
		req.Params.TLS.KeyPath = strings.TrimSpace(req.Params.TLS.KeyPath)

		normalizedSSH, sshErr := validateAndNormalizeSSH(req.Driver, req.Params.SSH)
		if sshErr != nil {
			return dto.ConnectionDTO{}, sshErr
		}
		req.Params.SSH = normalizedSSH
	}

	errorCharset, charsetErr := normalizeAndValidateErrorCharset(req.Driver, req.Params.ErrorCharset)
	if charsetErr != nil {
		return dto.ConnectionDTO{}, charsetErr
	}
	req.Params.ErrorCharset = errorCharset

	cfgBytes, err := json.Marshal(req.Params)
	if err != nil {
		return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInternal, "序列化连接配置失败", err.Error())
	}

	now := time.Now().UnixMilli()
	id := strings.TrimSpace(req.ID)
	isNew := id == ""
	if isNew {
		id = uuid.NewString()
	}

	s.cfgMu.Lock()
	var existing *entity.ConnectionEntity
	if !isNew {
		var err error
		existing, err = s.repo.GetConnection(ctx, id)
		if err != nil {
			s.cfgMu.Unlock()
			return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInternal, "查询原连接配置失败", err.Error())
		}
	}

	pendingRefs := s.pendingCleanupRefsLocked(ctx, id)
	secretRef := ""
	createdAt := now
	if existing != nil {
		secretRef = existing.SecretRef
		createdAt = existing.CreatedAt
	}
	hadDBSecret := existing != nil && existing.SecretRef != ""
	undo := newSecretUndoLog(pendingRefs)
	defer undo.wipe()

	rollbackAndWrap := func(baseCode dto.ErrorCode, baseMsg, baseDetail string) (dto.ConnectionDTO, error) {
		rollbackErrs := s.rollbackSecretUndoLocked(ctx, id, undo)
		s.cfgMu.Unlock()
		if len(rollbackErrs) > 0 {
			detail := fmt.Sprintf("save error: %s; rollback error: %s", baseDetail, strings.Join(rollbackErrs, "; "))
			if baseDetail == "" {
				detail = "rollback error: " + strings.Join(rollbackErrs, "; ")
			}
			return dto.ConnectionDTO{}, dto.NewAppError(
				baseCode,
				baseMsg+"，且恢复保存前凭据状态失败；请检查安全存储后重试保存或删除连接",
				detail,
			)
		}
		return dto.ConnectionDTO{}, dto.NewAppError(baseCode, baseMsg, baseDetail)
	}

	// Only overwrite stored DB password when creating new or UpdatePassword == true.
	if existing == nil || req.UpdatePassword {
		if req.Password != "" {
			if secretRef == "" {
				secretRef = dbPasswordRef(id)
			}
			if err := s.setSecretWithUndo(undo, secretRef, req.Password, "读取原数据库密码状态失败", "保存密码到安全存储失败"); err != nil {
				return rollbackAndWrap(err.Code, err.Message, err.Detail)
			}
		} else if secretRef != "" {
			if err := s.deleteSecretWithUndo(undo, secretRef, "读取原数据库密码状态失败", "清除已保存的数据库密码失败"); err != nil {
				return rollbackAndWrap(err.Code, err.Message, err.Detail)
			}
			secretRef = ""
		} else if pendingRefs[dbPasswordRef(id)] {
			if err := s.deleteSecretWithUndo(undo, dbPasswordRef(id), "读取待清理数据库密码状态失败", "清除待清理的数据库密码失败"); err != nil {
				return rollbackAndWrap(err.Code, err.Message, err.Detail)
			}
		}
	} else if !hadDBSecret && pendingRefs[dbPasswordRef(id)] {
		if err := s.deleteSecretWithUndo(undo, dbPasswordRef(id), "读取待清理数据库密码状态失败", "清除待清理的数据库密码失败"); err != nil {
			return rollbackAndWrap(err.Code, err.Message, err.Detail)
		}
	}

	// Persist or clear SSH credentials strictly according to SSH.Enabled & SSH.SaveCredential,
	// snapshotting ONLY credentials actually mutated so SQLite / non-SSH connections never touch
	// the keyring while any failed SaveConnection restores all modified secrets to pre-save state.
	if sshCredErr := s.syncSSHCredentialsLocked(id, existing, pendingRefs, undo, req); sshCredErr != nil {
		return rollbackAndWrap(sshCredErr.Code, sshCredErr.Message, sshCredErr.Detail)
	}

	var groupPtr *string
	if req.GroupID != "" {
		g := req.GroupID
		groupPtr = &g
	}

	ent := entity.ConnectionEntity{
		ID:         id,
		GroupID:    groupPtr,
		Name:       name,
		Driver:     string(req.Driver),
		EnvColor:   req.EnvColor,
		ReadOnly:   req.ReadOnly,
		ConfigJSON: string(cfgBytes),
		SecretRef:  secretRef,
		CreatedAt:  createdAt,
		UpdatedAt:  now,
	}
	if err := s.upsertConnection(ctx, ent); err != nil {
		return rollbackAndWrap(dto.ErrInternal, "保存连接配置失败", err.Error())
	}

	if len(pendingRefs) > 0 {
		s.clearPendingCleanupLocked(ctx, id)
	}

	newVer := s.bumpConfigVerLocked(id)
	if s.tunnelMgr != nil {
		s.tunnelMgr.InvalidateConfig(id, newVer)
	}
	dtoOut := s.entityToConnectionDTO(ent)
	s.cfgMu.Unlock()

	// If editing an already-open connection, close stale sessions/pools so new settings apply cleanly.
	if existing != nil && s.sessMgr != nil {
		s.sessMgr.CloseConnection(id)
	}
	return dtoOut, nil
}

// DeleteConnection releases runtime resources, invalidates config versions, and deletes ALL saved
// database & SSH credentials as well as the local connection profile.
func (s *ConnectionService) DeleteConnection(ctx context.Context, id string) error {
	if s.sessMgr != nil {
		s.sessMgr.CloseConnection(id)
	}
	if s.resMgr != nil {
		s.resMgr.ReleaseByConnection(id)
	}

	s.cfgMu.Lock()
	newVer := s.bumpConfigVerLocked(id)
	if s.tunnelMgr != nil {
		s.tunnelMgr.InvalidateConfig(id, newVer)
	}
	existing, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		s.cfgMu.Unlock()
		return dto.NewAppError(dto.ErrInternal, "查询连接失败", err.Error())
	}
	pendingRefs := s.pendingCleanupRefsLocked(ctx, id)
	if s.secrets != nil {
		dbRef := ""
		if existing != nil && existing.SecretRef != "" {
			dbRef = existing.SecretRef
		} else if pendingRefs[dbPasswordRef(id)] {
			dbRef = dbPasswordRef(id)
		}
		if dbRef != "" {
			if delErr := s.deleteSecretChecked(dbRef, "清除已保存的数据库密码失败"); delErr != nil {
				s.cfgMu.Unlock()
				return delErr
			}
		}
		cfgMightHavePw, cfgMightHavePp := existingSSHCredentialFlags(existing)
		if cfgMightHavePw || pendingRefs[sshPasswordRef(id)] {
			if delErr := s.deleteSecretChecked(sshPasswordRef(id), "清除已保存的 SSH 密码失败"); delErr != nil {
				s.cfgMu.Unlock()
				return delErr
			}
		}
		if cfgMightHavePp || pendingRefs[sshPassphraseRef(id)] {
			if delErr := s.deleteSecretChecked(sshPassphraseRef(id), "清除已保存的 SSH 私钥口令失败"); delErr != nil {
				s.cfgMu.Unlock()
				return delErr
			}
		}
	}
	if err := s.repo.DeleteConnection(ctx, id); err != nil {
		s.cfgMu.Unlock()
		return dto.NewAppError(dto.ErrInternal, "删除连接失败", err.Error())
	}
	if len(pendingRefs) > 0 {
		s.clearPendingCleanupLocked(ctx, id)
	}
	s.cfgMu.Unlock()
	return nil
}

// TestConnection validates connectivity using explicit inline form fields first, falling back to saved credentials.
func (s *ConnectionService) TestConnection(ctx context.Context, req dto.SaveConnectionRequest) dto.TestConnectionResult {
	errorCharset, charsetErr := normalizeAndValidateErrorCharset(req.Driver, req.Params.ErrorCharset)
	if charsetErr != nil {
		return dto.TestConnectionResult{Success: false, Error: charsetErr}
	}
	if req.Driver != dto.EngineSQLite {
		if tlsErr := driver.ValidateTLSConfig(req.Params.TLS); tlsErr != nil {
			return dto.TestConnectionResult{Success: false, Error: tlsErr}
		}
		normalizedSSH, sshErr := validateAndNormalizeSSH(req.Driver, req.Params.SSH)
		if sshErr != nil {
			return dto.TestConnectionResult{Success: false, Error: sshErr}
		}
		req.Params.SSH = normalizedSSH
	} else if req.Params.SSH != nil && req.Params.SSH.Enabled {
		return dto.TestConnectionResult{
			Success: false,
			Error:   dto.NewAppError(dto.ErrInvalidArgument, "SQLite 本地连接不支持配置 SSH 隧道", ""),
		}
	}

	pw := req.Password
	if !req.UpdatePassword && req.ID != "" && s.secrets != nil {
		if existing, err := s.repo.GetConnection(ctx, req.ID); err == nil && existing != nil && existing.SecretRef != "" {
			if stored, serr := s.secrets.Get(existing.SecretRef); serr == nil {
				pw = stored
			}
		}
	}

	start := time.Now()
	timeoutSec := req.Params.ConnectTimeoutS
	if timeoutSec <= 0 {
		timeoutSec = 10
	}
	testCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	var dialCtx func(ctx context.Context, network, addr string) (net.Conn, error)
	if req.Params.SSH != nil && req.Params.SSH.Enabled {
		if s.tunnelMgr == nil {
			return dto.TestConnectionResult{
				Success:   false,
				LatencyMs: time.Since(start).Milliseconds(),
				Error:     dto.NewAppError(dto.ErrTunnelFailed, "SSH 隧道管理器未初始化，无法通过 SSH 测试连接", ""),
			}
		}
		authMethod := normalizeSSHAuthMethod(req.Params.SSH.AuthMethod)
		sshCfg := tunnel.SSHConfig{
			Host:           strings.TrimSpace(req.Params.SSH.Host),
			Port:           req.Params.SSH.Port,
			Username:       strings.TrimSpace(req.Params.SSH.Username),
			AuthMethod:     authMethod,
			PrivateKeyPath: strings.TrimSpace(req.Params.SSH.PrivateKeyPath),
			ConnectTimeout: time.Duration(timeoutSec) * time.Second,
		}
		switch authMethod {
		case "password":
			if req.SSHPassword != "" {
				sshCfg.Password = req.SSHPassword
			} else if !req.UpdateSSHPassword && req.ID != "" && s.secrets != nil {
				if stored, serr := s.secrets.Get(sshPasswordRef(req.ID)); serr == nil {
					sshCfg.Password = stored
				}
			}
		case "key":
			if req.SSHPassphrase != "" {
				sshCfg.Passphrase = req.SSHPassphrase
			} else if !req.UpdateSSHPassphrase && req.ID != "" && s.secrets != nil {
				if stored, serr := s.secrets.Get(sshPassphraseRef(req.ID)); serr == nil {
					sshCfg.Passphrase = stored
				}
			}
		}

		dFn, closeTunnel, terr := s.tunnelMgr.OpenTransient(testCtx, sshCfg)
		if terr != nil {
			return dto.TestConnectionResult{
				Success:   false,
				LatencyMs: time.Since(start).Milliseconds(),
				Error:     mapTunnelError(terr, req.ID, sshCfg),
			}
		}
		defer closeTunnel()
		dialCtx = dFn
	}

	cfg := driver.ResolvedConfig{
		ConnectionID: req.ID,
		Engine:       req.Driver,
		Host:         req.Params.Host,
		Port:         req.Params.Port,
		Username:     req.Params.Username,
		Password:     pw,
		Database:     req.Params.DefaultDatabase,
		FilePath:     req.Params.FilePath,
		ReadOnly:     req.ReadOnly,
		TLS:          req.Params.TLS,
		TimeoutSec:   timeoutSec,
		ErrorCharset: errorCharset,
		DialContext:  dialCtx,
	}

	conn, err := s.sessMgr.OpenTransient(testCtx, cfg)
	if err != nil {
		return dto.TestConnectionResult{
			Success:   false,
			LatencyMs: time.Since(start).Milliseconds(),
			Error:     asAppError(err, dto.ErrConnectionFailed, "连接数据库失败"),
		}
	}
	defer conn.Close()

	fact, err := conn.ServerFact(testCtx)
	if err != nil {
		return dto.TestConnectionResult{
			Success:   false,
			LatencyMs: time.Since(start).Milliseconds(),
			Error:     asAppError(err, dto.ErrConnectionFailed, "获取数据库版本信息失败"),
		}
	}

	return dto.TestConnectionResult{
		Success:       true,
		DetectedType:  fact.DetectedType,
		ServerVersion: fact.ServerVersion,
		LatencyMs:     time.Since(start).Milliseconds(),
		Capabilities:  conn.Capabilities(fact.ServerVersion),
	}
}

// Connect warms up the metadata pool for connID and returns real server version + Capabilities.
func (s *ConnectionService) Connect(ctx context.Context, connID string) (dto.ConnectResult, error) {
	return s.connectInternal(ctx, connID, nil)
}

// ConnectWithSSHSecret opens/activates connID supplying a runtime SSH password or passphrase.
// When saveSecret is true, the credential is saved pre-auth regardless of subsequent auth outcome.
func (s *ConnectionService) ConnectWithSSHSecret(ctx context.Context, connID, sshSecret string, saveSecret bool) (dto.ConnectResult, error) {
	reqCtx, carrier := withRuntimeSSHSecret(ctx, sshSecret, saveSecret)
	return s.connectInternal(reqCtx, connID, carrier)
}

func (s *ConnectionService) connectInternal(ctx context.Context, connID string, carrier *runtimeSSHSecret) (dto.ConnectResult, error) {
	// Persist explicit saveSecret=true submission BEFORE WithMetaConn borrows any idle pooled DB conn
	// (which skips ResolveConfig when an idle connection is already warm).
	s.PrepareRuntimeSSHCredential(ctx, connID, carrier)

	var res dto.ConnectResult
	err := s.sessMgr.WithMetaConn(ctx, connID, "", func(conn driver.Conn) error {
		fact, err := conn.ServerFact(ctx)
		if err != nil {
			return err
		}
		res = dto.ConnectResult{
			ConnectionID:  connID,
			DetectedType:  fact.DetectedType,
			ServerVersion: fact.ServerVersion,
			CurrentDB:     fact.CurrentDB,
			Capabilities:  conn.Capabilities(fact.ServerVersion),
		}
		return nil
	})
	if err != nil {
		appErr := asAppError(err, dto.ErrConnectionFailed, "打开数据库连接失败")
		attachSaveOutcomeToError(appErr, carrier)
		return dto.ConnectResult{}, appErr
	}
	if carrier != nil {
		res.CredentialSaved = carrier.credentialSaved.Load()
		res.SaveWarning = carrier.getSaveWarning()
	}
	return res, nil
}

// Disconnect releases ONLY runtime resources (Track A/B sessions, ResultBuffer snapshots, and active
// SSH tunnel) for connID. It NEVER deletes saved database or SSH credentials!
func (s *ConnectionService) Disconnect(connID string) {
	if s.sessMgr != nil {
		s.sessMgr.CloseConnection(connID)
	}
	if s.resMgr != nil {
		s.resMgr.ReleaseByConnection(connID)
	}
	if s.tunnelMgr != nil {
		s.tunnelMgr.CloseConnection(connID)
	}
}

func (s *ConnectionService) entityToConnectionDTO(e entity.ConnectionEntity) dto.ConnectionDTO {
	var params dto.ConnectionParams
	_ = json.Unmarshal([]byte(e.ConfigJSON), &params)
	gid := ""
	if e.GroupID != nil {
		gid = *e.GroupID
	}
	return dto.ConnectionDTO{
		ID:               e.ID,
		GroupID:          gid,
		Name:             e.Name,
		Driver:           dto.EngineType(e.Driver),
		EnvColor:         e.EnvColor,
		ReadOnly:         e.ReadOnly,
		Params:           params,
		HasPassword:      e.SecretRef != "",
		HasSSHPassword:   s.hasStoredSecret(sshPasswordRef(e.ID)),
		HasSSHPassphrase: s.hasStoredSecret(sshPassphraseRef(e.ID)),
		SortOrder:        e.SortOrder,
		CreatedAt:        e.CreatedAt,
		UpdatedAt:        e.UpdatedAt,
	}
}

// wrapErr returns an untyped nil error when err == nil, avoiding Go's typed-nil interface trap.
func wrapErr(err error, fallbackCode dto.ErrorCode, fallbackMsg string) error {
	if err == nil {
		return nil
	}
	return asAppError(err, fallbackCode, fallbackMsg)
}

func asAppError(err error, fallbackCode dto.ErrorCode, fallbackMsg string) *dto.AppError {
	if err == nil {
		return nil
	}
	var ae *dto.AppError
	if errors.As(err, &ae) && ae != nil {
		return ae
	}
	if mapped := tunnelChallengeOrAuthAppError(err, ""); mapped != nil {
		return mapped
	}
	return dto.NewAppError(fallbackCode, fmt.Sprintf("%s: %s", fallbackMsg, err.Error()), err.Error()).WithCause(err)
}

func normalizeAndValidateErrorCharset(engine dto.EngineType, raw string) (string, *dto.AppError) {
	trimmed := strings.TrimSpace(raw)
	if engine != dto.EnginePostgres || trimmed == "" {
		return "", nil
	}
	if driver.LookupCharset(trimmed) == nil {
		return "", dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("不支持的报错编码：%s (例如 GBK、GB18030、Windows-1252)", trimmed), "")
	}
	return trimmed, nil
}
