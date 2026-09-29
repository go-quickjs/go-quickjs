package regexp

import "testing"

// TestASCIIClassesAgree pins that the bitmap a program answers a class's
// ASCII members from says what the class itself does, under every flag that
// changes membership.
func TestASCIIClassesAgree(t *testing.T) {
	patterns := []string{
		`[a-z]`, `[^a-z]`, `[A-Z_0-9]`, `\d`, `\D`, `\w`, `\W`, `\s`, `\S`, `.`,
		`[ſ]`, `[K]`, `[^ſ]`, `[k]`, `[^k]`, `[\W\d]`, `[^\W]`,
		`\p{L}`, `\P{L}`, `\p{ASCII_Hex_Digit}`, `[\p{Lu}--[A-C]]`, `[[a-z]&&[aeiou]]`,
	}
	for _, src := range patterns {
		for _, flags := range []string{"", "i", "u", "iu", "v", "iv"} {
			re, err := Compile(src, flags)
			if err != nil {
				continue
			}
			p := re.prog
			for i, s := range p.classes {
				for r := rune(0); r < 128; r++ {
					got := p.asciiClasses[i][r>>6]&(1<<(r&63)) != 0
					if want := s.contains(r, p.unicodeFold); got != want {
						t.Errorf("/%s/%s class %d: %q is %v in the bitmap, %v in the class", src, flags, i, r, got, want)
					}
				}
			}
		}
	}
}
