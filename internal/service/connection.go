package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
	"sql-manager/internal/entity"
	"sql-manager/internal/repository"
	"sql-manager/internal/resultbuf"
	"sql-manager/internal/secret"
	"sql-manager/internal/session"
)

// ConnectionService manages saved connection profiles, credential storage, and connection lifecycle.
type ConnectionService struct {
	repo    *repository.LocalDB
	secrets *secret.Store
	sessMgr *session.Manager
	resMgr  *resultbuf.Manager
}

func NewConnectionService(
	repo *repository.LocalDB,
	secrets *secret.Store,
	sessMgr *session.Manager,
	resMgr *resultbuf.Manager,
) *ConnectionService {
	return &ConnectionService{
		repo:    repo,
		secrets: secrets,
		sessMgr: sessMgr,
		resMgr:  resMgr,
	}
}

// SetSessionManager wires the circular ConfigResolver <-> session.Manager dependency cleanly at bootstrap time.
func (s *ConnectionService) SetSessionManager(mgr *session.Manager) {
	s.sessMgr = mgr
}

// ResolveConfig builds a decrypted driver.ResolvedConfig for internal/session.
func (s *ConnectionService) ResolveConfig(ctx context.Context, connID string, overrideDB string) (driver.ResolvedConfig, error) {
	ent, err := s.repo.GetConnection(ctx, connID)
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
	if ent.SecretRef != "" {
		pw, err := s.secrets.Get(ent.SecretRef)
		if err != nil {
			return driver.ResolvedConfig{}, dto.NewAppError(dto.ErrInternal, "读取连接凭据失败", err.Error())
		}
		password = pw
	}
	db := params.DefaultDatabase
	if overrideDB != "" {
		db = overrideDB
	}
	return driver.ResolvedConfig{
		ConnectionID: ent.ID,
		Engine:       dto.EngineType(ent.Driver),
		Host:         params.Host,
		Port:         params.Port,
		Username:     params.Username,
		Password:     password,
		Database:     db,
		FilePath:     params.FilePath,
		ReadOnly:     ent.ReadOnly,
		TLS:          params.TLS,
		TimeoutSec:   params.ConnectTimeoutS,
	}, nil
}

func (s *ConnectionService) ListConnections(ctx context.Context) ([]dto.ConnectionDTO, error) {
	ents, err := s.repo.ListConnections(ctx)
	if err != nil {
		return nil, dto.NewAppError(dto.ErrInternal, "读取连接列表失败", err.Error())
	}
	out := make([]dto.ConnectionDTO, 0, len(ents))
	for _, e := range ents {
		out = append(out, entityToConnectionDTO(e))
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
	if req.Params.Port <= 0 {
		switch req.Driver {
		case dto.EnginePostgres:
			req.Params.Port = 5432
		case dto.EngineMySQL, dto.EngineMariaDB:
			req.Params.Port = 3306
		}
	}

	now := time.Now().UnixMilli()
	id := strings.TrimSpace(req.ID)
	var existing *entity.ConnectionEntity
	if id != "" {
		var err error
		existing, err = s.repo.GetConnection(ctx, id)
		if err != nil {
			return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInternal, "查询原连接配置失败", err.Error())
		}
	} else {
		id = uuid.NewString()
	}

	secretRef := ""
	createdAt := now
	if existing != nil {
		secretRef = existing.SecretRef
		createdAt = existing.CreatedAt
	}

	// Only overwrite stored secret when creating new or UpdatePassword == true.
	if existing == nil || req.UpdatePassword {
		if req.Password != "" {
			if secretRef == "" {
				secretRef = "conn:" + id + ":password"
			}
			if err := s.secrets.Set(secretRef, req.Password); err != nil {
				return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInternal, "保存密码到安全存储失败", err.Error())
			}
		} else if secretRef != "" {
			_ = s.secrets.Delete(secretRef)
			secretRef = ""
		}
	}

	cfgBytes, err := json.Marshal(req.Params)
	if err != nil {
		return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInternal, "序列化连接配置失败", err.Error())
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
	if err := s.repo.UpsertConnection(ctx, ent); err != nil {
		return dto.ConnectionDTO{}, dto.NewAppError(dto.ErrInternal, "保存连接配置失败", err.Error())
	}

	// If editing an already-open connection, close stale sessions/pools so new settings apply cleanly.
	if existing != nil && s.sessMgr != nil {
		s.sessMgr.CloseConnection(id)
	}
	return entityToConnectionDTO(ent), nil
}

func (s *ConnectionService) DeleteConnection(ctx context.Context, id string) error {
	if s.sessMgr != nil {
		s.sessMgr.CloseConnection(id)
	}
	if s.resMgr != nil {
		s.resMgr.ReleaseByConnection(id)
	}
	existing, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return dto.NewAppError(dto.ErrInternal, "查询连接失败", err.Error())
	}
	if existing != nil && existing.SecretRef != "" {
		_ = s.secrets.Delete(existing.SecretRef)
	}
	if err := s.repo.DeleteConnection(ctx, id); err != nil {
		return dto.NewAppError(dto.ErrInternal, "删除连接失败", err.Error())
	}
	return nil
}

// TestConnection validates connectivity using either inline form fields or a saved connection's password.
func (s *ConnectionService) TestConnection(ctx context.Context, req dto.SaveConnectionRequest) dto.TestConnectionResult {
	pw := req.Password
	if !req.UpdatePassword && req.ID != "" {
		if existing, err := s.repo.GetConnection(ctx, req.ID); err == nil && existing != nil && existing.SecretRef != "" {
			if stored, serr := s.secrets.Get(existing.SecretRef); serr == nil {
				pw = stored
			}
		}
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
		TimeoutSec:   req.Params.ConnectTimeoutS,
	}

	start := time.Now()
	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

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
		return dto.ConnectResult{}, wrapErr(err, dto.ErrConnectionFailed, "打开数据库连接失败")
	}
	return res, nil
}

// Disconnect closes all Track A metadata pools, Track B tab sessions, and ResultBuffer snapshots for connID.
func (s *ConnectionService) Disconnect(connID string) {
	if s.sessMgr != nil {
		s.sessMgr.CloseConnection(connID)
	}
	if s.resMgr != nil {
		s.resMgr.ReleaseByConnection(connID)
	}
}

func entityToConnectionDTO(e entity.ConnectionEntity) dto.ConnectionDTO {
	var params dto.ConnectionParams
	_ = json.Unmarshal([]byte(e.ConfigJSON), &params)
	gid := ""
	if e.GroupID != nil {
		gid = *e.GroupID
	}
	return dto.ConnectionDTO{
		ID:          e.ID,
		GroupID:     gid,
		Name:        e.Name,
		Driver:      dto.EngineType(e.Driver),
		EnvColor:    e.EnvColor,
		ReadOnly:    e.ReadOnly,
		Params:      params,
		HasPassword: e.SecretRef != "",
		SortOrder:   e.SortOrder,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
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
	if ae, ok := err.(*dto.AppError); ok {
		return ae
	}
	return dto.NewAppError(fallbackCode, fmt.Sprintf("%s: %s", fallbackMsg, err.Error()), err.Error())
}
