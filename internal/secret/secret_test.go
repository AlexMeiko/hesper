package secret

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestStoreDeleteNotFoundSucceedsAndRealErrorsSurface(t *testing.T) {
	// 1. File-only store: deleting nonexistent key succeeds without creating vault.enc.
	dir := t.TempDir()
	s := NewFileOnlyStore(dir)
	if err := s.Delete("conn:missing:ssh_password"); err != nil {
		t.Fatalf("delete nonexistent key should succeed, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.enc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleting nonexistent key should not create vault.enc, stat err=%v", err)
	}

	// 2. File-only store: set then delete removes entry cleanly.
	if err := s.Set("conn:1:ssh_password", "top-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete("conn:1:ssh_password"); err != nil {
		t.Fatalf("Delete existing key: %v", err)
	}
	if got, err := s.Get("conn:1:ssh_password"); err != nil || got != "" {
		t.Fatalf("expected empty after delete, got %q err=%v", got, err)
	}

	// 3. Keyring store: keyring.ErrNotFound is treated as success, real keyring error is returned!
	sKeyring := NewStore(t.TempDir())
	sKeyring.keyringDelete = func(_, _ string) error {
		return keyring.ErrNotFound
	}
	if err := sKeyring.Delete("conn:1:ssh_password"); err != nil {
		t.Fatalf("keyring ErrNotFound should be treated as success, got %v", err)
	}

	realErr := errors.New("keyring locked by user")
	sKeyring.keyringDelete = func(_, _ string) error {
		return realErr
	}
	if err := sKeyring.Delete("conn:1:ssh_password"); !errors.Is(err, realErr) {
		t.Fatalf("expected real keyring deletion error to be propagated, got %v", err)
	}

	// 4. File-only store: real write failure while deleting an existing entry surfaces error.
	roDir := t.TempDir()
	sRO := NewFileOnlyStore(roDir)
	if err := sRO.Set("conn:ro:ssh_password", "stuck-secret"); err != nil {
		t.Fatalf("Set ro: %v", err)
	}
	vaultPath := filepath.Join(roDir, "vault.enc")
	if err := os.Chmod(vaultPath, 0o400); err != nil {
		t.Fatalf("chmod vault read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(vaultPath, 0o600) })
	// Only assert when non-root (root bypasses file permissions).
	if os.Geteuid() != 0 {
		if err := sRO.Delete("conn:ro:ssh_password"); err == nil {
			t.Fatalf("expected file write error when deleting existing secret from read-only vault.enc")
		}
	}
}

func TestStoreDeleteStillTargetsKeyringAfterTransientFallbackAndSkipsWhenFileOnly(t *testing.T) {
	keyringData := map[string]string{}
	keyringDown := false
	errLocked := errors.New("os keyring temporarily locked")

	s := NewStore(t.TempDir())
	s.keyringSet = func(_, user, pass string) error {
		if keyringDown {
			return errLocked
		}
		keyringData[user] = pass
		return nil
	}
	s.keyringGet = func(_, user string) (string, error) {
		if keyringDown {
			return "", errLocked
		}
		v, ok := keyringData[user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	deleteAttempts := 0
	s.keyringDelete = func(_, user string) error {
		deleteAttempts++
		if keyringDown {
			return errLocked
		}
		if _, ok := keyringData[user]; !ok {
			return keyring.ErrNotFound
		}
		delete(keyringData, user)
		return nil
	}

	// 1. Save secret while OS keyring is healthy.
	const ref = "conn:kr-1:ssh_password"
	if err := s.Set(ref, "old-keyring-pw"); err != nil {
		t.Fatalf("Set in keyring: %v", err)
	}
	if keyringData[ref] != "old-keyring-pw" {
		t.Fatalf("expected secret in mock keyring")
	}

	// 2. OS keyring becomes temporarily unavailable -> Get falls back to file and sets keyringUnavailable.
	keyringDown = true
	if _, err := s.Get(ref); err != nil {
		t.Fatalf("fallback Get should succeed with empty file value, got %v", err)
	}
	if !s.keyringUnavailable {
		t.Fatalf("expected keyringUnavailable=true after transient keyring failure")
	}

	// 3. Delete MUST STILL target OS keyring even after transient fallback and return the real error
	//    instead of silently succeeding while old-keyring-pw stays alive in OS keyring!
	if err := s.Delete(ref); !errors.Is(err, errLocked) {
		t.Fatalf("expected Delete to still attempt keyring and return %v, got %v", errLocked, err)
	}
	if deleteAttempts != 1 {
		t.Fatalf("expected 1 keyring delete attempt, got %d", deleteAttempts)
	}
	if keyringData[ref] != "old-keyring-pw" {
		t.Fatalf("old secret should remain in keyring while locked")
	}

	// 4. Once keyring recovers, Delete clears the old keyring secret and restores keyring availability.
	keyringDown = false
	if err := s.Delete(ref); err != nil {
		t.Fatalf("Delete after keyring recovery failed: %v", err)
	}
	if _, stillThere := keyringData[ref]; stillThere {
		t.Fatalf("expected keyring secret to be deleted once keyring recovered")
	}
	if s.keyringUnavailable {
		t.Fatalf("expected keyringUnavailable=false after successful keyring delete")
	}

	// 5. Explicitly file-only store NEVER touches OS keyring on Delete.
	fileOnly := NewFileOnlyStore(t.TempDir())
	fileOnlyCalled := false
	fileOnly.keyringDelete = func(_, _ string) error {
		fileOnlyCalled = true
		return errors.New("must not be called")
	}
	if err := fileOnly.Delete(ref); err != nil {
		t.Fatalf("fileOnly Delete should succeed without touching keyring, got %v", err)
	}
	if fileOnlyCalled {
		t.Fatalf("NewFileOnlyStore must NEVER call OS keyring on Delete")
	}
}

func TestStoreFallbackOverwriteNeverReadsBackStaleKeyringAndMigratesOnRecovery(t *testing.T) {
	dir := t.TempDir()
	keyringData := map[string]string{}
	keyringDown := false
	errDown := errors.New("os keyring unavailable")

	attachMockKeyring := func(s *Store) {
		s.keyringSet = func(_, user, pass string) error {
			if keyringDown {
				return errDown
			}
			keyringData[user] = pass
			return nil
		}
		s.keyringGet = func(_, user string) (string, error) {
			if keyringDown {
				return "", errDown
			}
			v, ok := keyringData[user]
			if !ok {
				return "", keyring.ErrNotFound
			}
			return v, nil
		}
		s.keyringDelete = func(_, user string) error {
			if keyringDown {
				return errDown
			}
			if _, ok := keyringData[user]; !ok {
				return keyring.ErrNotFound
			}
			delete(keyringData, user)
			return nil
		}
	}

	const ref = "conn:overwrite:ssh_password"
	s1 := NewStore(dir)
	attachMockKeyring(s1)

	// 1. Save "A" in healthy OS keyring.
	if err := s1.Set(ref, "A"); err != nil {
		t.Fatalf("Set A: %v", err)
	}

	// 2. Keyring temporarily down -> overwrite with "B" falls back to encrypted file and succeeds.
	keyringDown = true
	if err := s1.Set(ref, "B"); err != nil {
		t.Fatalf("Set B fallback: %v", err)
	}
	if keyringData[ref] != "A" {
		t.Fatalf("expected stale A still in mock keyring while down")
	}

	// 3. Keyring recovers and deleting another secret clears keyringUnavailable -> Get(ref) MUST return "B"
	//    (never stale "A"!) and migrate "B" into OS keyring before deleting the fallback copy!
	keyringDown = false
	if err := s1.Delete("conn:other:ssh_password"); err != nil {
		t.Fatalf("Delete other ref: %v", err)
	}
	got, err := s1.Get(ref)
	if err != nil || got != "B" {
		t.Fatalf("expected newer fallback value B after keyring recovery, got %q err=%v", got, err)
	}
	if keyringData[ref] != "B" {
		t.Fatalf("expected B to be migrated into OS keyring, keyring has %q", keyringData[ref])
	}
	if _, stillInFile, _ := s1.getFileEntry(ref); stillInFile {
		t.Fatalf("expected fallback vault copy to be cleaned up AFTER successful keyring migration")
	}

	// 4. Simulate app restart with stale "B" in keyring and newer "C" written while keyring was down:
	keyringDown = true
	if err := s1.Set(ref, "C"); err != nil {
		t.Fatalf("Set C fallback: %v", err)
	}
	// If Delete fails while keyring is down, "C" MUST stay in vault so stale "B" cannot resurface!
	if err := s1.Delete(ref); !errors.Is(err, errDown) {
		t.Fatalf("expected Delete error while keyring down, got %v", err)
	}

	// Restart into a brand-new Store instance with keyring recovered -> MUST read "C", NOT stale "B"!
	keyringDown = false
	s2 := NewStore(dir)
	attachMockKeyring(s2)
	gotAfterRestart, err := s2.Get(ref)
	if err != nil || gotAfterRestart != "C" {
		t.Fatalf("expected C after restart + keyring recovery, got %q err=%v", gotAfterRestart, err)
	}
	if keyringData[ref] != "C" {
		t.Fatalf("expected C migrated into OS keyring on restart, got %q", keyringData[ref])
	}
}

func TestStorePartialMutationErrorOnVaultFailureAfterKeyringChange(t *testing.T) {
	dir := t.TempDir()
	keyringData := map[string]string{}

	s := NewStore(dir)
	s.keyringSet = func(_, user, pass string) error {
		keyringData[user] = pass
		return nil
	}
	s.keyringGet = func(_, user string) (string, error) {
		v, ok := keyringData[user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	s.keyringDelete = func(_, user string) error {
		if _, ok := keyringData[user]; !ok {
			return keyring.ErrNotFound
		}
		delete(keyringData, user)
		return nil
	}

	const ref = "conn:pm:ssh_password"
	// Make vaultPath a directory so loadVault() deterministically returns a real read error on deleteFile.
	if err := os.Mkdir(s.vaultPath, 0o700); err != nil {
		t.Fatalf("mkdir vaultPath: %v", err)
	}

	// 1. Set writes to OS keyring, then deleteFile fails -> MUST report IsPartialMutation(err)==true.
	err := s.Set(ref, "written-in-keyring")
	if err == nil || !IsPartialMutation(err) {
		t.Fatalf("expected PartialMutationError when Set wrote keyring then deleteFile failed, got %v", err)
	}
	if keyringData[ref] != "written-in-keyring" {
		t.Fatalf("expected keyring to have been mutated")
	}

	// 2. Delete removes ref from OS keyring, then deleteFile fails -> MUST report IsPartialMutation(err)==true.
	err = s.Delete(ref)
	if err == nil || !IsPartialMutation(err) {
		t.Fatalf("expected PartialMutationError when Delete removed keyring entry then deleteFile failed, got %v", err)
	}
	if _, exists := keyringData[ref]; exists {
		t.Fatalf("expected keyring entry to have been removed before deleteFile failed")
	}

	// 3. Delete when ref is NOT in OS keyring (ErrNotFound) and deleteFile fails -> NOT a keyring partial mutation.
	err = s.Delete(ref)
	if err == nil {
		t.Fatalf("expected vault read error from Delete")
	}
	if IsPartialMutation(err) {
		t.Fatalf("expected IsPartialMutation==false when keyring returned ErrNotFound, got %v", err)
	}
}
