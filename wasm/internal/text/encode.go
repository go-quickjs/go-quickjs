package text

import (
	"sort"
	"unicode/utf8"
)

// The section kinds custom sections are placed around, in binary order.
const (
	secCustom = iota
	secType
	secImport
	secFunc
	secTable
	secMemory
	secTag
	secGlobal
	secExport
	secStart
	secElem
	secDataCount
	secCode
	secData
)

var sectionKinds = map[string]int{
	"type": secType, "import": secImport, "func": secFunc, "table": secTable,
	"memory": secMemory, "tag": secTag, "global": secGlobal, "export": secExport,
	"start": secStart, "elem": secElem, "datacount": secDataCount, "code": secCode,
	"data": secData, "first": secType, "last": secData,
}

// custom is a custom section from an @custom annotation. place orders it:
// twice the section kind it is placed around, plus one if after it.
type custom struct {
	name    string
	content []byte
	place   int
	order   int
}

// annotations takes the module's @custom and @name annotations.
func (c *compiler) annotations(fields []field, from, hi int, inline bool) {
	inside := func(a Annot) *field {
		for i := range fields {
			if a.At > fields[i].start && a.At <= fields[i].end {
				return &fields[i]
			}
		}
		return nil
	}
	c.nameAnnots = map[uint32]string{}
	c.tagAnnots = map[uint32]string{}
	for i, a := range c.annots {
		at := Token{Line: a.Line, Col: a.Col}
		switch a.Name {
		case "custom":
			if inside(a) != nil {
				c.fail(at, "misplaced @custom annotation")
			}
			c.customs = append(c.customs, c.customAnnot(a, i))
		case "name":
			items := a.Items
			if len(items) == 0 || items[0].Kind != String {
				c.fail(at, "@name annotation: string expected")
			}
			if !utf8.ValidString(items[0].Text) {
				c.fail(items[0], "malformed UTF-8 encoding")
			}
			if len(items) > 1 {
				c.fail(items[1], "@name annotation: unexpected token")
			}
			name := items[0].Text
			f := inside(a)
			switch {
			case f == nil && (len(fields) == 0 || a.At <= fields[0].start):
				if c.moduleName != nil && c.moduleNameAnnot {
					c.fail(at, "@name annotation: multiple module names")
				}
				c.moduleName = &name
				c.moduleNameAnnot = true
			case f != nil && f.kind == "func" && a.At == f.nameSlot:
				if _, dup := c.nameAnnots[f.idx]; dup {
					c.fail(at, "@name annotation: multiple function names")
				}
				c.nameAnnots[f.idx] = name
			case f != nil && f.kind == "tag" && a.At == f.nameSlot:
				c.tagAnnots[f.idx] = name
			default:
				c.fail(at, "misplaced @name annotation")
			}
		}
	}
}

func (c *compiler) customAnnot(a Annot, order int) custom {
	at := Token{Line: a.Line, Col: a.Col}
	items := a.Items
	if len(items) == 0 || items[0].Kind != String {
		c.fail(at, "@custom annotation: missing section name")
	}
	if !utf8.ValidString(items[0].Text) {
		c.fail(items[0], "@custom annotation: malformed UTF-8 encoding")
	}
	cs := custom{name: items[0].Text, place: 2*secData + 1, order: order}
	items = items[1:]
	if len(items) > 0 && items[0].Kind == LParen {
		// (before|after kind)
		end := 1
		for depth := 1; end < len(items); end++ {
			if items[end].Kind == LParen {
				depth++
			} else if items[end].Kind == RParen {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		in := items[1:end]
		if len(in) == 0 || in[0].Kind != Keyword && in[0].Kind != Reserved || in[0].Text != "before" && in[0].Text != "after" {
			c.fail(at, "@custom annotation: malformed placement")
		}
		after := in[0].Text == "after"
		if len(in) < 2 {
			c.fail(at, "@custom annotation: malformed section kind")
		}
		kind, ok := sectionKinds[in[1].Text]
		if !ok || in[1].Kind != Keyword && in[1].Kind != Reserved {
			c.fail(in[1], "@custom annotation: malformed section kind")
		}
		if len(in) > 2 {
			c.fail(in[2], "@custom annotation: unexpected token")
		}
		cs.place = 2 * kind
		if after {
			cs.place++
		}
		if end < len(items) {
			items = items[end+1:]
		} else {
			items = nil
		}
	}
	for _, it := range items {
		if it.Kind != String {
			c.fail(it, "@custom annotation: unexpected token")
		}
		cs.content = append(cs.content, it.Text...)
	}
	return cs
}

func appendSection(out []byte, id byte, content []byte) []byte {
	out = append(out, id)
	out = appendU32(out, uint32(len(content)))
	return append(out, content...)
}

func vec(items [][]byte) []byte {
	b := appendU32(nil, uint32(len(items)))
	for _, it := range items {
		b = append(b, it...)
	}
	return b
}

// encodeModule assembles the binary module.
func (c *compiler) encodeModule() []byte {
	sort.SliceStable(c.customs, func(i, j int) bool {
		if c.customs[i].place != c.customs[j].place {
			return c.customs[i].place < c.customs[j].place
		}
		return c.customs[i].order < c.customs[j].order
	})
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	ci := 0
	customsUpTo := func(place int) {
		for ci < len(c.customs) && c.customs[ci].place <= place {
			cs := c.customs[ci]
			out = appendSection(out, 0, append(appendName(nil, cs.name), cs.content...))
			ci++
		}
	}
	section := func(kind int, id byte, content []byte, present bool) {
		customsUpTo(2 * kind)
		if present {
			out = appendSection(out, id, content)
		}
	}
	var types [][]byte
	for _, g := range c.recGroups {
		if len(g) == 1 {
			types = append(types, g[0].encode(nil))
			continue
		}
		b := appendU32([]byte{0x4e}, uint32(len(g)))
		for _, st := range g {
			b = st.encode(b)
		}
		types = append(types, b)
	}
	section(secType, 1, vec(types), len(types) > 0)
	section(secImport, 2, vec(c.imports), len(c.imports) > 0)
	var funcs [][]byte
	for _, x := range c.funcTypes {
		funcs = append(funcs, appendU32(nil, x))
	}
	section(secFunc, 3, vec(funcs), len(funcs) > 0)
	section(secTable, 4, vec(c.tableSecs), len(c.tableSecs) > 0)
	section(secMemory, 5, vec(c.memSecs), len(c.memSecs) > 0)
	section(secTag, 13, vec(c.tagSecs), len(c.tagSecs) > 0)
	section(secGlobal, 6, vec(c.globalSecs), len(c.globalSecs) > 0)
	section(secExport, 7, vec(c.exports), len(c.exports) > 0)
	if c.start != nil {
		section(secStart, 8, appendU32(nil, *c.start), true)
	} else {
		section(secStart, 8, nil, false)
	}
	section(secElem, 9, vec(c.elemSecs), len(c.elemSecs) > 0)
	section(secDataCount, 12, appendU32(nil, uint32(len(c.dataSecs))), c.dataCount)
	var code [][]byte
	for _, body := range c.code {
		code = append(code, append(appendU32(nil, uint32(len(body))), body...))
	}
	section(secCode, 10, vec(code), len(code) > 0)
	section(secData, 11, vec(c.dataSecs), len(c.dataSecs) > 0)
	customsUpTo(2*secData + 1)
	if names := c.nameSection(); names != nil {
		out = appendSection(out, 0, append(appendName(nil, "name"), names...))
	}
	return out
}

func nameMap(m map[uint32]string) []byte {
	if len(m) == 0 {
		return nil
	}
	idx := make([]uint32, 0, len(m))
	for x := range m {
		idx = append(idx, x)
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
	b := appendU32(nil, uint32(len(idx)))
	for _, x := range idx {
		b = appendU32(b, x)
		b = appendName(b, m[x])
	}
	return b
}

func indirectNameMap(m map[uint32]map[uint32]string) []byte {
	if len(m) == 0 {
		return nil
	}
	idx := make([]uint32, 0, len(m))
	for x := range m {
		idx = append(idx, x)
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
	b := appendU32(nil, uint32(len(idx)))
	for _, x := range idx {
		b = appendU32(b, x)
		b = append(b, nameMap(m[x])...)
	}
	return b
}

// nameSection encodes the name section's contents from the module's
// identifiers and @name annotations, or returns nil if there are none.
func (c *compiler) nameSection() []byte {
	var b []byte
	sub := func(id byte, content []byte) {
		if content != nil {
			b = appendSection(b, id, content)
		}
	}
	if c.moduleName != nil {
		sub(0, appendName(nil, *c.moduleName))
	}
	funcs := map[uint32]string{}
	for x, n := range c.funcs.byIndex {
		funcs[x] = n
	}
	for x, n := range c.nameAnnots {
		funcs[x] = n
	}
	sub(1, nameMap(funcs))
	locals := map[uint32]map[uint32]string{}
	for _, l := range c.funcLocals {
		locals[l.fn] = l.names
	}
	sub(2, indirectNameMap(locals))
	sub(4, nameMap(c.types.byIndex))
	sub(5, nameMap(c.tables.byIndex))
	sub(6, nameMap(c.mems.byIndex))
	sub(7, nameMap(c.globals.byIndex))
	sub(8, nameMap(c.elems.byIndex))
	sub(9, nameMap(c.datas.byIndex))
	fields := map[uint32]map[uint32]string{}
	for x, m := range c.fieldNames {
		if len(m) == 0 {
			continue
		}
		byIdx := map[uint32]string{}
		for n, i := range m {
			byIdx[i] = n
		}
		fields[uint32(x)] = byIdx
	}
	sub(10, indirectNameMap(fields))
	tags := map[uint32]string{}
	for x, n := range c.tags.byIndex {
		tags[x] = n
	}
	for x, n := range c.tagAnnots {
		tags[x] = n
	}
	sub(11, nameMap(tags))
	return b
}
