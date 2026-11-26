package driver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"sql-manager/internal/dto"
)

// NormalizeTLSMode canonicalizes a TLS mode string, defaulting empty to "disable".
func NormalizeTLSMode(raw string) string {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return "disable"
	}
	return mode
}

// ValidateTLSConfig validates TLS mode and certificate path combinations so unsupported or
// ineffective settings are rejected explicitly instead of being silently ignored.
func ValidateTLSConfig(cfg dto.TLSConfig) *dto.AppError {
	mode := NormalizeTLSMode(cfg.Mode)
	caPath := strings.TrimSpace(cfg.CACertPath)
	certPath := strings.TrimSpace(cfg.CertPath)
	keyPath := strings.TrimSpace(cfg.KeyPath)

	switch mode {
	case "disable", "prefer", "require", "verify-ca", "verify-full":
	default:
		return dto.NewAppError(
			dto.ErrInvalidArgument,
			fmt.Sprintf("不支持的 TLS 模式: %s", cfg.Mode),
			"valid modes: disable, prefer, require, verify-ca, verify-full",
		)
	}

	if (certPath == "") != (keyPath == "") {
		return dto.NewAppError(
			dto.ErrInvalidArgument,
			"客户端证书路径与私钥路径必须同时填写",
			"both certPath and keyPath are required for mTLS",
		)
	}

	if mode == "disable" {
		if caPath != "" || certPath != "" || keyPath != "" {
			return dto.NewAppError(
				dto.ErrInvalidArgument,
				"TLS 关闭（disable）模式下不能配置 CA 证书或客户端证书",
				"certificate paths are not permitted when tls.mode=disable",
			)
		}
		return nil
	}

	if (mode == "prefer" || mode == "require") && caPath != "" {
		return dto.NewAppError(
			dto.ErrInvalidArgument,
			fmt.Sprintf("TLS %s 模式不校验 CA 证书链，请切换至 verify-ca / verify-full 或清空 CA 证书路径", mode),
			"caCertPath is only valid for verify-ca and verify-full",
		)
	}

	return nil
}

// BuildTLSConfig builds a shared *tls.Config for MySQL/MariaDB and PostgreSQL.
// Protocol-level plaintext fallback for "prefer" is owned independently by each driver.
func BuildTLSConfig(cfg dto.TLSConfig, serverName string) (*tls.Config, error) {
	if appErr := ValidateTLSConfig(cfg); appErr != nil {
		return nil, appErr
	}

	mode := NormalizeTLSMode(cfg.Mode)
	if mode == "disable" {
		return nil, nil
	}

	caPath := strings.TrimSpace(cfg.CACertPath)
	certPath := strings.TrimSpace(cfg.CertPath)
	keyPath := strings.TrimSpace(cfg.KeyPath)

	var clientCerts []tls.Certificate
	if certPath != "" && keyPath != "" {
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, dto.NewAppError(dto.ErrInvalidArgument, "加载客户端 TLS 证书或私钥失败", err.Error())
		}
		clientCerts = []tls.Certificate{pair}
	}

	switch mode {
	case "prefer", "require":
		return &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true,
			Certificates:       clientCerts,
		}, nil

	case "verify-ca":
		roots, err := loadRootCAs(caPath, true)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // Hostname verification is intentionally skipped; CA chain is verified below.
			RootCAs:            roots,
			Certificates:       clientCerts,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("tls server presented no certificates")
				}
				intermediates := x509.NewCertPool()
				for _, cert := range cs.PeerCertificates[1:] {
					intermediates.AddCert(cert)
				}
				_, verifyErr := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
					Roots:         roots,
					Intermediates: intermediates,
				})
				return verifyErr
			},
		}, nil

	case "verify-full":
		roots, err := loadRootCAs(caPath, false)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: false,
			ServerName:         strings.TrimSpace(serverName),
			RootCAs:            roots,
			Certificates:       clientCerts,
		}, nil
	}

	return nil, nil
}

func loadRootCAs(caPath string, requirePool bool) (*x509.CertPool, error) {
	if caPath != "" {
		pemBytes, err := os.ReadFile(caPath)
		if err != nil {
			return nil, dto.NewAppError(dto.ErrInvalidArgument, "读取 TLS CA 证书文件失败", err.Error())
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, dto.NewAppError(dto.ErrInvalidArgument, "TLS CA 证书 PEM 格式无效", caPath)
		}
		return pool, nil
	}
	if !requirePool {
		// Nil RootCAs instructs crypto/tls to use the host system certificate pool.
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, dto.NewAppError(dto.ErrConnectionFailed, "加载系统根证书池失败", err.Error())
	}
	return pool, nil
}
