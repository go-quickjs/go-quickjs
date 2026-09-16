// Package wtf8 encodes and decodes JavaScript strings that contain lone
// surrogates.
//
// A JavaScript string is a sequence of UTF-16 code units with no well-formedness
// requirement, so "\uD83D" -- the high half of an emoji with nothing after it --
// is a perfectly ordinary one-code-unit string. Slicing between the halves of a
// surrogate pair produces one, String.fromCharCode can produce one directly, and
// a script may compare, concatenate and index them like any other string.
//
// Go's string type is UTF-8, and Go's UTF-8 machinery refuses to represent a
// surrogate: utf8.EncodeRune and strings.Builder.WriteRune both substitute
// U+FFFD, and ranging over a string decodes a surrogate's bytes as U+FFFD. An
// engine that stores JavaScript strings as Go strings must therefore encode
// surrogates itself.
//
// WTF-8 is that encoding. It is UTF-8 extended to allow the three-byte form for
// the surrogate range U+D800..U+DFFF, which is exactly the encoding Go's
// decoders reject. Because the surrogate range is otherwise unused in UTF-8,
// well-formed text is unaffected: WTF-8 and UTF-8 agree on every string that
// contains no lone surrogate.
package wtf8

import (
	"unicode/utf16"
	"unicode/utf8"
)

const (
	surrogateMin = 0xD800
	surrogateMax = 0xDFFF
	// surrHighMin and surrLowMin bound the two halves of a pair.
	surrHighMin = 0xD800
	surrHighMax = 0xDBFF
	surrLowMin  = 0xDC00
	surrLowMax  = 0xDFFF
)

// IsSurrogate reports whether r is in the surrogate range.
func IsSurrogate(r rune) bool { return r >= surrogateMin && r <= surrogateMax }

// RuneLen returns the number of bytes needed to encode r, including the
// three-byte form for a surrogate.
func RuneLen(r rune) int {
	if IsSurrogate(r) {
		return 3
	}
	n := utf8.RuneLen(r)
	if n < 0 {
		// An out-of-range value encodes as the replacement character.
		return 3
	}
	return n
}

// AppendRune appends the encoding of r to dst.
func AppendRune(dst []byte, r rune) []byte {
	if IsSurrogate(r) {
		// The ordinary three-byte UTF-8 layout, applied to a value Go's encoder
		// would reject.
		return append(dst,
			byte(0xE0|(r>>12)),
			byte(0x80|((r>>6)&0x3F)),
			byte(0x80|(r&0x3F)))
	}
	return utf8.AppendRune(dst, r)
}

// DecodeRune decodes the code point at the start of s, recognizing the
// three-byte encoding of a surrogate.
//
// It returns utf8.RuneError with a size of 1 for invalid input, matching
// utf8.DecodeRuneInString so that callers can treat the two alike.
func DecodeRune(s string) (r rune, size int) {
	if len(s) == 0 {
		return utf8.RuneError, 0
	}
	if s[0] < utf8.RuneSelf {
		return rune(s[0]), 1
	}
	if r, ok := decodeSurrogateAt(s, 0); ok {
		return r, 3
	}
	return utf8.DecodeRuneInString(s)
}

// decodeSurrogateAt reports whether a surrogate is encoded at s[i:].
//
// The three-byte encoding of U+D800..U+DFFF always begins with 0xED followed by
// a continuation byte in 0xA0..0xBF, which is what distinguishes it from every
// well-formed three-byte sequence.
func decodeSurrogateAt(s string, i int) (rune, bool) {
	if i+2 >= len(s) || s[i] != 0xED {
		return 0, false
	}
	b1, b2 := s[i+1], s[i+2]
	if b1 < 0xA0 || b1 > 0xBF || b2 < 0x80 || b2 > 0xBF {
		return 0, false
	}
	return 0xD000 | rune(b1&0x3F)<<6 | rune(b2&0x3F), true
}

// DecodeSurrogateAt exposes the surrogate check for callers that scan a string
// themselves.
func DecodeSurrogateAt(s string, i int) (rune, bool) { return decodeSurrogateAt(s, i) }

// ToUTF16 converts a WTF-8 string to UTF-16 code units, which is the
// representation JavaScript semantics are defined over.
func ToUTF16(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			out = append(out, uint16(c))
			i++
			continue
		}
		if r, ok := decodeSurrogateAt(s, i); ok {
			out = append(out, uint16(r))
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			out = append(out, uint16(hi), uint16(lo))
		} else {
			out = append(out, uint16(r))
		}
		i += size
	}
	return out
}

// FromUTF16 converts UTF-16 code units to WTF-8.
//
// A valid surrogate pair is combined into a single code point, so that a string
// built from well-formed halves is ordinary UTF-8; an unpaired half is encoded
// on its own rather than replaced.
func FromUTF16(u []uint16) string {
	buf := make([]byte, 0, len(u))
	for i := 0; i < len(u); i++ {
		c := rune(u[i])
		if c >= surrHighMin && c <= surrHighMax && i+1 < len(u) {
			if lo := rune(u[i+1]); lo >= surrLowMin && lo <= surrLowMax {
				buf = utf8.AppendRune(buf, utf16.DecodeRune(c, lo))
				i++
				continue
			}
		}
		buf = AppendRune(buf, c)
	}
	return string(buf)
}

// Count returns the number of UTF-16 code units a WTF-8 string encodes, which
// is what JavaScript reports as its length.
func Count(s string) int {
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			n++
			i++
			continue
		}
		if _, ok := decodeSurrogateAt(s, i); ok {
			// An encoded surrogate is one code unit despite being three bytes.
			n++
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > 0xFFFF {
			// A code point outside the basic multilingual plane is a pair.
			n += 2
		} else {
			n++
		}
		i += size
	}
	return n
}

// IsASCII reports whether every byte is below 0x80, in which case byte offsets
// and code-unit offsets coincide and indexing needs no conversion.
func IsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// WellFormed reports whether s contains no lone surrogate, which is what
// String.prototype.isWellFormed tests.
func WellFormed(s string) bool {
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		if _, ok := decodeSurrogateAt(s, i); ok {
			return false
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return true
}

// ToWellFormed replaces every lone surrogate with U+FFFD, which is what
// String.prototype.toWellFormed does.
func ToWellFormed(s string) string {
	if WellFormed(s) {
		return s
	}
	buf := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			buf = append(buf, s[i])
			i++
			continue
		}
		if _, ok := decodeSurrogateAt(s, i); ok {
			buf = utf8.AppendRune(buf, utf8.RuneError)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		buf = utf8.AppendRune(buf, r)
		i += size
	}
	return string(buf)
}
