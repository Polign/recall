//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachAttr starts the local server outside the CLI's console and process
// group so it outlives the CLI invocation that launched it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
