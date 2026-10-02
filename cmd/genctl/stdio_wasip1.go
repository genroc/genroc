//go:build wasip1

package main

import "syscall"

// A WASI host may hand over NON-BLOCKING stdio (Node on Linux); Go then returns EAGAIN rather than
// waiting, ending an LSP session one message in. specs/language-server.md §4.
func blockStdio() {
	for _, fd := range []int{0, 1, 2} {
		_ = syscall.SetNonblock(fd, false)
	}
}
