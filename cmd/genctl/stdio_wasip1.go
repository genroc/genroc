//go:build wasip1

package main

import "syscall"

// A WASI host may hand the guest NON-BLOCKING stdio: Node's does on Linux and does not on
// macOS. Go then surfaces a read with nothing waiting as EAGAIN instead of waiting for it,
// which ends an LSP session one message in — and a write that fills the pipe would end it the
// same way. Clearing the flag is the whole fix. specs/language-server.md §4.
func blockStdio() {
	for _, fd := range []int{0, 1, 2} {
		_ = syscall.SetNonblock(fd, false)
	}
}
