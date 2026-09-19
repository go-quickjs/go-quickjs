package vm

import "github.com/go-quickjs/go-quickjs/internal/normalize"

func normalizeString(s, form string) string {
	return normalize.String(s, form)
}

func combiningClass(r rune) int { return normalize.CombiningClass(r) }
