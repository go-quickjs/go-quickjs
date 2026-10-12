//go:build quickjs_jit && !android && !ios && linux

package jit

// Fresh immutable code published after mprotect needs no explicit cache
// maintenance on x86-64. Windows also calls FlushInstructionCache as required
// by its executable-memory API.
func flushCode([]byte) error { return nil }

func executablePolicy() error { return nil }
