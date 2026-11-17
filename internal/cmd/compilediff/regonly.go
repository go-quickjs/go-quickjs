package main

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register-only output (-reg): each function's fields but those of
// the stack form, and its register code with every operand that indexes a
// table of the function's written as what it names -- a name as its
// string, a constant as its value, a nested function by its place among
// the function's, a cache site by the order of its first use and its name
// -- and each instruction's source position. Two compiles that number
// their names, constants and sites differently, or one with a stack form
// and one without, write the same output where their register code does
// the same. Fields are read by reflection where one side may lack them,
// so that the tool builds over either.

// stackFields are the fields of bytecode.Function the register-only output
// leaves out: the stack form's code and what is numbered by it, and the
// tables the register code's operands are written through instead.
var stackFields = map[string]bool{
	"Code": true, "Lines": true, "MaxStack": true, "ParamEnd": true,
	"PropSites": true, "PrivateSites": true, "Names": true, "Constants": true,
}

// regFields writes fn's fields as -reg has them.
func (d *writer) regFields(path string, fn *bytecode.Function) {
	fmt.Fprintf(d.w, "function %s %s\n", path, fn.Name)
	v := reflect.ValueOf(fn).Elem()
	for i := range v.NumField() {
		f := functionType.Field(i)
		switch {
		case stackFields[f.Name]:
			continue
		case f.Name == "Reg":
			d.regSymbolic(fn)
			continue
		case f.Name == "LiteralKeys":
			// The keys alone: their sites are the stack form's numbers.
			for _, lk := range fn.LiteralKeys {
				fmt.Fprintf(d.w, "literal keys %q\n", lk.Keys)
			}
			continue
		case f.Name == "Debug":
			d.regDebug(v.Field(i))
			continue
		}
		fmt.Fprint(d.w, f.Name, ": ")
		d.value(v.Field(i))
		fmt.Fprintln(d.w)
	}
}

// regDebug writes a function's Debug but for its statements' pcs, which
// are the stack form's or the register form's by the side.
func (d *writer) regDebug(v reflect.Value) {
	fmt.Fprint(d.w, "Debug: ")
	if v.IsNil() {
		fmt.Fprintln(d.w, "nil")
		return
	}
	e := v.Elem()
	t := e.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		fmt.Fprint(d.w, f.Name, ":")
		if f.Name != "Statements" {
			d.value(e.Field(i))
			continue
		}
		s := e.Field(i)
		fmt.Fprintf(d.w, "[%d ", s.Len())
		for j := range s.Len() {
			st := s.Index(j)
			fmt.Fprint(d.w, "{")
			for k := range st.NumField() {
				if name := st.Type().Field(k).Name; name != "PC" {
					fmt.Fprint(d.w, name, ":")
					d.value(st.Field(k))
				}
			}
			fmt.Fprint(d.w, "} ")
		}
		fmt.Fprint(d.w, "] ")
	}
	fmt.Fprintln(d.w)
}

// regTables are the names and constants an instruction's operands index:
// the code's own where it was compiled at the function's first call
// (RegCode.Names), the function's otherwise.
type regTables struct {
	names  []string
	consts []bytecode.Constant
	fns    map[*bytecode.Function]int
	sites  map[uint32]int
	rc     *bytecode.RegCode
}

func (t *regTables) name(i uint32) string {
	if int(i) < len(t.names) {
		return fmt.Sprintf("%q", t.names[i])
	}
	return fmt.Sprintf("name?%d", i)
}

func (t *regTables) konst(k uint32) string {
	if int(k) >= len(t.consts) {
		return fmt.Sprintf("k?%d", k)
	}
	c := t.consts[k]
	if c.Fn != nil {
		return fmt.Sprintf("fn#%d", t.fns[c.Fn])
	}
	return fmt.Sprintf("k{%d %x %q %q}", c.Kind, c.Num, c.Str, c.Flags)
}

// site is a cache site as the order of its first use, and its name.
func (t *regTables) site(s uint32) string {
	if s == 0 {
		return "@-"
	}
	n, ok := t.sites[s]
	if !ok {
		n = len(t.sites) + 1
		t.sites[s] = n
	}
	name := ""
	if int(s) < len(t.rc.SiteName) {
		name = t.name(t.rc.SiteName[s])
	}
	return fmt.Sprintf("@%d%s", n, name)
}

// regPos is the source position of the instruction at pc: through the
// stack form's instruction it was made from, where the code has SrcPC, or
// the code's own table.
func regPos(fn *bytecode.Function, rc *bytecode.RegCode, pc int) string {
	rv := reflect.ValueOf(rc)
	if m := rv.MethodByName("PosAt"); m.IsValid() {
		out := m.Call([]reflect.Value{reflect.ValueOf(pc)})
		return fmt.Sprint(out[0].Interface(), out[1].Interface())
	}
	src := rv.Elem().FieldByName("SrcPC")
	m := reflect.ValueOf(fn).MethodByName("PosAt")
	if !src.IsValid() || !m.IsValid() || pc >= src.Len() {
		return "?"
	}
	out := m.Call([]reflect.Value{reflect.ValueOf(uint32(src.Index(pc).Uint()))})
	return fmt.Sprint(out[0].Interface(), out[1].Interface())
}

// regSymbolic writes fn's register code with its operands as what they
// name.
func (d *writer) regSymbolic(fn *bytecode.Function) {
	why, _ := refusals.LoadAndDelete(fn)
	var panicked string
	if d.res != nil {
		panicked = d.res.regPanics[fn]
	}
	rc := fn.RegCompiled()
	switch {
	case panicked != "":
		fmt.Fprintf(d.w, "Reg: panic %q %v\n", panicked, why)
		return
	case fn.Reg == nil:
		fmt.Fprintf(d.w, "Reg: nil %v\n", why)
		return
	case rc == nil:
		fmt.Fprintf(d.w, "Reg: none %v\n", why)
		return
	}
	fmt.Fprintf(d.w, "Reg: %v\n", why)
	t := &regTables{names: fn.Names, consts: fn.Constants, fns: map[*bytecode.Function]int{}, sites: map[uint32]int{}, rc: rc}
	if rv := reflect.ValueOf(rc).Elem(); rv.FieldByName("Names").IsValid() && !rv.FieldByName("Names").IsNil() {
		t.names = rv.FieldByName("Names").Interface().([]string)
		t.consts = rv.FieldByName("Constants").Interface().([]bytecode.Constant)
	}
	n := 0
	for _, c := range fn.Constants {
		if c.Fn != nil {
			t.fns[c.Fn] = n
			n++
		}
	}
	for pc, w := range rc.Code {
		fmt.Fprintf(d.w, "r%5d %s ; pos %s\n", pc, regInstr(t, fn, rc, pc, w), regPos(fn, rc, pc))
	}
	v := reflect.ValueOf(rc).Elem()
	for i := range v.NumField() {
		f := regCodeType.Field(i)
		switch f.Name {
		case "Code", "SrcPC", "SiteName", "Names", "Constants", "PropSites", "Aux", "FarNames", "Lines":
			continue
		case "Caps":
			for k, caps := range rc.Caps {
				if caps != nil {
					fmt.Fprintf(d.w, "reg caps %s %v\n", t.konst(uint32(k)), caps)
				}
			}
			continue
		case "Literals":
			for i, lit := range rc.Literals {
				fmt.Fprintf(d.w, "reg literal %d:", i)
				for _, e := range lit {
					if e.Kind == bytecode.LitConst {
						fmt.Fprint(d.w, " ", t.konst(e.K))
					} else {
						fmt.Fprint(d.w, " lit", e.Kind)
					}
				}
				fmt.Fprintln(d.w)
			}
			continue
		case "ObjKeys":
			for i, keys := range rc.ObjKeys {
				fmt.Fprintf(d.w, "reg objkeys %d:", i)
				for _, k := range keys {
					fmt.Fprint(d.w, " ", t.name(k))
				}
				fmt.Fprintln(d.w)
			}
			continue
		case "Switches":
			for i, cases := range rc.Switches {
				fmt.Fprintf(d.w, "reg switch %d:", i)
				for _, c := range cases {
					k := ""
					if c.Kind == bytecode.SwitchStr {
						k = t.konst(c.K)
					}
					fmt.Fprintf(d.w, " {%d %x %q %s -> %d}", c.Kind, c.Num, c.Str, k, c.To)
				}
				fmt.Fprintln(d.w)
			}
			continue
		}
		fmt.Fprint(d.w, "reg ", f.Name, ": ")
		d.value(v.Field(i))
		fmt.Fprintln(d.w)
	}
}

// regInstr writes the instruction w at pc with the operands that index
// the function's tables as what they name, which regcomp.Verify says of
// each opcode; the rest as rc.Format has them.
func regInstr(t *regTables, fn *bytecode.Function, rc *bytecode.RegCode, pc int, w bytecode.RInstr) string {
	a, b, c := uint32(w.A()), uint32(w.B()), uint32(w.C())
	aux, hasAux := uint32(0), pc < len(rc.Aux)
	if hasAux {
		aux = rc.Aux[pc]
	}
	mark := uint8(0)
	if pc < len(rc.Marks) {
		mark = rc.Marks[pc]
	}
	name := func(op uint32) string {
		if op == bytecode.RNameFar && pc < len(rc.FarNames) {
			return t.name(rc.FarNames[pc])
		}
		return t.name(op)
	}
	op := fmt.Sprintf("%s a%d b%d c%d x%d", w.Op(), a, b, c, w.X())
	var extra []string
	// Aux, where it is not an operand written below, as it is.
	auxRaw := hasAux
	switch w.Op() {
	case bytecode.RGetGlobal, bytecode.RSetGlobal:
		extra = append(extra, name(b), t.site(c))
		op = fmt.Sprintf("%s a%d x%d", w.Op(), a, w.X())
	case bytecode.RCheckGlobalRef:
		extra = append(extra, name(b), t.site(c))
		op = fmt.Sprintf("%s a%d", w.Op(), a)
	case bytecode.RLoadConst, bytecode.RClosureW, bytecode.RNewRegExp:
		op = fmt.Sprintf("%s a%d", w.Op(), a)
		extra = append(extra, t.konst(w.BC()))
	case bytecode.RTemplateObject:
		// The function's Templates, which the walk numbers.
	case bytecode.RGetProp, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RSetProp,
		bytecode.RNewObject, bytecode.RAddProp, bytecode.RAddPropI, bytecode.RNewArrayK:
		op = fmt.Sprintf("%s a%d b%d x%d", w.Op(), a, b, w.X())
		extra = append(extra, t.site(c))
	case bytecode.RNewObjectLit:
		op = fmt.Sprintf("%s a%d b%d x%d", w.Op(), a, b, w.X())
		extra = append(extra, t.site(c))
	case bytecode.RCharCodeAt:
		op = fmt.Sprintf("%s a%d b%d x%d", w.Op(), a, b, w.X())
		extra = append(extra, t.site(c))
		// Aux: the stack form's instruction.
		auxRaw = false
	case bytecode.RDefineField, bytecode.RWithRead, bytecode.RWithWrite, bytecode.RDefineGetter,
		bytecode.RDefineSetter, bytecode.RDefineMethod, bytecode.RClassGetter, bytecode.RClassSetter:
		op = fmt.Sprintf("%s a%d b%d x%d", w.Op(), a, b, w.X())
		extra = append(extra, name(c))
	case bytecode.RCheckGlobalLex, bytecode.RCheckGlobalVar, bytecode.RDefineGlobalVar,
		bytecode.RDeclareGlobalLex, bytecode.RDefineGlobalFunc, bytecode.RInitGlobalLex,
		bytecode.RDeclareModuleLex, bytecode.RInitModuleLex, bytecode.RGetGlobalOpt, bytecode.RDeleteVar,
		bytecode.RWithGet, bytecode.RWithDelete, bytecode.RWithSet, bytecode.RWithResolve, bytecode.RWithGetThis,
		bytecode.RAssignConst, bytecode.RThrowRef, bytecode.RAssertResolved, bytecode.RPrivateName,
		bytecode.RSuperGet, bytecode.RSuperSet:
		op = fmt.Sprintf("%s a%d c%d x%d", w.Op(), a, c, w.X())
		extra = append(extra, name(b))
	case bytecode.RGetPrivate, bytecode.RSetPrivate:
		extra = append(extra, t.site(aux>>16), t.name(aux&0xffff))
		auxRaw = false
	case bytecode.RInstanceOf:
		extra = append(extra, t.site(aux))
		auxRaw = false
	case bytecode.RLit:
		op = fmt.Sprintf("%s a%d b%d x%d", w.Op(), a, b, w.X())
		extra = append(extra, t.konst(c))
		// Aux of a call: the stack form's instruction.
		auxRaw = false
	case bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32:
		// Aux: the stack form's instruction of the | 0.
		auxRaw = false
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
		switch {
		case mark&bytecode.RMarkReuse != 0:
			// Aux: the stack form's call.
			auxRaw = false
		case mark&(bytecode.RMarkLitRecv|bytecode.RMarkLitArg) != 0:
			extra = append(extra, t.konst(aux))
			auxRaw = false
		}
	case bytecode.RDebug:
		// Aux: the statement's index, which is the same on both sides.
	}
	if auxRaw {
		extra = append(extra, fmt.Sprintf("aux%d", aux))
	}
	if mark != 0 {
		extra = append(extra, fmt.Sprintf("mark%d", mark))
	}
	if pc < len(rc.FarNames) && rc.FarNames[pc] != 0 && w.Op() == bytecode.RCheckRef {
		extra = append(extra, fmt.Sprintf("far%d", rc.FarNames[pc]))
	}
	if len(extra) == 0 {
		return op
	}
	return op + " " + strings.Join(extra, " ")
}
