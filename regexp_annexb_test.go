package quickjs_test

import "testing"

// TestRegExpAnnexBEscapes pins the escapes Annex B gives a meaning outside
// unicode mode: a reference to a group the pattern does not have, and a \c
// followed by anything but a letter.
func TestRegExpAnnexBEscapes(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// \2 with one group is the octal escape \x02, and \8 the digit.
		{`String(/\b(\w+) \2\b/.test("the the"))`, "false"},
		{`[/(a)\2/.test("a\x02"), /\8/.test("8"), /(a)\1/.test("aa")].join()`, "true,true,true"},
		{`[/\k<a>\1/.test("k<a>\x01"), /\1(b)\k<a>/.test("bk<a>")].join()`, "true,true"},
		// A stray \c is a backslash, and the c is still there after it.
		{`[/\c /.test("\\c "), /\c /.test("c "), /\cа/.test("\\cа")].join()`, "true,false,true"},
		{`var re = /[\c ]/; [re.test("\\"), re.test("c"), re.test(" ")].join()`, "true,true,true"},
		// In a class a digit or an underscore makes a control character.
		{`[/[\c1]/.test("\x11"), /[\c_]/.test("\x1f")].join()`, "true,true"},
		// Under the u flag each is an error.
		{`var errs = []; for (var p of ["(a)\\2", "\\c "]) { try { new RegExp(p, "u") } catch (e) { errs.push(e.constructor.name) } } errs.join()`,
			"SyntaxError,SyntaxError"},
	})
}

// TestRegExpUnboundedCountOfEmpty pins that {n,} over a body that can match
// nothing stops repeating once an iteration past the minimum matches nothing,
// as * does, rather than going round until the step budget runs out.
func TestRegExpUnboundedCountOfEmpty(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`[/.(?=Z){2,}/.exec("a bZ cZZ")[0], /.(?=Z){2,}?/.exec("a bZ")[0], /.(?!Z){3,}/.exec("aZ b")[0]].join()`, "b,b,Z"},
		{`JSON.stringify([/(a|){2,}b/.exec("aab"), /(a|){3,}b/.exec("ab"), /(){2,}x/.exec("x")])`,
			`[["aab","a"],["ab",""],["x",""]]`},
	})
}
