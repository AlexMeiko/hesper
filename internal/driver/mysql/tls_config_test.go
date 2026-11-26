package mysql

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
)

func TestConfigureTLSUsesDirectConfigAndStrictFallback(t *testing.T) {
	for _, mode := range []string{"disable", "prefer", "require", "verify-ca", "verify-full"} {
		mc := mysqldriver.NewConfig()
		if err := configureTLS(mc, dto.TLSConfig{Mode: mode}, "mysql.internal"); err != nil {
			t.Fatalf("configureTLS(%s): %v", mode, err)
		}
		if mc.TLSConfig != "" {
			t.Fatalf("mode=%s should not use global RegisterTLSConfig name, got %q", mode, mc.TLSConfig)
		}
		if mode == "disable" {
			if mc.TLS != nil || mc.AllowFallbackToPlaintext {
				t.Fatalf("disable must have TLS=nil and AllowFallbackToPlaintext=false")
			}
			continue
		}
		if mc.TLS == nil {
			t.Fatalf("mode=%s must populate mc.TLS directly", mode)
		}
		wantFallback := (mode == "prefer")
		if mc.AllowFallbackToPlaintext != wantFallback {
			t.Fatalf("mode=%s AllowFallbackToPlaintext=%v, want %v", mode, mc.AllowFallbackToPlaintext, wantFallback)
		}
		if mode == "verify-full" && mc.TLS.ServerName != "mysql.internal" {
			t.Fatalf("verify-full ServerName=%q, want mysql.internal", mc.TLS.ServerName)
		}
	}
}

// TestStrictTLSRefusesServerWithoutSSL starts a minimal MySQL v10 handshake stub without
// CLIENT_SSL capability and verifies strict TLS modes fail before sending auth credentials.
func TestStrictTLSRefusesServerWithoutSSL(t *testing.T) {
	// Minimal MySQL protocol v10 initial handshake packet with CLIENT_PROTOCOL_41 (0x0200)
	// and WITHOUT CLIENT_SSL (0x0800).
	buildHandshake := func() []byte {
		var payload []byte
		payload = append(payload, 10)                       // protocol version 10
		payload = append(payload, []byte("8.4.0-stub\x00")...)
		payload = append(payload, 1, 0, 0, 0)               // connection id
		payload = append(payload, []byte("12345678")...)    // auth-plugin-data-part-1
		payload = append(payload, 0)                        // filler
		payload = append(payload, 0x00, 0x02)               // capability flags lower (CLIENT_PROTOCOL_41 only, NO CLIENT_SSL)
		payload = append(payload, 33)                       // charset
		payload = append(payload, 0x02, 0x00)               // status flags
		payload = append(payload, 0x00, 0x00)               // capability flags upper
		payload = append(payload, 0)                        // auth plugin data len
		payload = append(payload, make([]byte, 10)...)      // reserved
		pkt := make([]byte, 4+len(payload))
		pkt[0] = byte(len(payload))
		pkt[1] = byte(len(payload) >> 8)
		pkt[2] = byte(len(payload) >> 16)
		pkt[3] = 0 // seq 0
		copy(pkt[4:], payload)
		return pkt
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				_, _ = conn.Write(buildHandshake())
				buf := make([]byte, 64)
				_, _ = conn.Read(buf)
			}(c)
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	opener := NewOpener(dto.EngineMySQL)

	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := opener.Open(ctx, driver.ResolvedConfig{
			Engine:   dto.EngineMySQL,
			Host:     host,
			Port:     port,
			Username: "root",
			TLS:      dto.TLSConfig{Mode: mode},
		})
		cancel()
		if err == nil {
			t.Fatalf("mode=%s MUST fail when server does not advertise CLIENT_SSL", mode)
		}
	}
}
