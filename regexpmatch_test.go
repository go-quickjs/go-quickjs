package quickjs_test

import "testing"

// TestRegExpCaseFoldingAgreesWithNode covers the i flag's comparison of a
// character, which is one of the characters listed for it rather than a
// fold of what is read: letters past ASCII with two and three forms and
// four (θ, ϑ, ϴ), the long s and the Kelvin sign, which only the u flag
// takes for s and k, the dotted and dotless i, which neither does, a
// character outside the Basic Multilingual Plane, a backreference, classes
// and a global search the first units of the pattern pass over text for.
// The answers are Node v26's.
func TestRegExpCaseFoldingAgreesWithNode(t *testing.T) {
	checkEval(t, `var K = String.fromCharCode(0x212A)
	JSON.stringify([
	  "Ärger ÜBER Öl".match(/über/iu)?.index, "Ärger ÜBER Öl".match(/über/i)?.index,
	  "ΣΊΣΥΦΟΣ".match(/σίσυφος/iu)?.[0], "ΣΊΣΥΦΟΣ".match(/σίσυφος/i)?.[0],
	  "ς".match(/Σ/i)?.[0], "ϑ".match(/θ/iu)?.[0], "ϴ".match(/θ/iu)?.[0], "ϴ".match(/θ/i)?.[0] ?? null,
	  K.match(/k/iu)?.index, K.match(/k/i), "ſ".match(/S/i), "ſ".match(/S/iu)?.index,
	  "ǅ".match(/ǆ/i)?.[0], "ẞ".match(/ß/iu)?.[0] ?? null, "ẞ".match(/ß/i)?.[0] ?? null,
	  "xxİ".match(/i/iu), "ı".match(/I/i), "ı".match(/I/iu),
	  "café CAFÉ".match(/É/gi), String.fromCodePoint(0x10400).match(new RegExp(String.fromCodePoint(0x10428), "iu"))?.index,
	  "aBc".replace(/b/gi, "-"), "xyzK".search(/k/i), ("xyz" + K).search(/k/i), ("xyz" + K).search(/k/iu),
	  /(é)\1/i.test("éÉ"), /(é)\1/iu.test("éÉ"), "ÀÉÎ".replace(/[à-ï]/gi, "."), "ÀÉÎ".replace(/[à-ï]/giu, "."),
	  "Straße STRASSE".match(/strasse/gi), "ﬀ FF".match(/ﬀ/giu), "µ Μ μ".match(/µ/gi), "µ Μ μ".match(/µ/giu),
	  "abcÅÅå".match(/å+/giu)?.[0], "İı".match(/[İ]/giu)?.[0],
	  "naïve café ΣΊΣΥΦΟΣ".match(/café|σίσυφος/giu), "ΣΊΣΥΦΟΣ naïve".replace(/ü|ï|σ/giu, "_"),
	])`, `[6,6,"ΣΊΣΥΦΟΣ","ΣΊΣΥΦΟΣ","ς","ϑ","ϴ",null,0,null,null,0,"ǅ","ẞ",null,null,null,null,["é","É"],0,"a-c",3,-1,3,true,true,"...","...",["STRASSE"],["ﬀ"],["µ","Μ","μ"],["µ","Μ","μ"],"ÅÅå","İ",["café","ΣΊΣΥΦΟΣ"],"_Ί_ΥΦΟ_ na_ve"]`)
}
