package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// regTypeofLeaf recognizes a parameter's or upvalue's typeof comparison with a type
// name. Its descriptor holds RJTypeof, the type and equality sense, the
// source index, and an upvalue flag in bit 32.
func regTypeofLeaf(fn *bytecode.Function, rc *bytecode.RegCode) uint64 {
	if rc.Leaf != bytecode.LeafPure || rc.PureCalls || len(rc.Code) < 5 {
		return 0
	}
	code := rc.Code
	var up uint64
	var source uint16
	if w := code[0]; w.Op() == bytecode.RGetUpval && w.BC() <= 0xffff && len(code) >= 6 {
		up, source = 1<<32, uint16(w.BC())
		code = code[1:]
		if code[0].A() != w.A() {
			return 0
		}
	}
	w := code[0]
	if w.Op() != bytecode.RJTypeof || w.X()&^bytecode.RCmpTrue >= uint8(len(typeofNames)) || w.SC() != 2 ||
		up == 0 && (w.A() < 2 || int(w.A()) >= 2+rc.Params) {
		return 0
	}
	f, j, t, ret := code[1], code[2], code[3], code[4]
	if f.Op() != bytecode.RLoadFalse || j.Op() != bytecode.RJmp || int32(j.BC()) != 1 ||
		t.Op() != bytecode.RLoadTrue || t.A() != f.A() || ret.Op() != bytecode.RRet || ret.A() != f.A() {
		return 0
	}
	if up == 0 {
		source = w.A() - 2
	}
	return uint64(bytecode.RJTypeof) | uint64(w.X())<<8 | uint64(source)<<16 | up
}
