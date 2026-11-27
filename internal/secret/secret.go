package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"
)

const keyringService = "hesper"

// Store manages sensitive credentials (database passwords, SSH passphrases).
// Uses OS keyring (Windows Credential Manager / Linux Secret Service) first,
// and falls back gracefully to an AES-256-GCM encrypted local vault file when
// running in headless or minimal Linux WM environments without D-Bus Secret Service.
type Store struct {
	mu                 sync.Mutex
	vaultPath          string
	keyPath            string
	fileOnly           bool // true ONLY when created via NewFileOnlyStore; never touches OS keyring
	keyringUnavailable bool // transient fallback state when OS keyring errored during Set/Get
	keyringSet         func(service, user, pass string) error
	keyringGet         func(service, user string) (string, error)
	keyringDelete      func(service, user string) error
}

// IsNotFound reports whether err represents a missing credential entry (safe to treat as already deleted).
func IsNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, keyring.ErrNotFound)
}

// PartialMutationError reports that the OS keyring was already modified before a subsequent
// local fallback vault cleanup failed. Callers with rollback logic must treat this ref as mutated.
type PartialMutationError struct {
	Op  string
	Err error
}

func (e *PartialMutationError) Error() string { return e.Err.Error() }
func (e *PartialMutationError) Unwrap() error { return e.Err }

// IsPartialMutation reports whether err indicates the OS keyring was already modified before failing.
func IsPartialMutation(err error) bool {
	var pm *PartialMutationError
	return errors.As(err, &pm)
}

// NewStore creates a credential store rooted at configDir for encrypted fallback.
func NewStore(configDir string) *Store {
	return &Store{
		vaultPath: filepath.Join(configDir, "vault.enc"),
		keyPath:   filepath.Join(configDir, ".vault.key"),
	}
}

// NewFileOnlyStore forces encrypted file storage (useful for isolated tests without touching host keyring).
func NewFileOnlyStore(configDir string) *Store {
	return &Store{
		vaultPath: filepath.Join(configDir, "vault.enc"),
		keyPath:   filepath.Join(configDir, ".vault.key"),
		fileOnly:  true,
	}
}

func (s *Store) Set(ref, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.fileOnly && !s.keyringUnavailable {
		setFn := s.keyringSet
		if setFn == nil {
			setFn = keyring.Set
		}
		if err := setFn(keyringService, ref, value); err == nil {
			// Clear any older fallback vault copy so Get never shadows the freshly written keyring value.
			if delErr := s.deleteFile(ref); delErr != nil {
				return &PartialMutationError{Op: "set", Err: delErr}
			}
			return nil
		}
		// Secret Service / keyring temporarily unavailable; fall back to encrypted local vault for writes.
		s.keyringUnavailable = true
	}
	return s.setFile(ref, value)
}

func (s *Store) Get(ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ref == "" {
		return "", nil
	}

	// Check local encrypted vault FIRST: any entry present in vault.enc is an unmigrated fallback
	// write newer than whatever older value may still sit in the OS keyring.
	fv, foundInFile, fileErr := s.getFileEntry(ref)
	if fileErr != nil {
		return "", fileErr
	}
	if foundInFile {
		if !s.fileOnly && !s.keyringUnavailable {
			// Opportunistically migrate the newer fallback value back to OS keyring, then remove
			// the local vault copy ONLY after keyring write succeeds.
			setFn := s.keyringSet
			if setFn == nil {
				setFn = keyring.Set
			}
			if err := setFn(keyringService, ref, fv); err == nil {
				_ = s.deleteFile(ref)
			} else {
				s.keyringUnavailable = true
			}
		}
		return fv, nil
	}

	if !s.fileOnly && !s.keyringUnavailable {
		getFn := s.keyringGet
		if getFn == nil {
			getFn = keyring.Get
		}
		v, err := getFn(keyringService, ref)
		if err == nil {
			return v, nil
		}
		if errors.Is(err, keyring.ErrNotFound) {
			return "", nil
		}
		s.keyringUnavailable = true
	}
	return "", nil
}

func (s *Store) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ref == "" {
		return nil
	}
	// Unless explicitly configured as fileOnly, ALWAYS attempt OS keyring deletion even if
	// keyringUnavailable was set earlier in this session — otherwise a secret saved in a prior
	// session could remain in the OS keyring and resurface on next startup.
	keyringDeleted := false
	if !s.fileOnly {
		delFn := s.keyringDelete
		if delFn == nil {
			delFn = keyring.Delete
		}
		err := delFn(keyringService, ref)
		if err == nil {
			s.keyringUnavailable = false
			keyringDeleted = true
		} else if errors.Is(err, keyring.ErrNotFound) {
			s.keyringUnavailable = false
		} else {
			s.keyringUnavailable = true
			// Keep any newer fallback vault entry intact until keyring deletion succeeds so a
			// stale keyring secret cannot resurface over a newer fallback secret on retry/recovery.
			return err
		}
	}
	if fileErr := s.deleteFile(ref); fileErr != nil {
		if keyringDeleted {
			return &PartialMutationError{Op: "delete", Err: fileErr}
		}
		return fileErr
	}
	return nil
}

func (s *Store) ensureKey() ([]byte, error) {
	if raw, err := os.ReadFile(s.keyPath); err == nil {
		key, decErr := base64.StdEncoding.DecodeString(string(raw))
		if decErr == nil && len(key) == 32 {
			return key, nil
		}
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.keyPath), 0o700); err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(s.keyPath, []byte(enc), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Store) loadVault() (map[string]string, error) {
	raw, err := os.ReadFile(s.vaultPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	key, err := s.ensureKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return map[string]string{}, nil
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) saveVault(m map[string]string) error {
	key, err := s.ensureKey()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	if err := os.MkdirAll(filepath.Dir(s.vaultPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.vaultPath, sealed, 0o600)
}

func (s *Store) setFile(ref, val string) error {
	m, err := s.loadVault()
	if err != nil {
		return err
	}
	m[ref] = val
	return s.saveVault(m)
}

func (s *Store) getFileEntry(ref string) (string, bool, error) {
	m, err := s.loadVault()
	if err != nil {
		return "", false, err
	}
	v, ok := m[ref]
	return v, ok, nil
}

func (s *Store) deleteFile(ref string) error {
	m, err := s.loadVault()
	if err != nil {
		return err
	}
	if _, exists := m[ref]; !exists {
		return nil
	}
	delete(m, ref)
	return s.saveVault(m)
}
