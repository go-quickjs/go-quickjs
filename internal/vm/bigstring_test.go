package vm

import (
	"strings"
	"testing"
)

// TestBigIntStringLimit pins V8's limit on a string converted to a BigInt:
// 2^24 parts of 64 bits, each of as many whole digits as fit, its leading
// zeros not counted. A string past it is no BigInt, as a malformed one is
// not.
func TestBigIntStringLimit(t *testing.T) {
	for _, c := range []struct {
		prefix string
		digit  string
		base   int
		max    int
	}{
		{"0x", "f", 16, 16 << 24},
		{"", "9", 10, 19 << 24},
		{"0o", "7", 8, 21 << 24},
	} {
		at := strings.Repeat(c.digit, c.max)
		if bigStringTooLong(at, c.base) {
			t.Errorf("base %d: %d digits refused", c.base, c.max)
		}
		if bigStringTooLong("000"+at, c.base) {
			t.Errorf("base %d: %d digits after leading zeros refused", c.base, c.max)
		}
		over := []string{c.prefix + at + c.digit}
		if c.base == 10 {
			over = append(over, " -"+at+"1 ")
		}
		for _, s := range over {
			if b, ok := StringToBigInt(s); ok || b != nil {
				t.Errorf("base %d: %d characters converted", c.base, len(s))
			}
		}
	}
}
