package vm

import (
	"runtime"
	"strings"
	"testing"
)

// TestBigIntStringLimit pins V8's limit on a string converted to a BigInt:
// 2^24 parts of 64 bits, each of as many whole digits as fit, its leading
// zeros not counted. A string past it is no BigInt, as a malformed one is
// not.
func TestBigIntStringLimit(t *testing.T) {
	// Each string at a limit is a block of 268 to 352 MB. The test makes
	// them one at a time, each let go and collected before the next, and
	// the largest first, so that each fits where the one before it was:
	// its peak is one block. Made all at once, a base's four strings and
	// the last base's garbage had 1.5 GB in use, and a windows/386
	// process, whose 2 GB of address space hold the program and its DLLs
	// too, found no room for the next 352 MB block.
	heavyAlone(t)
	for _, c := range []struct {
		prefix string
		digit  string
		base   int
		max    int
	}{
		{"0o", "7", 8, 21 << 24},
		{"", "9", 10, 19 << 24},
		{"0x", "f", 16, 16 << 24},
	} {
		withDigits(t, "000", c.digit, c.max, "", func(zeros string) {
			if bigStringTooLong(zeros[3:], c.base) {
				t.Errorf("base %d: %d digits refused", c.base, c.max)
			}
			if bigStringTooLong(zeros, c.base) {
				t.Errorf("base %d: %d digits after leading zeros refused", c.base, c.max)
			}
		})
		converted := func(s string) {
			if b, ok := StringToBigInt(s); ok || b != nil {
				t.Errorf("base %d: %d characters converted", c.base, len(s))
			}
		}
		withDigits(t, c.prefix, c.digit, c.max+1, "", converted)
		if c.base == 10 {
			withDigits(t, " -", c.digit, c.max, "1 ", converted)
		}
	}
}

// withDigits calls f with head, n of digit and tail, and collects the
// string once f is done with it.
func withDigits(t *testing.T, head, digit string, n int, tail string, f func(string)) {
	t.Helper()
	var b strings.Builder
	b.Grow(len(head) + n*len(digit) + len(tail))
	b.WriteString(head)
	chunk := strings.Repeat(digit, 1<<16)
	for ; n >= 1<<16; n -= 1 << 16 {
		b.WriteString(chunk)
	}
	b.WriteString(chunk[:n*len(digit)])
	b.WriteString(tail)
	f(b.String())
	b = strings.Builder{}
	runtime.GC()
}
