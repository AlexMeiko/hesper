package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const knownHostKVPrefix = "ssh:known_host:"

// KVStore is the minimal persistence interface satisfied by repository.LocalDB.
type KVStore interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, valueJSON string) error
}

// KnownHostEntry persists one trusted SSH host key fingerprint per canonical host:port.
type KnownHostEntry struct {
	HostPort    string `json:"hostPort"`
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
	TrustedAt   int64  `json:"trustedAt"`
}

// KnownHostsStore implements TOFU host key verification using atomic per-host app_kv entries.
type KnownHostsStore struct {
	kv KVStore
}

func NewKnownHostsStore(kv KVStore) *KnownHostsStore {
	return &KnownHostsStore{kv: kv}
}

func knownHostKey(normalizedHostPort string) string {
	return knownHostKVPrefix + normalizedHostPort
}

// Lookup reads the trusted entry for one canonical host:port.
func (s *KnownHostsStore) Lookup(ctx context.Context, hostPort string) (*KnownHostEntry, bool, error) {
	if s == nil || s.kv == nil {
		return nil, false, errors.New("known hosts store is not initialized")
	}
	norm := NormalizeHostPortString(hostPort)
	raw, found, err := s.kv.GetKV(ctx, knownHostKey(norm))
	if err != nil || !found {
		return nil, found, err
	}
	var entry KnownHostEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return nil, false, fmt.Errorf("decode known host entry for %s: %w", norm, err)
	}
	return &entry, true, nil
}

// TrustHostKey atomically persists a user-confirmed SHA-256 host key fingerprint for hostPort.
func (s *KnownHostsStore) TrustHostKey(ctx context.Context, hostPort, algorithm, fingerprint string) error {
	if s == nil || s.kv == nil {
		return errors.New("known hosts store is not initialized")
	}
	norm := NormalizeHostPortString(hostPort)
	fp := strings.TrimSpace(fingerprint)
	if norm == "" || fp == "" {
		return errors.New("hostPort and fingerprint are required")
	}
	entry := KnownHostEntry{
		HostPort:    norm,
		Algorithm:   strings.TrimSpace(algorithm),
		Fingerprint: fp,
		TrustedAt:   time.Now().UnixMilli(),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return s.kv.SetKV(ctx, knownHostKey(norm), string(data))
}

const defaultHostKeyLookupTimeout = 5 * time.Second

// HostKeyCallback returns an ssh.HostKeyCallback enforcing TOFU + mismatch protection.
// lifetimeCtx must remain valid for the lifetime of the SSH tunnel (not just the initial handshake)
// so periodic SSH key re-exchanges (rekey) continue verifying host keys without failing with context.Canceled.
func (s *KnownHostsStore) HostKeyCallback(lifetimeCtx context.Context, normalizedHostPort string) ssh.HostKeyCallback {
	norm := NormalizeHostPortString(normalizedHostPort)
	if lifetimeCtx == nil {
		lifetimeCtx = context.Background()
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		lookupCtx, cancel := context.WithTimeout(lifetimeCtx, defaultHostKeyLookupTimeout)
		defer cancel()

		fp := ssh.FingerprintSHA256(key)
		alg := key.Type()
		entry, found, err := s.Lookup(lookupCtx, norm)
		if err != nil {
			return fmt.Errorf("read ssh known host %s: %w", norm, err)
		}
		if !found {
			return &HostKeyChallengeError{
				HostPort:    norm,
				Algorithm:   alg,
				Fingerprint: fp,
				Mismatch:    false,
			}
		}
		if entry.Fingerprint != fp {
			return &HostKeyChallengeError{
				HostPort:            norm,
				Algorithm:           alg,
				Fingerprint:         fp,
				Mismatch:            true,
				PreviousFingerprint: entry.Fingerprint,
			}
		}
		return nil
	}
}
