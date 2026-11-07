// Package jsonscan finds where the plain text of a JSON string ends: the first
// byte that is not simply itself. A long string is most of what a document
// that has one costs, so where the machine can read 32 bytes at a time it
// does; elsewhere the caller's own loop, which reads a word, goes on from the
// start. Nothing here may be needed for an answer: each function says how far
// the text is known to be plain, which may be nowhere.
package jsonscan

// MinLength is the length of text worth the call: below it the loops of the
// caller, a word at a time, are as fast.
const MinLength = 64

// ASCII is how many of the first bytes of s are known to be none of a
// quotation mark, a backslash, a control character or a byte above ASCII,
// which on a machine with no fast way to tell is none of them. Where there is
// one, in text of MinLength or more, if that is less than len(s)-31, the byte
// there is the first that is one.
func ASCII(s string) int {
	if !fast || len(s) < MinLength {
		return 0
	}
	return ascii(s)
}

// Wide is ASCII for text that may be wider than ASCII: bytes above it are
// plain, but for other, which is a byte above ASCII or a quotation mark.
func Wide(s string, other byte) int {
	if !fast || len(s) < MinLength {
		return 0
	}
	return wide(s, other)
}
