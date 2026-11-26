package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// SSHConfig holds internal decrypted or runtime-supplied SSH bastion parameters.
// It is NEVER serialized into app.db or returned to the frontend.
type SSHConfig struct {
	Host           string
	Port           int
	Username       string
	AuthMethod     string // password | key | agent
	PrivateKeyPath string
	Password       string
	Passphrase     string
	ConnectTimeout time.Duration
}

// NormalizeHostPort returns a canonical host:port key for SSH Known Hosts and tunnel digests
// (lowercased hostname, bracketed IPv6, port defaulting to 22).
func NormalizeHostPort(host string, port int) string {
	h := strings.TrimSpace(host)
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	h = strings.ToLower(h)
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(h, strconv.Itoa(port))
}

// NormalizeHostPortString canonicalizes a raw "host" or "host:port" string.
func NormalizeHostPortString(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if h, pStr, err := net.SplitHostPort(trimmed); err == nil {
		p, perr := strconv.Atoi(pStr)
		if perr == nil && p > 0 {
			return NormalizeHostPort(h, p)
		}
		return NormalizeHostPort(h, 22)
	}
	return NormalizeHostPort(trimmed, 22)
}

// EndpointDigest returns a stable digest over the non-secret SSH endpoint & auth identity.
// Deliberately excludes SaveCredential, Password, and Passphrase so credential-metadata save
// updates never invalidate an already-authenticated live tunnel.
func EndpointDigest(cfg SSHConfig) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s",
		NormalizeHostPort(cfg.Host, cfg.Port),
		strings.TrimSpace(cfg.Username),
		strings.ToLower(strings.TrimSpace(cfg.AuthMethod)),
		strings.TrimSpace(cfg.PrivateKeyPath),
	)
	return hex.EncodeToString(h.Sum(nil))
}

// AuthDigest hashes the credential inputs so concurrent handshakes carrying different
// passwords or passphrases never coalesce onto each other.
func AuthDigest(cfg SSHConfig) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s",
		strings.ToLower(strings.TrimSpace(cfg.AuthMethod)),
		strings.TrimSpace(cfg.PrivateKeyPath),
		cfg.Password,
		cfg.Passphrase,
	)
	return hex.EncodeToString(h.Sum(nil))
}

// HostKeyChallengeError is returned when an SSH server presents an unknown or changed host key.
type HostKeyChallengeError struct {
	HostPort            string
	Algorithm           string
	Fingerprint         string
	Mismatch            bool
	PreviousFingerprint string
}

func (e *HostKeyChallengeError) Error() string {
	if e.Mismatch {
		return fmt.Sprintf("SSH 主机 %s 的指纹已变更（原: %s，现: %s）", e.HostPort, e.PreviousFingerprint, e.Fingerprint)
	}
	return fmt.Sprintf("首次连接 SSH 主机 %s，需确认主机指纹 %s", e.HostPort, e.Fingerprint)
}

// AuthFailedError is returned when SSH authentication fails or requires a missing password/passphrase.
type AuthFailedError struct {
	HostPort       string
	Username       string
	AuthMethod     string
	NeedPassphrase bool
	Reason         string
	Err            error
}

func (e *AuthFailedError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	if e.Err != nil {
		return fmt.Sprintf("SSH 认证失败 (%s@%s): %v", e.Username, e.HostPort, e.Err)
	}
	return fmt.Sprintf("SSH 认证失败 (%s@%s)", e.Username, e.HostPort)
}

func (e *AuthFailedError) Unwrap() error {
	return e.Err
}
