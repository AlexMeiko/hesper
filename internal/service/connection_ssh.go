package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hesper/internal/dto"
	"hesper/internal/tunnel"
)

type runtimeSSHSecretKey struct{}

// runtimeSSHSecret carries a single-request SSH password/passphrase strictly in context
// (never persisted in a shared in-memory map) together with independent save feedback.
type runtimeSSHSecret struct {
	secret          string
	saveSecret      bool
	expectedCfgVer  uint64
	handledSave     atomic.Bool
	credentialSaved atomic.Bool
	saveWarningMu   sync.Mutex
	saveWarning     string
}

func (r *runtimeSSHSecret) setSaveWarning(msg string) {
	if r == nil || msg == "" {
		return
	}
	r.saveWarningMu.Lock()
	r.saveWarning = msg
	r.saveWarningMu.Unlock()
}

func (r *runtimeSSHSecret) getSaveWarning() string {
	if r == nil {
		return ""
	}
	r.saveWarningMu.Lock()
	defer r.saveWarningMu.Unlock()
	return r.saveWarning
}

// withRuntimeSSHSecret attaches an explicit runtime SSH credential to ctx for one call.
func withRuntimeSSHSecret(ctx context.Context, secretVal string, saveSecret bool) (context.Context, *runtimeSSHSecret) {
	carrier := &runtimeSSHSecret{
		secret:     secretVal,
		saveSecret: saveSecret,
	}
	return context.WithValue(ctx, runtimeSSHSecretKey{}, carrier), carrier
}

func runtimeSSHSecretFromContext(ctx context.Context) *runtimeSSHSecret {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(runtimeSSHSecretKey{}).(*runtimeSSHSecret)
	return v
}

// resolveSSHTunnel implements:
//  1. Pre-auth credential metadata save when saveSecret==true (before TryAcquireExisting & AcquireDialer,
//     re-checking configVer & existence under write lock, NEVER bumping configVer or closing a live tunnel).
//  2. Fast reuse of live pooled tunnel via TryAcquireExisting without demanding credentials.
//  3. Unified credential priority: explicit runtime secret > stored secret > SSHAuthPrompt.
func (s *ConnectionService) resolveSSHTunnel(
	ctx context.Context,
	connID string,
	cfgVer uint64,
	params dto.ConnectionParams,
) (tunnel.DialFunc, uint64, error) {
	sshDTO := params.SSH
	authMethod := normalizeSSHAuthMethod(sshDTO.AuthMethod)
	carrier := runtimeSSHSecretFromContext(ctx)

	// 1. Pre-auth save entry: runs BEFORE TryAcquireExisting and BEFORE SSH auth so checked
	// submissions always persist (even when reusing a live tunnel or if auth subsequently fails),
	// while skipping persistence if carrier is pinned to an older ConfigVersion.
	if carrier != nil && carrier.saveSecret && carrier.secret != "" && (authMethod == "password" || authMethod == "key") {
		if carrier.handledSave.CompareAndSwap(false, true) {
			if carrier.expectedCfgVer == 0 || carrier.expectedCfgVer == cfgVer {
				s.saveRuntimeSSHCredential(ctx, connID, cfgVer, authMethod, carrier.secret, carrier)
			}
		}
	}

	baseCfg := tunnel.SSHConfig{
		Host:           strings.TrimSpace(sshDTO.Host),
		Port:           sshDTO.Port,
		Username:       strings.TrimSpace(sshDTO.Username),
		AuthMethod:     authMethod,
		PrivateKeyPath: strings.TrimSpace(sshDTO.PrivateKeyPath),
		ConnectTimeout: time.Duration(params.ConnectTimeoutS) * time.Second,
	}
	epDigest := tunnel.EndpointDigest(baseCfg)

	// 2. Live tunnel fast-path reuse (zero password prompts while tunnel stays healthy).
	if dialFn, gen, ok := s.tunnelMgr.TryAcquireExisting(connID, cfgVer, epDigest); ok {
		return dialFn, gen, nil
	}

	// 3. No live tunnel -> resolve credentials with unified priority:
	//    (1) explicit runtime secret from ctx, (2) stored secret from secret.Store.
	sshCfg := baseCfg
	switch authMethod {
	case "password":
		if carrier != nil && carrier.secret != "" {
			sshCfg.Password = carrier.secret
		} else if s.secrets != nil {
			if stored, serr := s.secrets.Get(sshPasswordRef(connID)); serr == nil {
				sshCfg.Password = stored
			}
		}
	case "key":
		if carrier != nil && carrier.secret != "" {
			sshCfg.Passphrase = carrier.secret
		} else if s.secrets != nil {
			if stored, serr := s.secrets.Get(sshPassphraseRef(connID)); serr == nil {
				sshCfg.Passphrase = stored
			}
		}
	}

	dialFn, gen, err := s.tunnelMgr.AcquireDialer(ctx, connID, cfgVer, sshCfg)
	if err != nil {
		appErr := mapTunnelError(err, connID, sshCfg)
		attachSaveOutcomeToError(appErr, carrier)
		return nil, 0, appErr
	}
	return dialFn, gen, nil
}

// saveRuntimeSSHCredential updates the stored SSH credential & SaveCredential=true metadata
// under cfgMu.Lock(), strictly verifying configVer and DB row existence so concurrent edits or
// deletes can never be clobbered or resurrected. Does NOT bump configVer or close live tunnels.
func (s *ConnectionService) saveRuntimeSSHCredential(
	ctx context.Context,
	connID string,
	expectedCfgVer uint64,
	authMethod string,
	secretVal string,
	carrier *runtimeSSHSecret,
) {
	if s.secrets == nil {
		carrier.setSaveWarning("凭据存储未初始化，未能保存 SSH 密码")
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	if s.configVerLocked(connID) != expectedCfgVer {
		// Connection was concurrently edited or deleted; skip write-back cleanly.
		return
	}
	latestEnt, err := s.repo.GetConnection(ctx, connID)
	if err != nil {
		carrier.setSaveWarning("保存 SSH 凭据前读取连接配置失败: " + err.Error())
		return
	}
	if latestEnt == nil {
		// Connection was deleted; never resurrect it.
		return
	}
	var latestParams dto.ConnectionParams
	if err := json.Unmarshal([]byte(latestEnt.ConfigJSON), &latestParams); err != nil {
		carrier.setSaveWarning("保存 SSH 凭据前解析连接配置失败: " + err.Error())
		return
	}
	if latestParams.SSH == nil || !latestParams.SSH.Enabled || normalizeSSHAuthMethod(latestParams.SSH.AuthMethod) != authMethod {
		return
	}

	ref := sshPasswordRef(connID)
	if authMethod == "key" {
		ref = sshPassphraseRef(connID)
	}
	if err := s.secrets.Set(ref, secretVal); err != nil {
		carrier.setSaveWarning("保存 SSH 凭据到安全存储失败: " + err.Error())
		return
	}

	if !latestParams.SSH.SaveCredential {
		latestParams.SSH.SaveCredential = true
		cfgBytes, err := json.Marshal(latestParams)
		if err != nil {
			carrier.setSaveWarning("序列化连接配置失败，SSH 凭据状态可能未同步: " + err.Error())
			return
		}
		latestEnt.ConfigJSON = string(cfgBytes)
		latestEnt.UpdatedAt = time.Now().UnixMilli()
		if err := s.repo.UpsertConnection(ctx, *latestEnt); err != nil {
			carrier.setSaveWarning("更新连接保存标志失败: " + err.Error())
			return
		}
	}

	carrier.credentialSaved.Store(true)
}

// PrepareRuntimeSSHCredential persists an explicit runtime SSH credential when saveSecret==true
// BEFORE borrowing any idle DB connection or acquiring a tunnel. Re-checks configVer & DB row
// existence under write lock, NEVER bumps configVer or closes live tunnels, and runs at most once per request.
func (s *ConnectionService) PrepareRuntimeSSHCredential(ctx context.Context, connID string, carrier *runtimeSSHSecret) {
	if carrier == nil || !carrier.saveSecret || carrier.secret == "" || connID == "" {
		return
	}
	if !carrier.handledSave.CompareAndSwap(false, true) {
		return
	}
	s.cfgMu.RLock()
	ent, err := s.repo.GetConnection(ctx, connID)
	cfgVer := s.configVerLocked(connID)
	s.cfgMu.RUnlock()
	if err != nil {
		carrier.setSaveWarning("保存 SSH 凭据前读取连接配置失败: " + err.Error())
		return
	}
	if ent == nil {
		return
	}
	if carrier.expectedCfgVer > 0 && cfgVer != carrier.expectedCfgVer {
		// Connection configuration changed since the plan was created; never write secret to an edited host.
		return
	}
	var params dto.ConnectionParams
	if err := json.Unmarshal([]byte(ent.ConfigJSON), &params); err != nil {
		carrier.setSaveWarning("保存 SSH 凭据前解析连接配置失败: " + err.Error())
		return
	}
	if params.SSH == nil || !params.SSH.Enabled {
		return
	}
	authMethod := normalizeSSHAuthMethod(params.SSH.AuthMethod)
	if authMethod != "password" && authMethod != "key" {
		return
	}
	expectedVer := cfgVer
	if carrier.expectedCfgVer > 0 {
		expectedVer = carrier.expectedCfgVer
	}
	s.saveRuntimeSSHCredential(ctx, connID, expectedVer, authMethod, carrier.secret, carrier)
}

// TrustSSHHostKey atomically persists a user-confirmed SSH host key fingerprint.
func (s *ConnectionService) TrustSSHHostKey(ctx context.Context, hostPort, algorithm, fingerprint string) error {
	if s.tunnelMgr == nil {
		return dto.NewAppError(dto.ErrTunnelFailed, "SSH 隧道管理器未初始化", "")
	}
	if err := s.tunnelMgr.TrustHostKey(ctx, hostPort, algorithm, fingerprint); err != nil {
		return dto.NewAppError(dto.ErrInvalidArgument, "保存 SSH 主机指纹失败", err.Error())
	}
	return nil
}

func normalizeSSHAuthMethod(raw string) string {
	m := strings.ToLower(strings.TrimSpace(raw))
	if m == "" {
		return "password"
	}
	return m
}

func validateAndNormalizeSSH(eng dto.EngineType, raw *dto.SSHConfig) (*dto.SSHConfig, *dto.AppError) {
	if raw == nil || !raw.Enabled {
		return nil, nil
	}
	if eng == dto.EngineSQLite {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "SQLite 本地连接不支持配置 SSH 隧道", "")
	}
	cfg := &dto.SSHConfig{
		Enabled:        true,
		Host:           strings.TrimSpace(raw.Host),
		Port:           raw.Port,
		Username:       strings.TrimSpace(raw.Username),
		AuthMethod:     normalizeSSHAuthMethod(raw.AuthMethod),
		PrivateKeyPath: strings.TrimSpace(raw.PrivateKeyPath),
		SaveCredential: raw.SaveCredential,
	}
	if cfg.Host == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "SSH 跳板机主机地址不能为空", "")
	}
	if cfg.Port <= 0 {
		cfg.Port = 22
	}
	if cfg.Port > 65535 {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "SSH 端口必须在 1 到 65535 之间", "")
	}
	if cfg.Username == "" {
		return nil, dto.NewAppError(dto.ErrInvalidArgument, "SSH 用户名不能为空", "")
	}
	switch cfg.AuthMethod {
	case "password", "agent":
		cfg.PrivateKeyPath = ""
	case "key":
		if cfg.PrivateKeyPath == "" {
			return nil, dto.NewAppError(dto.ErrInvalidArgument, "私钥认证模式下 SSH 私钥路径不能为空", "")
		}
	default:
		return nil, dto.NewAppError(dto.ErrInvalidArgument, fmt.Sprintf("不支持的 SSH 认证方式: %s", raw.AuthMethod), "valid: password, key, agent")
	}
	return cfg, nil
}

func tunnelChallengeOrAuthAppError(err error, connID string) *dto.AppError {
	var challenge *tunnel.HostKeyChallengeError
	if errors.As(err, &challenge) {
		return &dto.AppError{
			Code:    dto.ErrTunnelFailed,
			Message: challenge.Error(),
			HostKeyChallenge: &dto.SSHHostKeyChallenge{
				HostPort:            challenge.HostPort,
				Algorithm:           challenge.Algorithm,
				Fingerprint:         challenge.Fingerprint,
				Mismatch:            challenge.Mismatch,
				PreviousFingerprint: challenge.PreviousFingerprint,
			},
		}
	}
	var authErr *tunnel.AuthFailedError
	if errors.As(err, &authErr) {
		detail := ""
		if authErr.Err != nil {
			detail = authErr.Err.Error()
		}
		return &dto.AppError{
			Code:    dto.ErrAuthFailed,
			Message: authErr.Error(),
			Detail:  detail,
			SSHAuthPrompt: &dto.SSHAuthPrompt{
				ConnectionID:   connID,
				HostPort:       authErr.HostPort,
				Username:       authErr.Username,
				AuthMethod:     authErr.AuthMethod,
				NeedPassphrase: authErr.NeedPassphrase,
				Reason:         authErr.Reason,
			},
		}
	}
	return nil
}

func mapTunnelError(err error, connID string, cfg tunnel.SSHConfig) *dto.AppError {
	if mapped := tunnelChallengeOrAuthAppError(err, connID); mapped != nil {
		return mapped
	}
	var ae *dto.AppError
	if errors.As(err, &ae) {
		return ae
	}
	return dto.NewAppError(dto.ErrTunnelFailed, fmt.Sprintf("建立 SSH 隧道失败 (%s): %s", tunnel.NormalizeHostPort(cfg.Host, cfg.Port), err.Error()), err.Error()).WithCause(err)
}

func attachSaveOutcomeToError(appErr *dto.AppError, carrier *runtimeSSHSecret) {
	if appErr == nil || carrier == nil {
		return
	}
	if carrier.credentialSaved.Load() {
		appErr.CredentialSaved = true
	}
	if w := carrier.getSaveWarning(); w != "" && appErr.SaveWarning == "" {
		appErr.SaveWarning = w
	}
}
