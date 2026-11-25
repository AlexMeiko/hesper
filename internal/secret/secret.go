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

const keyringService = "sql-manager"

// Store manages sensitive credentials (database passwords, SSH passphrases).
// Uses OS keyring (Windows Credential Manager / Linux Secret Service) first,
// and falls back gracefully to an AES-256-GCM encrypted local vault file when
// running in headless or minimal Linux WM environments without D-Bus Secret Service.
type Store struct {
	mu        sync.Mutex
	vaultPath string
	keyPath   string
	useFile   bool
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
		useFile:   true,
	}
}

func (s *Store) Set(ref, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.useFile {
		if err := keyring.Set(keyringService, ref, value); err == nil {
			return nil
		}
		// Secret Service unavailable (e.g., headless Linux / minimal WM); switch to encrypted local vault.
		s.useFile = true
	}
	return s.setFile(ref, value)
}

func (s *Store) Get(ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ref == "" {
		return "", nil
	}
	if !s.useFile {
		v, err := keyring.Get(keyringService, ref)
		if err == nil {
			return v, nil
		}
		if errors.Is(err, keyring.ErrNotFound) {
			// Also check fallback vault in case previously saved under fallback mode.
			if fv, ferr := s.getFile(ref); ferr == nil {
				return fv, nil
			}
			return "", nil
		}
		s.useFile = true
	}
	v, err := s.getFile(ref)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return v, err
}

func (s *Store) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ref == "" {
		return nil
	}
	if !s.useFile {
		_ = keyring.Delete(keyringService, ref)
	}
	return s.deleteFile(ref)
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

func (s *Store) getFile(ref string) (string, error) {
	m, err := s.loadVault()
	if err != nil {
		return "", err
	}
	return m[ref], nil
}

func (s *Store) deleteFile(ref string) error {
	m, err := s.loadVault()
	if err != nil {
		return err
	}
	delete(m, ref)
	return s.saveVault(m)
}
