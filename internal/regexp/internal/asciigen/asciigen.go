// Package asciigen makes the regexp matchers over the bytes of a Go string from
// the matcher over UTF-16 code units, so that they are the same code and the
// one the engine uses is left exactly as written.
//
// Two are made. The ASCII matcher reads an ASCII string's bytes, each of which
// is a code unit, and is the code-unit matcher with its units made bytes. The
// UTF-8 matcher reads a string's code points from its UTF-8 bytes, and differs
// in a few places the text of exec.go is patched at: where a unit is taken to
// be a whole character, and a backreference, whose case variants may differ in
// length. Its input's reading methods are written by hand.
package asciigen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Variant says which matcher to make.
type Variant struct {
	// Suffix is added to the names of the input and matcher types and of
	// indexUnits: ASCII or UTF8.
	Suffix string
	// OwnReaders leaves out the input's methods, which the variant has
	// written by hand.
	OwnReaders bool
	// Patches are replacements made in exec.go's text before it is read,
	// each of which must match exactly once.
	Patches []Patch
}

// Patch is one replacement in exec.go's text.
type Patch struct{ Old, New string }

// ASCII is the matcher over an ASCII string's bytes.
var ASCII = Variant{Suffix: "ASCII"}

// UTF8 is the matcher over a string's UTF-8 bytes, read by code point.
var UTF8 = Variant{
	Suffix:     "UTF8",
	OwnReaders: true,
	Patches: []Patch{
		{
			// The bytes no match can begin with are passed over a byte at a
			// time too. That never stops inside a character: from where one
			// begins, only bytes of ASCII are passed over when a byte past
			// ASCII can begin a match, and every byte past ASCII is when
			// none can, so the loop stops where a character begins.
			Old: "if first != nil && !in.unicode && !sticky {",
			New: "if first != nil && !sticky {",
		},
		{
			// Only a byte below 0x80 is a character by itself.
			Old: "\tunits, unicode := m.in.units, m.in.unicode\n",
			New: "\tunits := m.in.units\n",
		},
		{
			Old: "if u := units[pos]; !unicode || !utf16.IsSurrogate(rune(u)) {",
			New: "if u := units[pos]; u < 0x80 {",
		},
		{
			// Case variants may differ in length, so a backreference is
			// compared a character at a time; see backref.
			Old: `			n := end - start
			// Leftwards the reference matches the text ending at the cursor,
			// so the comparison starts n units before it.
			at := pos
			if in.rev {
				at = pos - n
				if at < 0 {
					goto backtrack
				}
			} else if pos+n > m.in.length() {
				goto backtrack
			}
			if !m.compareRange(start, at, n, in.op == opBackrefFold) {
				goto backtrack
			}
			pos = at
			if !in.rev {
				pos = pos + n
			}
`,
			New: `			if p, ok := m.backref(start, end, pos, in.rev, in.op == opBackrefFold); ok {
				pos = p
			} else {
				goto backtrack
			}
`,
		},
	},
}

// Generate makes the text of a variant's file from the matcher in path,
// exec.go: its input and matcher types and their methods, renamed, with the
// input's code units made bytes.
func Generate(path string, v Variant) ([]byte, error) {
	srcBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src := strings.ReplaceAll(string(srcBytes), "\r\n", "\n")
	for _, p := range v.Patches {
		if n := strings.Count(src, p.Old); n != 1 {
			return nil, fmt.Errorf("%s: a patch for the %s matcher matches %d times, not once: %.60q", path, v.Suffix, n, p.Old)
		}
		src = strings.Replace(src, p.Old, p.New, 1)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	rename := map[string]string{
		"input":      "input" + v.Suffix,
		"matcher":    "matcher" + v.Suffix,
		"indexUnits": "indexUnits" + v.Suffix,
	}
	recvIs := func(fd *ast.FuncDecl, names ...string) bool {
		if fd.Recv == nil || len(fd.Recv.List) != 1 {
			return false
		}
		star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		id, ok := star.X.(*ast.Ident)
		if !ok {
			return false
		}
		for _, n := range names {
			if id.Name == n {
				return true
			}
		}
		return false
	}
	var decls []ast.Decl
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts := spec.(*ast.TypeSpec)
				if ts.Name.Name == "input" || ts.Name.Name == "matcher" {
					decls = append(decls, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{ts}})
				}
			}
		case *ast.FuncDecl:
			if recvIs(d, "matcher") || recvIs(d, "input") && !v.OwnReaders {
				d.Doc = nil
				decls = append(decls, d)
			}
		}
	}
	// A field, a selected name and a literal's key keep their names: only the
	// types and the function are renamed, not the matcher's field called input.
	kept := map[*ast.Ident]bool{}
	for _, d := range decls {
		ast.Inspect(d, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Field:
				for _, id := range n.Names {
					kept[id] = true
				}
			case *ast.SelectorExpr:
				kept[n.Sel] = true
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok {
					kept[id] = true
				}
			}
			return true
		})
	}
	imports := map[string]bool{}
	for _, d := range decls {
		ast.Inspect(d, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if to, ok := rename[n.Name]; ok && !kept[n] {
					n.Name = to
				}
			case *ast.TypeSpec:
				// The input's code units are bytes.
				if n.Name.Name == "input" || n.Name.Name == rename["input"] {
					for _, f := range n.Type.(*ast.StructType).Fields.List {
						if len(f.Names) == 1 && f.Names[0].Name == "units" {
							f.Type = &ast.ArrayType{Elt: ast.NewIdent("byte")}
						}
					}
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok {
					switch pkg.Name {
					case "utf16", "utf8", "errors", "sync":
						imports[pkg.Name] = true
					}
				}
			}
			return true
		})
	}
	paths := map[string]string{"utf16": "unicode/utf16", "utf8": "unicode/utf8", "errors": "errors", "sync": "sync"}
	var names []string
	for name := range imports {
		names = append(names, paths[name])
	}
	sort.Strings(names)

	var b bytes.Buffer
	b.WriteString("// Code generated by internal/asciigen from exec.go; DO NOT EDIT.\n\n")
	b.WriteString("package regexp\n\n")
	if len(names) > 0 {
		b.WriteString("import (\n")
		for _, p := range names {
			b.WriteString("\t" + strconv.Quote(p) + "\n")
		}
		b.WriteString(")\n")
	}
	for _, d := range decls {
		b.WriteString("\n")
		if err := format.Node(&b, token.NewFileSet(), d); err != nil {
			return nil, err
		}
		b.WriteString("\n")
	}
	return format.Source(b.Bytes())
}
