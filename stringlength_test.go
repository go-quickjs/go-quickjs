package quickjs_test

import "testing"

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
	} {
		t.Run(name, func(t *testing.T) {
			checkEval(t, `try { `+src+`; "made" } catch (e) { e.name + ": " + e.message }`,
				"RangeError: Invalid string length")
		})
	}
	// Up to the limit is a string like any other.
	checkEval(t, `"xy".repeat(1000).padEnd(3000, "z").length`, "3000")
}
