//go:build !linux

package platform

// InitGraphicsEnv is a no-op on non-Linux operating systems.
func InitGraphicsEnv(_ []string, _ string) {}
