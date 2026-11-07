//go:build (!amd64 && !arm64) || purego

package jsonscan

const fast = false

func ascii(s string) int { return 0 }

func wide(s string, other byte) int { return 0 }
