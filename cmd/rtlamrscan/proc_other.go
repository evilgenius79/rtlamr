//go:build !windows

package main

// On non-Windows platforms child processes are cleaned up through context
// cancellation alone.
func killChildrenOnExit() {}
