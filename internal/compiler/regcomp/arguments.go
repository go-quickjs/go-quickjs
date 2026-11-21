package regcomp

import "github.com/go-quickjs/go-quickjs/internal/ast"

type argumentsNotes struct {
	aliases [][2]*ast.Ident
	reads   []*ast.Ident
}

// argumentsNotes is the walk's notes of r's possible arguments aliases,
// or nil.
func (r *Record) argumentsNotes() *argumentsNotes {
	if r.rare == nil {
		return nil
	}
	return r.rare.arguments
}

// argumentsAliases keeps safe aliases on the arguments binding's one
// register, so that a slow index read materializes the object for them all.
// It belongs to this compile; the deferred compiler's AST and Record stay
// unchanged.
type argumentsAliases struct {
	slot  uint32
	inits map[*ast.Ident]bool
	reads map[*ast.Ident]bool
}

func (g *gen) argumentsAliases() *argumentsAliases {
	fn := g.fn
	notes := g.rec.argumentsNotes()
	if notes == nil || !fn.UsesArguments || fn.ForwardsArguments || !fn.HasSimpleParams || fn.HasRest ||
		fn.Async || fn.Generator || fn.TopLevel || fn.Debug != nil || len(g.rec.Evals) != 0 || len(g.rec.Withs) != 0 {
		return nil
	}
	root := -1
	for i, l := range fn.Locals {
		if l.Name == "arguments" {
			if root >= 0 || l.Captured {
				return nil
			}
			root = i
		}
	}
	if root < fn.ParamCount {
		return nil
	}
	a := &argumentsAliases{slot: uint32(root), inits: map[*ast.Ident]bool{}, reads: map[*ast.Ident]bool{}}
	aliases := map[uint32]*ast.Ident{}
	// A declaration directly in the body runs before every later read of
	// its alias. Declarations in branches or loops need a dominance proof.
	for _, pair := range notes.aliases {
		id, src := pair[0], pair[1]
		r := g.rec.readRes(src)
		w := g.rec.writeRes(id)
		if (r.Kind != ResLocal && r.Kind != ResLazyArgs) || int(r.Index) != root ||
			(w.Kind != ResLocal && w.Kind != ResLocalInit) || int(w.Index) < fn.ParamCount ||
			int(w.Index) >= len(fn.Locals) || w.Index == a.slot || fn.Locals[w.Index].Captured {
			continue
		}
		if aliases[w.Index] != nil {
			return nil
		}
		aliases[w.Index] = id
		a.inits[id], a.reads[src] = true, true
	}
	if len(aliases) == 0 {
		return nil
	}
	for _, slot := range g.rec.Funcs {
		if slot == a.slot || aliases[slot] != nil {
			return nil
		}
	}
	for _, id := range notes.reads {
		r := g.rec.readRes(id)
		if (r.Kind == ResLocal || r.Kind == ResLocalCheck || r.Kind == ResLazyArgs) && (r.Index == a.slot || aliases[r.Index] != nil) {
			a.reads[id] = true
		}
	}
	// Every read must be one the walk proved a member read, or an alias's
	// initializer. Stores, escapes and captures retain the ordinary object.
	ok := true
	g.rec.each(func(s *identSlot) bool {
		if s.read.set && (s.read.kind == ResLocal || s.read.kind == ResLocalCheck || s.read.kind == ResLazyArgs) {
			i := s.read.index
			if i == a.slot || aliases[i] != nil {
				if !a.reads[s.id] || aliases[i] != nil && s.id.Start <= aliases[i].Start {
					ok = false
					return false
				}
			}
		}
		if s.write.set && (s.write.kind == ResLocal || s.write.kind == ResLocalCheck || s.write.kind == ResLocalInit) {
			i := s.write.index
			if i == a.slot || aliases[i] != nil && !a.inits[s.id] {
				ok = false
				return false
			}
		}
		return true
	})
	if !ok {
		return nil
	}
	return a
}

// NoteArgumentsAliases records possible aliases for the existing walk to
// collect reads of. Only declarations directly in the body are candidates:
// their initialization precedes every later read outside a nested function.
func (r *Record) NoteArgumentsAliases(body []ast.Stmt) {
	if r == nil {
		return
	}
	for _, s := range body {
		d, ok := s.(*ast.VarDecl)
		if !ok || d.Kind.IsUsing() {
			continue
		}
		for _, d := range d.Decls {
			id, ok := d.Target.(*ast.Ident)
			if !ok {
				continue
			}
			src, ok := d.Init.(*ast.Ident)
			if ok && src.Name == "arguments" {
				x := r.rareOf()
				if x.arguments == nil {
					x.arguments = &argumentsNotes{}
				}
				x.arguments.aliases = append(x.arguments.aliases, [2]*ast.Ident{id, src})
			}
		}
	}
}

// NoteArgumentsRead records a read of a candidate alias's length or element.
// Calls with a member receiver, stores, updates and deletes walk their base
// directly, so they leave an unapproved binding read in the record.
func (r *Record) NoteArgumentsRead(m *ast.Member) {
	if r == nil || r.rare == nil || r.rare.arguments == nil || m.Optional || !m.Computed && memberName(m) != "length" {
		return
	}
	notes := r.rare.arguments
	id, ok := m.Object.(*ast.Ident)
	if !ok {
		return
	}
	if id.Name != "arguments" {
		found := false
		for _, p := range notes.aliases {
			found = found || p[0].Name == id.Name
		}
		if !found {
			return
		}
	}
	notes.reads = append(notes.reads, id)
}
