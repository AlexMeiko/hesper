//go:build windows

package tunnel

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/crypto/ssh/agent"
)

const windowsOpenSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

type winNamedPipe struct {
	mu     sync.Mutex
	handle syscall.Handle
	closed bool
}

func (p *winNamedPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	h := p.handle
	closed := p.closed
	p.mu.Unlock()
	if closed || h == syscall.InvalidHandle {
		return 0, io.EOF
	}
	var done uint32
	if err := syscall.ReadFile(h, b, &done, nil); err != nil {
		return int(done), err
	}
	return int(done), nil
}

func (p *winNamedPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	h := p.handle
	closed := p.closed
	p.mu.Unlock()
	if closed || h == syscall.InvalidHandle {
		return 0, syscall.EINVAL
	}
	var done uint32
	if err := syscall.WriteFile(h, b, &done, nil); err != nil {
		return int(done), err
	}
	return int(done), nil
}

func (p *winNamedPipe) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	h := p.handle
	p.handle = syscall.InvalidHandle
	p.mu.Unlock()
	if h != syscall.InvalidHandle {
		// CancelIoEx best-effort unblocks any in-flight synchronous ReadFile/WriteFile on the handle.
		cancelPipeIO(h)
		return syscall.CloseHandle(h)
	}
	return nil
}

var (
	modKernel32   = syscall.NewLazyDLL("kernel32.dll")
	procCancelIOX = modKernel32.NewProc("CancelIoEx")
)

func cancelPipeIO(h syscall.Handle) {
	if err := procCancelIOX.Find(); err == nil {
		_, _, _ = procCancelIOX.Call(uintptr(h), uintptr(unsafe.Pointer(nil)))
	}
}

// openSSHAgent opens Windows OpenSSH Authentication Agent named pipe without CGO.
// Callers MUST keep the returned io.Closer open until ssh.NewClientConn completes.
func openSSHAgent() (agent.ExtendedAgent, io.Closer, error) {
	namePtr, err := syscall.UTF16PtrFromString(windowsOpenSSHAgentPipe)
	if err != nil {
		return nil, nil, err
	}
	handle, err := syscall.CreateFile(
		namePtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, nil, fmt.Errorf("未检测到 Windows OpenSSH Agent 服务 (%s)，请先启动 ssh-agent 服务: %w", windowsOpenSSHAgentPipe, err)
		}
		return nil, nil, fmt.Errorf("打开 Windows OpenSSH Agent 命名管道失败: %w", err)
	}
	pipe := &winNamedPipe{handle: handle}
	return agent.NewClient(pipe), pipe, nil
}
