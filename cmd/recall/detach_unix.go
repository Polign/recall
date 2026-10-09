//go:build !windows

package main

import "syscall"

// detachAttr starts the local server in its own session so it outlives the
// CLI invocation that launched it.
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
