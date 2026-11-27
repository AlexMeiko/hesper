package service

import (
	"context"
	"encoding/json"
	"fmt"

	"hesper/internal/dto"
	"hesper/internal/entity"
	"hesper/internal/secret"
)

// SecretStore is the minimal credential store contract satisfied by *secret.Store.
type SecretStore interface {
	Get(ref string) (string, error)
	Set(ref, value string) error
	Delete(ref string) error
}

func dbPasswordRef(connID string) string    { return "conn:" + connID + ":password" }
func sshPasswordRef(connID string) string   { return "conn:" + connID + ":ssh_password" }
func sshPassphraseRef(connID string) string { return "conn:" + connID + ":ssh_passphrase" }

const pendingSecretCleanupKVPrefix = "pending_secret_cleanup:"

func (s *ConnectionService) deleteSecretChecked(ref, msg string) *dto.AppError {
	if s.secrets == nil || ref == "" {
		return nil
	}
	if err := s.secrets.Delete(ref); err != nil && !secret.IsNotFound(err) {
		return dto.NewAppError(dto.ErrInternal, msg, err.Error())
	}
	return nil
}

func (s *ConnectionService) hasStoredSecret(ref string) bool {
	if s.secrets == nil || ref == "" {
		return false
	}
	v, err := s.secrets.Get(ref)
	return err == nil && v != ""
}

type secretBeforeState struct {
	ref     string
	existed bool
	value   string
	mutated bool
}

// secretUndoLog records pre-mutation existence & value ONLY for secrets touched by one SaveConnection
// call so any failure before UpsertConnection completes can restore credentials to their exact pre-save state.
type secretUndoLog struct {
	pendingRefs map[string]bool
	byRef       map[string]*secretBeforeState
	order       []*secretBeforeState
}

func newSecretUndoLog(pendingRefs map[string]bool) *secretUndoLog {
	return &secretUndoLog{
		pendingRefs: pendingRefs,
		byRef:       make(map[string]*secretBeforeState),
	}
}

func (u *secretUndoLog) snapshotBeforeMutate(secrets SecretStore, ref string) error {
	if secrets == nil || ref == "" {
		return nil
	}
	if _, ok := u.byRef[ref]; ok {
		return nil
	}
	prev, err := secrets.Get(ref)
	if err != nil && !secret.IsNotFound(err) {
		return err
	}
	// Refs previously recorded in pendingRefs are unowned orphans from an earlier failed rollback
	// and must be deleted (not restored) if this save also fails.
	st := &secretBeforeState{
		ref:     ref,
		existed: !u.pendingRefs[ref] && prev != "",
		value:   prev,
	}
	u.byRef[ref] = st
	u.order = append(u.order, st)
	return nil
}

func (u *secretUndoLog) recordMutated(ref string) {
	if st, ok := u.byRef[ref]; ok {
		st.mutated = true
	}
}

func (u *secretUndoLog) wipe() {
	for _, st := range u.order {
		st.value = ""
	}
}

func pendingCleanupKVKey(connID string) string {
	return pendingSecretCleanupKVPrefix + connID
}

func (s *ConnectionService) pendingCleanupRefsLocked(ctx context.Context, connID string) map[string]bool {
	out := make(map[string]bool)
	if connID == "" {
		return out
	}
	if mem, ok := s.pendingSecretCleanup[connID]; ok {
		for ref := range mem {
			out[ref] = true
		}
	}
	if s.repo != nil {
		if raw, found, err := s.repo.GetKV(ctx, pendingCleanupKVKey(connID)); err == nil && found && raw != "" {
			var persisted []string
			if json.Unmarshal([]byte(raw), &persisted) == nil {
				for _, ref := range persisted {
					if ref != "" {
						out[ref] = true
					}
				}
			}
		}
	}
	return out
}

func (s *ConnectionService) persistPendingCleanupLocked(ctx context.Context, connID string, refs map[string]struct{}) {
	if s.repo == nil || connID == "" {
		return
	}
	if len(refs) == 0 {
		_ = s.repo.DeleteKV(ctx, pendingCleanupKVKey(connID))
		return
	}
	list := make([]string, 0, len(refs))
	for ref := range refs {
		list = append(list, ref)
	}
	if b, err := json.Marshal(list); err == nil {
		_ = s.repo.SetKV(ctx, pendingCleanupKVKey(connID), string(b))
	}
}

func (s *ConnectionService) clearPendingCleanupLocked(ctx context.Context, connID string) {
	delete(s.pendingSecretCleanup, connID)
	if s.repo != nil && connID != "" {
		_ = s.repo.DeleteKV(ctx, pendingCleanupKVKey(connID))
	}
}

// rollbackSecretUndoLocked restores every secret mutated during a failed SaveConnection back to its
// exact pre-save state: previously absent secrets are deleted (recording any failed Delete in
// pendingSecretCleanup for retry), and previously present secrets are restored with their original values.
func (s *ConnectionService) rollbackSecretUndoLocked(ctx context.Context, connID string, undo *secretUndoLog) []string {
	if s.secrets == nil || undo == nil || len(undo.order) == 0 {
		return nil
	}
	var errs []string
	var failedDeleteRefs []string
	var cleanedDeleteRefs []string

	for i := len(undo.order) - 1; i >= 0; i-- {
		st := undo.order[i]
		if !st.mutated || st.ref == "" {
			continue
		}
		if st.existed {
			if err := s.secrets.Set(st.ref, st.value); err != nil {
				errs = append(errs, fmt.Sprintf("restore %s: %v", st.ref, err))
			}
		} else {
			if err := s.secrets.Delete(st.ref); err != nil && !secret.IsNotFound(err) {
				errs = append(errs, fmt.Sprintf("delete %s: %v", st.ref, err))
				failedDeleteRefs = append(failedDeleteRefs, st.ref)
			} else {
				cleanedDeleteRefs = append(cleanedDeleteRefs, st.ref)
			}
		}
	}

	if connID != "" && (len(failedDeleteRefs) > 0 || len(cleanedDeleteRefs) > 0) {
		existing := s.pendingCleanupRefsLocked(ctx, connID)
		if len(existing) > 0 || len(failedDeleteRefs) > 0 {
			set := make(map[string]struct{}, len(existing)+len(failedDeleteRefs))
			for ref := range existing {
				set[ref] = struct{}{}
			}
			for _, ref := range cleanedDeleteRefs {
				delete(set, ref)
			}
			for _, ref := range failedDeleteRefs {
				set[ref] = struct{}{}
			}
			if len(set) == 0 {
				s.clearPendingCleanupLocked(ctx, connID)
			} else {
				s.pendingSecretCleanup[connID] = set
				s.persistPendingCleanupLocked(ctx, connID, set)
			}
		}
	}
	return errs
}

// existingSSHCredentialFlags reports which SSH credential refs could plausibly exist in secret.Store
// based on the previously persisted connection entity.
// - Brand-new connections, SQLite connections, and non-SSH connections cannot hold SSH credentials.
// - Corrupt ConfigJSON fails closed (returns true, true) so cleanup is never mistakenly skipped.
// - Does NOT rely on SaveCredential == false to skip cleanup, because a prior partial save could
//   have written the secret before updating ConfigJSON.
func existingSSHCredentialFlags(existing *entity.ConnectionEntity) (mightHavePassword, mightHavePassphrase bool) {
	if existing == nil || dto.EngineType(existing.Driver) == dto.EngineSQLite {
		return false, false
	}
	var params dto.ConnectionParams
	if err := json.Unmarshal([]byte(existing.ConfigJSON), &params); err != nil {
		return true, true
	}
	if params.SSH == nil || !params.SSH.Enabled {
		return false, false
	}
	switch normalizeSSHAuthMethod(params.SSH.AuthMethod) {
	case "password":
		return true, false
	case "key":
		return false, true
	case "agent":
		return false, false
	default:
		return true, true
	}
}

func (s *ConnectionService) setSecretWithUndo(undo *secretUndoLog, ref, val, readErrMsg, writeErrMsg string) *dto.AppError {
	if s.secrets == nil || ref == "" {
		return nil
	}
	if err := undo.snapshotBeforeMutate(s.secrets, ref); err != nil {
		return dto.NewAppError(dto.ErrInternal, readErrMsg, err.Error())
	}
	if err := s.secrets.Set(ref, val); err != nil {
		if secret.IsPartialMutation(err) {
			// OS keyring was already updated before fallback file cleanup failed; include in rollback!
			undo.recordMutated(ref)
		}
		return dto.NewAppError(dto.ErrInternal, writeErrMsg, err.Error())
	}
	undo.recordMutated(ref)
	return nil
}

func (s *ConnectionService) deleteSecretWithUndo(undo *secretUndoLog, ref, readErrMsg, delErrMsg string) *dto.AppError {
	if s.secrets == nil || ref == "" {
		return nil
	}
	if err := undo.snapshotBeforeMutate(s.secrets, ref); err != nil {
		return dto.NewAppError(dto.ErrInternal, readErrMsg, err.Error())
	}
	if err := s.secrets.Delete(ref); err != nil && !secret.IsNotFound(err) {
		if secret.IsPartialMutation(err) {
			// OS keyring entry was already removed before fallback file cleanup failed; include in rollback!
			undo.recordMutated(ref)
		}
		return dto.NewAppError(dto.ErrInternal, delErrMsg, err.Error())
	}
	undo.recordMutated(ref)
	return nil
}

func (s *ConnectionService) syncSSHCredentialsLocked(
	id string,
	existing *entity.ConnectionEntity,
	pendingRefs map[string]bool,
	undo *secretUndoLog,
	req dto.SaveConnectionRequest,
) *dto.AppError {
	if s.secrets == nil {
		return nil
	}
	isCreate := existing == nil
	cfgMightHavePw, cfgMightHavePp := existingSSHCredentialFlags(existing)
	pwRef := sshPasswordRef(id)
	ppRef := sshPassphraseRef(id)
	mightHavePw := cfgMightHavePw || pendingRefs[pwRef]
	mightHavePp := cfgMightHavePp || pendingRefs[ppRef]

	setWithUndo := func(ref, val, errMsg string) *dto.AppError {
		return s.setSecretWithUndo(undo, ref, val, "读取原 SSH 凭据状态失败", errMsg)
	}
	deleteWithUndo := func(ref, errMsg string) *dto.AppError {
		return s.deleteSecretWithUndo(undo, ref, "读取原 SSH 凭据状态失败", errMsg)
	}

	if req.Params.SSH == nil || !req.Params.SSH.Enabled || !req.Params.SSH.SaveCredential {
		if mightHavePw {
			if err := deleteWithUndo(pwRef, "清除已保存的 SSH 密码失败"); err != nil {
				return err
			}
		}
		if mightHavePp {
			if err := deleteWithUndo(ppRef, "清除已保存的 SSH 私钥口令失败"); err != nil {
				return err
			}
		}
		return nil
	}

	authMethod := normalizeSSHAuthMethod(req.Params.SSH.AuthMethod)
	switch authMethod {
	case "password":
		if mightHavePp {
			if err := deleteWithUndo(ppRef, "清除已保存的 SSH 私钥口令失败"); err != nil {
				return err
			}
		}
		if isCreate || req.UpdateSSHPassword {
			if req.SSHPassword != "" {
				if err := setWithUndo(pwRef, req.SSHPassword, "保存 SSH 密码到安全存储失败"); err != nil {
					return err
				}
			} else if mightHavePw {
				if err := deleteWithUndo(pwRef, "清除已保存的 SSH 密码失败"); err != nil {
					return err
				}
			}
		}
	case "key":
		if mightHavePw {
			if err := deleteWithUndo(pwRef, "清除已保存的 SSH 密码失败"); err != nil {
				return err
			}
		}
		if isCreate || req.UpdateSSHPassphrase {
			if req.SSHPassphrase != "" {
				if err := setWithUndo(ppRef, req.SSHPassphrase, "保存 SSH 私钥口令到安全存储失败"); err != nil {
					return err
				}
			} else if mightHavePp {
				if err := deleteWithUndo(ppRef, "清除已保存的 SSH 私钥口令失败"); err != nil {
					return err
				}
			}
		}
	case "agent":
		if mightHavePw {
			if err := deleteWithUndo(pwRef, "清除已保存的 SSH 密码失败"); err != nil {
				return err
			}
		}
		if mightHavePp {
			if err := deleteWithUndo(ppRef, "清除已保存的 SSH 私钥口令失败"); err != nil {
				return err
			}
		}
	}
	return nil
}
