package regcomp

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/ast"
)

// ident is an identifier named name of number id.
func ident(name string, id uint32) *ast.Ident {
	n := &ast.Ident{Name: name}
	n.SetID(id)
	return n
}

// TestRecordSlots pins what the record keeps of each identifier wherever
// it is kept: in its slot, in more where its slot is another's or out of
// range or it has no number, a read's literal apart from the write of the
// same name, a resolution dropped and set again, and Bail on a name noted
// two ways.
func TestRecordSlots(t *testing.T) {
	r := NewRecord(3)
	lit := &ast.NumberLit{Value: 1}
	a := ident("a", 1)
	b := ident("b", 1) // a's number: an arrow's parameter
	c := ident("c", 9) // past the record's numbers
	d := ident("d", 0) // made by the compiler
	e := ident("e", 3) // in its slot
	r.NoteRead(a, Res{Kind: ResLiteral, Lit: lit})
	r.NoteWrite(a, Res{Kind: ResConstAssign})
	r.NoteRead(b, Res{Kind: ResLocal, Index: 4})
	r.NoteWrite(c, Res{Kind: ResUpval, Index: 2})
	r.NoteRead(d, Res{Kind: ResGlobal})
	r.NoteRead(e, Res{Kind: ResLocalCheck, Index: 7})
	for _, tc := range []struct {
		id          *ast.Ident
		read, write Res
		hasR, hasW  bool
	}{
		{a, Res{Kind: ResLiteral, Lit: lit}, Res{Kind: ResConstAssign}, true, true},
		{b, Res{Kind: ResLocal, Index: 4}, Res{}, true, false},
		{c, Res{}, Res{Kind: ResUpval, Index: 2}, false, true},
		{d, Res{Kind: ResGlobal}, Res{}, true, false},
		{e, Res{Kind: ResLocalCheck, Index: 7}, Res{}, true, false},
		{ident("f", 2), Res{}, Res{}, false, false},
	} {
		if res, ok := r.ReadOf(tc.id); res != tc.read || ok != tc.hasR {
			t.Errorf("ReadOf(%s) = %v, %v; want %v, %v", tc.id.Name, res, ok, tc.read, tc.hasR)
		}
		if res, ok := r.WriteOf(tc.id); res != tc.write || ok != tc.hasW {
			t.Errorf("WriteOf(%s) = %v, %v; want %v, %v", tc.id.Name, res, ok, tc.write, tc.hasW)
		}
	}
	n := 0
	for range r.Reads() {
		n++
	}
	if n != 4 {
		t.Errorf("Reads has %d, want 4", n)
	}
	r.setRead(a, Res{}, false)
	if _, ok := r.ReadOf(a); ok {
		t.Error("a dropped read is still there")
	}
	r.setRead(a, Res{Kind: ResLiteral, Lit: lit}, true)
	if res, _ := r.ReadOf(a); res.Lit != lit {
		t.Errorf("a read set again is %v", res)
	}
	if r.Bail {
		t.Fatal("Bail with no name noted two ways")
	}
	r.NoteRead(b, Res{Kind: ResLocal, Index: 4})
	if r.Bail {
		t.Fatal("Bail on a name noted the same way twice")
	}
	r.NoteRead(b, Res{Kind: ResLocal, Index: 5})
	if !r.Bail {
		t.Error("no Bail on a name noted two ways")
	}
}
