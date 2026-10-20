package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestNegativeBigIntLiteral pins a negated BigInt literal folded into one
// constant, written in decimal: hexadecimal, octal and binary ones too,
// whose prefixed digits could take no sign, and -0n, which is 0n.
func TestNegativeBigIntLiteral(t *testing.T) {
	cases := []struct{ src, want string }{
		{`-1n`, "-1"},
		{`-0n`, "0"},
		{`-0x10n`, "-16"},
		{`-0o17n`, "-15"},
		{`-0b101n`, "-5"},
		{`-1_000n`, "-1000"},
		{`-123456789012345678901234567890n`, "-123456789012345678901234567890"},
	}
	for _, tc := range cases {
		prog, err := parser.Parse(tc.src, parser.Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		main, err := Compile(prog, Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		var consts []string
		for _, in := range main.Code {
			if in.Op == bytecode.OpNeg {
				t.Errorf("%s: negated at run time", tc.src)
			}
			if in.Op == bytecode.OpPushConst && main.Constants[in.A].Kind == bytecode.ConstBigInt {
				consts = append(consts, main.Constants[in.A].Str)
			}
		}
		if len(consts) != 1 || consts[0] != tc.want {
			t.Errorf("%s: BigInt constants %q, want [%q]", tc.src, consts, tc.want)
		}
	}
}
