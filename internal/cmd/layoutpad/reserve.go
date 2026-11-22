package main

// A pad's reserve: the room it keeps when -pages records an anchor's page
// anew. A pad holds its anchor on its recorded address while the code
// before it changes by less than its room; past that, the anchor and
// everything after it move a page, every offset modulo 4 KB the same,
// which Zen 2 still tells apart (json_stringify_index_keys 4% slower with
// the JIT at every placement when mir's growth wrapped XJ). So where -pages
// must record a page anew, it records the one that leaves the pad at least
// its reserve, and later anchors a page on with it: the next such move is
// that much growth away. XJ, after the JIT's compiler, and CZ, after the
// native handlers' block, take more, since what is before them grows in
// most rounds.

import (
	"fmt"
	"sort"
)

// reserveOf is a lever's reserve in bytes.
func reserveOf(lever string) int64 {
	switch lever {
	case "XJ", "CZ":
		return 8192
	}
	return 2048
}

// padRoom is how many bytes a lever's pad can give back: its stores, or
// an assembly pad's slots past its first.
func padRoom(l lever, v int) int64 {
	if v <= 0 {
		return 0
	}
	if l.kind == 'a' {
		return int64(v-1) * slot
	}
	return int64(v) * perStore
}

// reservePages moves the recorded address of each of c's code anchors
// whose page syms has lost, and of those forced, by as many pages as its
// pad needs for its reserve, and every anchor after it by as much; v is
// c's levers as solved. It reports whether it moved one, which a solve
// then pads to.
func reservePages(c *config, syms *symtab, v map[string]int, forced map[string]bool) bool {
	type item struct {
		i    int
		addr uint64
	}
	var list []item
	for i, a := range c.anchors {
		if modOf(a.lever) != codeMod || a.fresh {
			continue
		}
		if s, ok := syms.byName[a.symbol]; ok {
			list = append(list, item{i, s.addr})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].addr < list[j].addr })
	var shift uint64
	for _, it := range list {
		a := &c.anchors[it.i]
		lost := a.at != 0 && it.addr/codeMod != a.at/codeMod
		if lost || forced[a.lever] {
			l, _ := leverByName(a.lever)
			if need := reserveOf(a.lever) - padRoom(l, v[a.lever]); need > 0 {
				pages := uint64(need+codeMod-1) / codeMod
				shift += pages * codeMod
				fmt.Printf("%s: anchor %s a further %d page(s) on, for its pad's reserve of %d bytes\n", c, a.lever, pages, reserveOf(a.lever))
			}
		}
		if shift != 0 {
			a.at = it.addr + shift
		}
	}
	return shift != 0
}
