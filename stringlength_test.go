package quickjs_test

import (
	"runtime"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestStringLengthLimit pins V8's limit on a string's length, 2^29 - 24
// code units, and its message, however the string would have been made: a
// string doubled again and again used to wrap its length round, or end the
// process out of memory when it was written out (KI-03).
func TestStringLengthLimit(t *testing.T) {
	for name, src := range map[string]string{
		"doubling":    `let s = "ab".repeat(100); for (let i = 0; i < 70; i++) s += s; s`,
		"repeat":      `"x".repeat(2 ** 29)`,
		"padStart":    `"x".padStart(Infinity)`,
		"padEnd":      `"x".padEnd(1e300, "ab")`,
		"concat":      `let s = "x".repeat(2 ** 26); s.concat(s, s, s, s, s, s, s, s)`,
		"join":        `new Array(2 ** 12).fill("x".repeat(2 ** 18)).join("")`,
		"replaceAll":  `"x".repeat(2 ** 12).replaceAll("", "y".repeat(2 ** 18))`,
		"template":    "const s = \"x\".repeat(2 ** 26); `${s}${s}${s}${s}${s}${s}${s}${s}${s}`",
		"String.raw":  `const s = "x".repeat(2 ** 26); String.raw({raw: [s, s, s, s, s]}, s, s, s, s)`,
		"typed join":  `new Uint8Array(2 ** 12).join("y".repeat(2 ** 18))`,
		"regexp repl": `"x".repeat(2 ** 12).replace(/x/g, "y".repeat(2 ** 18))`,
		// Past the limit only with what comes after the last check: the
		// separators between holes, which contribute nothing themselves, and
		// a replace's last replacement and the tail after it. Each made a
		// string longer than any may be, where V8 throws.
		"join holes":         `new Array(8193).join("x".repeat(65536))`,
		"regexp repl last":   `"ab".repeat(8192).replace(/a/g, "x".repeat(65536))`,
		"regexp repl last $": `"ab".repeat(8192).replace(/a/g, "$&" + "x".repeat(65535))`,
		// Each U+FDFA is eighteen characters by compatibility: the limit is
		// found before anything is built.
		"normalize": `"\uFDFA".repeat(3e7).normalize("NFKD")`,
	} {
		t.Run(name, func(t *testing.T) {
			// What the last case built is let go of first: on a 32-bit
			// platform the two need not fit together.
			runtime.GC()
			checkEval(t, `try { `+src+`; "made" } catch (e) { e.name + ": " + e.message }`,
				"RangeError: Invalid string length")
		})
	}
	// Up to the limit is a string like any other.
	checkEval(t, `"xy".repeat(1000).padEnd(3000, "z").length`, "3000")

	// A runtime with a memory limit replaces by the general path, not the
	// one that knows the result's length before it writes it.
	for _, src := range []string{
		`"ab".repeat(8192).replace(/a/g, "x".repeat(65536))`,
		`"ab".repeat(8192).replace(/a/g, "$&" + "x".repeat(65535))`,
	} {
		runtime.GC()
		rt := quickjs.New(quickjs.WithMemoryLimit(8 << 30))
		v, err := rt.Eval(`try { ` + src + `; "made" } catch (e) { e.name + ": " + e.message }`)
		if err != nil || v.String() != "RangeError: Invalid string length" {
			t.Errorf("with a memory limit, %s = %v, %v", src, v, err)
		}
		rt.Close()
	}
}
