//go:build !windows

package tunnel

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh/agent"
)

// openSSHAgent connects to the local Unix SSH Agent socket ($SSH_AUTH_SOCK).
// Callers MUST keep the returned io.Closer open until ssh.NewClientConn completes.
func openSSHAgent() (agent.ExtendedAgent, io.Closer, error) {
	sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if sock == "" {
		return nil, nil, errors.New("未检测到 SSH_AUTH_SOCK 环境变量，请先启动 ssh-agent")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("连接 SSH Agent (%s) 失败: %w", sock, err)
	}
	return agent.NewClient(conn), conn, nil
}
