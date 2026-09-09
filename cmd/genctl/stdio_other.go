//go:build !wasip1

package main

// Every other host spawns a process with blocking stdio; there is nothing to clear.
func blockStdio() {}
