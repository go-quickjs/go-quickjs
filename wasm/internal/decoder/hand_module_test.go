package decoder

import "strings"

// sectionsSeed is a valid module with every section: imports of each kind,
// recursion groups and subtypes, tables with and without initializers,
// memories of each kind, tags, globals, exports of each kind, a start
// function, element segments of each form, data segments of each form, and
// name and other custom sections.
func sectionsSeed() []byte {
	types := [][]byte{
		voidFn,                       // 0
		functype(bs(tI32), bs(tI32)), // 1
		functype(bs(tI32), nil),      // 2
		bs(0x4e, 2, 0x50, 0, 0x5f, 1, tI32, 1, 0x4f, 1, 3, 0x5f, 2, tI32, 1, tI64, 0), // 3, 4: rec group
		bs(0x5e, tI32, 0), // 5
	}
	imports := [][]byte{
		cat("env", "f", 0, 1),
		cat("env", "t", 1, tFuncRef, 1, 1, 10),
		cat("env", "m", 2, 1, 1, 2),
		cat("env", "g", 3, tI32, 0),
		cat("env", "e", 4, 0, 2),
	}
	return modSpec{
		types:     types,
		imports:   imports,
		funcTypes: []uint32{0, 1},
		tables:    [][]byte{bs(tExternRef, 0, 1), bs(0x40, 0, tRef, tFuncRef, 0, 1, 0xd2, 1, 0x0b), bs(tFuncRef, 4, 1)},
		memories:  [][]byte{bs(3, 1, 4), bs(5, 1, 2)},
		tags:      [][]byte{bs(0, 2)},
		globals: [][]byte{bs(tI32, 1, 0x23, 0, 0x0b), bs(tFuncRef, 0, 0xd2, 1, 0x0b),
			cat(tRef, 5, 0, 0x41, 1, 0xfb, 0x08, 5, 1, 0x0b)},
		exports: [][]byte{cat("f", 0, 1), cat("t", 1, 0), cat("m", 2, 1), cat("g", 3, 1), cat("e", 4, 0), cat("ee", 4, 1)},
		start:   uleb(1),
		elems: [][]byte{
			cat(0, 0x41, 0, 0x0b, vec(uleb(0), uleb(2))),
			cat(1, 0, vec(uleb(1))),
			cat(2, 2, 0x41, 0, 0x0b, 0, vec(uleb(2))),
			cat(3, 0, vec(uleb(1))),
			cat(4, 0x41, 0, 0x0b, vec(bs(0xd2, 1, 0x0b))),
			cat(5, tFuncRef, vec(bs(0xd0, tFuncRef, 0x0b))),
			cat(6, 1, 0x41, 0, 0x0b, tExternRef, vec(bs(0xd0, tExternRef, 0x0b))),
			cat(7, tFuncRef, vec(bs(0xd2, 2, 0x0b))),
		},
		dataCount: uleb(3),
		bodies:    [][]byte{body(noLocals, 0x0b), body(cat(1, 1, tI64), 0x20, 0, 0x0b)},
		datas:     [][]byte{cat(0, 0x41, 0, 0x0b, "ab"), cat(1, "cd"), cat(2, 2, 0x42, 0, 0x0b, "ef")},
		customs: [][]byte{
			cat("name", sec(0, cat("mod")), sec(1, vec(cat(uleb(1), "one"), cat(uleb(2), "two"))),
				sec(2, vec(cat(uleb(2), vec(cat(uleb(1), "loc")))))),
			cat("other", "payload"),
			cat("sourceMappingURL", "x.map"),
		},
	}.bytes()
}

// memory64Pages is a module declaring a memory64 of initial size pages.
func memory64Pages(pages uint64) []byte {
	return wasmModule(sec(5, vec(cat(4, uleb(pages)))))
}

// moduleCases are modules with an error in a section other than code, or a
// structure error, one for each message the module decoder reports.
func moduleCases() []corpusCase {
	var cs []corpusCase
	add := func(name string, b []byte) { cs = append(cs, corpusCase{"hand/mod-" + name, b}) }
	m := func(sections ...[]byte) []byte { return wasmModule(sections...) }
	types := func(ts ...[]byte) []byte { return sec(1, vec(ts...)) }

	add("sections-valid", sectionsSeed())

	// Header and section structure.
	add("magic", bs(0, 'a', 's', 'n', 1, 0, 0, 0))
	add("version", bs(0, 'a', 's', 'm', 2, 0, 0, 0))
	add("header-short", bs(0, 'a', 's'))
	add("section-past-end", m(bs(1, 10, 0)))
	add("section-size-leb", m(bs(1, 0x80)))
	add("section-shorter", m(sec(1, bs(0, 0))))
	add("section-code-unknown", m(sec(0x15, nil)))
	add("section-stringref", m(sec(14, bs(0))))
	add("section-order", m(sec(3, vec()), sec(1, vec())))
	add("section-multiple", m(sec(12, bs(0)), sec(12, bs(0))))
	add("section-datacount-after-code", m(sec(10, vec()), sec(12, bs(0))))
	add("section-tag-after-global", m(sec(6, vec()), sec(13, vec())))
	add("custom-name-utf8", m(sec(0, cat("\xff"))))
	add("custom-name-past-end", m(bs(0, 2, 5, 'a')))
	add("custom-name-leb", m(bs(0, 1, 0x80)))
	add("custom-unknown-ok", m(sec(0, cat("whatever", "data"))))
	add("custom-name-malformed-ok", m(sec(0, cat("name", 1, 0x7f))))
	add("leb-extra-bits", m(sec(1, bs(0x80, 0x80, 0x80, 0x80, 0x10))))
	add("leb-overflow", m(sec(1, bs(0x80, 0x80, 0x80, 0x80, 0x80, 0))))
	add("leb-reached-end", m(sec(1, bs(0x80))))

	// Types.
	add("type-count-limit", m(sec(1, uleb(1_000_001))))
	add("type-form", m(types(bs(0x55))))
	add("type-form-shared", m(types(bs(0x50, 0, 0x65, 0x5f, 0))))
	add("type-cont", m(types(bs(0x5d, 0))))
	add("type-descriptor", m(types(bs(0x50, 0, 0x4d, 0, 0x5f, 0))))
	add("type-supertype-self", m(types(bs(0x50, 1, 0, 0x5f, 0))))
	add("type-supertype-count", m(types(bs(0x50, 2, 0, 0, 0x5f, 0))))
	add("type-extends-final", m(types(bs(0x5f, 0), bs(0x50, 1, 0, 0x5f, 0))))
	add("type-invalid-supertype", m(types(bs(0x50, 0, 0x5f, 1, tI32, 0), bs(0x50, 1, 0, 0x5f, 1, tI64, 0))))
	add("type-kind-mismatch", m(types(bs(0x50, 0, 0x5f, 0), bs(0x50, 1, 0, 0x5e, tI32, 0))))
	chain := [][]byte{bs(0x50, 0, 0x5f, 0)}
	for i := 1; i <= 64; i++ {
		chain = append(chain, cat(0x50, 1, uleb(uint64(i-1)), 0x5f, 0))
	}
	add("type-depth", m(types(chain...)))
	add("type-rec-size", m(sec(1, cat(1, 0x4e, uleb(1_000_001)))))
	add("type-rec-forward", m(types(bs(0x4e, 2, 0x5f, 1, tRefNull, 1, 0, 0x5f, 1, tRefNull, 0, 0))))
	add("type-mutability", m(types(bs(0x5f, 1, tI32, 2))))
	add("type-field-count", m(types(cat(0x5f, uleb(10_001)))))
	add("type-param-count", m(types(cat(0x60, uleb(1_001)))))
	add("type-result-count", m(types(cat(0x60, 0, uleb(1_001)))))
	add("type-value", m(types(functype(bs(0x7a), nil))))
	add("type-value-i8", m(types(functype(bs(0x78), nil))))
	add("type-index-oob", m(types(bs(0x60, 1, tRef, 5, 0))))
	add("type-heap-shared", m(types(bs(0x60, 1, tRefNull, 0x65, 0x70, 0))))
	add("type-stringref", m(types(bs(0x60, 1, 0x67, 0))))
	add("type-heap-string", m(types(bs(0x60, 1, tRefNull, 0x67, 0))))
	add("type-heap-exact", m(types(bs(0x60, 1, tRefNull, 0x62, 0, 0))))
	add("type-heap-unknown", m(types(bs(0x60, 1, tRefNull, 0x40, 0))))
	add("type-heap-big", m(types(cat(0x60, 1, tRefNull, uleb(1_000_000), 0))))
	add("type-heap-negative-big", m(types(bs(0x60, 1, tRefNull, 0xbf, 0x7f, 0))))
	add("type-cont-value", m(types(bs(0x60, 1, 0x68, 0))))
	add("type-exnref", m(types(bs(0x60, 1, tExnRef, 0))))
	add("type-packed-array", m(types(bs(0x5e, 0x77, 1))))

	// Imports.
	imp := func(entries ...[]byte) []byte { return sec(2, vec(entries...)) }
	add("import-count-limit", m(sec(2, uleb(1_000_001))))
	add("import-module-utf8", m(imp(cat("\xff", "f", 3, tI32, 0))))
	add("import-field-utf8", m(imp(cat("m", "\xc0\x80", 3, tI32, 0))))
	add("import-kind", m(imp(cat("m", "f", 5))))
	add("import-kind-exact", m(types(voidFn), imp(cat("m", "f", 0x20, 0))))
	add("import-func-sig", m(types(voidFn), imp(cat("m", "f", 0, 3))))
	add("import-func-struct", m(types(bs(0x5f, 0)), imp(cat("m", "f", 0, 0))))
	add("import-table-type", m(imp(cat("m", "t", 1, tI32, 0, 1))))
	add("import-table-flags", m(imp(cat("m", "t", 1, tFuncRef, 8, 1))))
	add("import-table-shared", m(imp(cat("m", "t", 1, tFuncRef, 2, 1))))
	add("import-table-max", m(imp(cat("m", "t", 1, tFuncRef, 1, 5, 1))))
	add("import-table-initial", m(imp(cat("m", "t", 1, tFuncRef, 0, uleb(10_000_001)))))
	add("import-memory-flags", m(imp(cat("m", "m", 2, 8, 1))))
	add("import-memory-shared", m(imp(cat("m", "m", 2, 2, 1))))
	add("import-memory-initial", m(imp(cat("m", "m", 2, 0, uleb(65_537)))))
	add("import-memory-max", m(imp(cat("m", "m", 2, 1, 0, uleb(65_537)))))
	add("import-memory-max-less", m(imp(cat("m", "m", 2, 1, 2, 1))))
	add("import-memory64-initial", m(imp(cat("m", "m", 2, 4, uleb(300_000)))))
	add("import-global-flags", m(imp(cat("m", "g", 3, tI32, 2))))
	add("import-global-type", m(imp(cat("m", "g", 3, 0x7a, 0))))
	add("import-tag-attribute", m(types(voidFn), imp(cat("m", "e", 4, 1, 0))))
	add("import-tag-result", m(types(functype(nil, bs(tI32))), imp(cat("m", "e", 4, 0, 0))))
	add("import-truncated", m(imp(cat("m"))))

	// Functions, tables, memories.
	add("function-sig", m(types(voidFn), sec(3, vec(uleb(4)))))
	add("function-count-limit", m(types(voidFn), sec(3, uleb(1_000_001))))
	add("function-no-code", m(types(voidFn), sec(3, vec(uleb(0)))))
	tables := func(entries ...[]byte) []byte { return sec(4, vec(entries...)) }
	add("table-type", m(tables(bs(tI32, 0, 1))))
	add("table-non-defaultable", m(tables(bs(tRef, tFuncRef, 0, 1))))
	add("table-reserved", m(tables(bs(0x40, 1, tFuncRef, 0, 1))))
	add("table-init-type", m(tables(bs(0x40, 0, tFuncRef, 0, 1, 0x41, 0, 0x0b))))
	add("table-init-missing-end", m(tables(bs(0x40, 0, tFuncRef, 0, 1, 0xd0, tFuncRef))))
	add("table-initial", m(tables(cat(tFuncRef, 0, uleb(10_000_001)))))
	add("table-max-less", m(tables(bs(tFuncRef, 1, 3, 2))))
	add("table64-ok", m(tables(bs(tFuncRef, 5, 1, 2))))
	add("table-count-limit", m(sec(4, uleb(100_001))))
	add("memory-count-limit", m(sec(5, uleb(100_001))))
	add("memory-flags", m(sec(5, vec(bs(0x10, 1)))))
	add("memory64-300000", memory64Pages(300_000))
	add("memory64-max", m(sec(5, vec(cat(5, 1, uleb(262_145))))))
	add("memory-multiple-ok", m(sec(5, vec(bs(0, 1), bs(0, 2)))))
	add("memory-past-end", m(sec(5, vec(bs(1, 1)))))

	// Globals and constant expressions.
	globals := func(entries ...[]byte) []byte { return sec(6, vec(entries...)) }
	add("global-flags", m(globals(bs(tI32, 3, 0x41, 0, 0x0b))))
	add("global-type-error", m(globals(bs(tI64, 0, 0x41, 0, 0x0b))))
	add("global-ref-func-oob", m(globals(bs(tFuncRef, 0, 0xd2, 5, 0x0b))))
	add("global-ref-null-type", m(globals(bs(tExternRef, 0, 0xd0, tFuncRef, 0x0b))))
	add("global-ref-null-oob", m(globals(bs(tFuncRef, 0, 0xd0, 9, 0x0b))))
	add("global-empty-init", m(globals(bs(tI32, 0))))
	add("global-missing-end", m(globals(bs(tI32, 0, 0x41, 0))))
	add("global-i32-leb", m(globals(bs(tI32, 0, 0x41, 0x80))))
	add("global-count-limit", m(sec(6, uleb(1_000_001))))
	add("global-v128-ok", m(globals(cat(tV128, 0, 0xfd, 0x0c, make([]byte, 16), 0x0b))))

	// Exports and start.
	fnMod := func(more ...[]byte) []byte {
		secs := append([][]byte{types(voidFn, functype(bs(tI32), nil)), sec(3, vec(uleb(0)))}, more...)
		return m(append(secs, sec(10, vec(body(noLocals, 0x0b))))...)
	}
	exports := func(entries ...[]byte) []byte { return sec(7, vec(entries...)) }
	add("export-utf8", fnMod(exports(cat("\xff", 0, 0))))
	add("export-duplicate", fnMod(exports(cat("a", 0, 0), cat("b", 0, 0), cat("a", 0, 0))))
	add("export-duplicate-long", fnMod(exports(cat(strings.Repeat("n", 60), 0, 0), cat(strings.Repeat("n", 60), 0, 0))))
	add("export-func-oob", fnMod(exports(cat("a", 0, 3))))
	add("export-table-oob", fnMod(exports(cat("a", 1, 0))))
	add("export-memory-oob", fnMod(exports(cat("a", 2, 0))))
	add("export-global-oob", fnMod(exports(cat("a", 3, 1))))
	add("export-tag-oob", fnMod(exports(cat("a", 4, 0))))
	add("export-kind", fnMod(exports(cat("a", 5, 0))))
	add("export-count-limit", fnMod(sec(7, uleb(1_000_001))))
	add("start-oob", fnMod(sec(8, uleb(2))))
	add("start-sig", m(types(voidFn, functype(bs(tI32), nil)), sec(3, vec(uleb(1))), sec(8, uleb(0)),
		sec(10, vec(body(noLocals, 0x0b)))))
	add("start-twice", fnMod(sec(8, uleb(0)), sec(8, uleb(0))))

	// Element segments.
	withTable := func(more ...[]byte) []byte {
		secs := append([][]byte{types(voidFn, bs(0x60, 1, tI32, 0)), sec(3, vec(uleb(0))), tables(bs(tFuncRef, 0, 1))}, more...)
		return m(append(secs, sec(10, vec(body(noLocals, 0x0b))))...)
	}
	elems := func(entries ...[]byte) []byte { return sec(9, vec(entries...)) }
	add("elem-flag", withTable(elems(bs(8))))
	add("elem-flag-big", withTable(elems(bs(0x80, 0x01))))
	add("elem-implicit-table", fnMod(elems(cat(0, 0x41, 0, 0x0b, vec()))))
	add("elem-table-index", withTable(elems(cat(2, 2, 0x41, 0, 0x0b, 0, vec()))))
	add("elem-kind", withTable(elems(cat(1, 1, vec()))))
	add("elem-type", withTable(elems(cat(6, 0, 0x41, 0, 0x0b, tExternRef, vec()))))
	add("elem-func-oob", withTable(elems(cat(0, 0x41, 0, 0x0b, vec(uleb(5))))))
	add("elem-expr-type", withTable(elems(cat(5, tFuncRef, vec(bs(0xd0, tExternRef, 0x0b))))))
	add("elem-expr-not-const", withTable(elems(cat(5, tFuncRef, vec(bs(0x00, 0x0b))))))
	add("elem-offset-type", withTable(elems(cat(0, 0x42, 0, 0x0b, vec()))))
	add("elem-table64-offset", m(types(voidFn), tables(bs(tFuncRef, 4, 1)), elems(cat(0, 0x41, 0, 0x0b, vec()))))
	add("elem-segment-count", withTable(sec(9, uleb(10_000_001))))
	add("elem-entry-count", withTable(elems(cat(1, 0, uleb(10_000_001)))))
	add("elem-typed-table", m(types(voidFn, bs(0x60, 1, tI32, 0)), sec(3, vec(uleb(0))), tables(bs(tRefNull, 1, 0, 1)),
		elems(cat(2, 0, 0x41, 0, 0x0b, 0, vec(uleb(0)))), sec(10, vec(body(noLocals, 0x0b)))))

	// Data segments, the data count, code.
	withMem := func(more ...[]byte) []byte {
		return m(append([][]byte{sec(5, vec(bs(0, 1)))}, more...)...)
	}
	datas := func(entries ...[]byte) []byte { return sec(11, vec(entries...)) }
	add("data-flag-3", withMem(datas(bs(3))))
	add("data-flag-8", withMem(datas(bs(8))))
	add("data-flag-16", withMem(datas(bs(0x10))))
	add("data-memory-oob", m(datas(cat(0, 0x41, 0, 0x0b, "x"))))
	add("data-memory-index-oob", withMem(datas(cat(2, 1, 0x41, 0, 0x0b, "x"))))
	add("data-offset-type", withMem(datas(cat(0, 0x42, 0, 0x0b, "x"))))
	add("data-past-end", withMem(datas(cat(1, 10, 'a'))))
	add("data-count-mismatch", withMem(sec(12, bs(2)), datas(cat(1, "x"))))
	add("data-count-no-data", withMem(sec(12, bs(1))))
	add("data-count-limit", withMem(sec(11, uleb(100_001))))
	add("datacount-limit", withMem(sec(12, uleb(100_001))))
	add("code-count-mismatch", m(types(voidFn), sec(3, vec(uleb(0))), sec(10, vec(body(noLocals, 0x0b), body(noLocals, 0x0b)))))
	add("code-without-functions", m(types(voidFn), sec(10, vec(body(noLocals, 0x0b)))))
	add("code-body-size", m(types(voidFn), sec(3, vec(uleb(0))), sec(10, cat(1, uleb(7_654_322)))))
	add("code-body-past-end", m(types(voidFn), sec(3, vec(uleb(0))), sec(10, bs(1, 5, 0, 0x0b))))

	// Tags.
	tags := func(entries ...[]byte) []byte { return sec(13, vec(entries...)) }
	add("tag-sig", m(types(voidFn), tags(bs(0, 3))))
	add("tag-attribute", m(types(voidFn), tags(bs(1, 0))))
	add("tag-result", m(types(functype(nil, bs(tI32))), tags(bs(0, 0))))
	add("tag-count-limit", m(sec(13, uleb(1_000_001))))
	return cs
}
