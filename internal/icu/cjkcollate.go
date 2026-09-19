package icu

import (
	_ "embed"
	"encoding/binary"
	"strings"
	"sync"
)

//go:embed cjkcollation.bin
var cjkCollationData []byte

const cjkCollationLimit = 0x33500

type packedCJKOrder struct {
	packed []byte
	once   sync.Once
	ranks  []int32
	sparse map[rune]int32
	groups int32
}

var (
	cjkOrdersOnce sync.Once
	cjkOrders     map[string]*packedCJKOrder
)

func cjkOrder(name string) *packedCJKOrder {
	cjkOrdersOnce.Do(func() {
		cjkOrders = map[string]*packedCJKOrder{}
		data := cjkCollationData
		if len(data) < 6 || string(data[:5]) != "QJCK\x01" {
			return
		}
		count := int(data[5])
		data = data[6:]
		for range count {
			nameBytes, rest, ok := takeCJKField(data)
			if !ok {
				return
			}
			packed, rest, ok := takeCJKField(rest)
			if !ok {
				return
			}
			cjkOrders[string(nameBytes)] = &packedCJKOrder{packed: packed}
			data = rest
		}
	})
	entry := cjkOrders[name]
	if entry == nil {
		return nil
	}
	entry.once.Do(func() {
		text, err := inflate(entry.packed)
		if err != nil || len(text)%3 != 0 {
			return
		}
		count := len(text) / 3
		if count <= 4096 {
			entry.sparse = make(map[rune]int32, count)
		} else {
			entry.ranks = make([]int32, cjkCollationLimit)
			for i := range entry.ranks {
				entry.ranks[i] = -1
			}
		}
		rank := int32(-1)
		for i := 0; i < len(text); i += 3 {
			value := uint32(text[i]) | uint32(text[i+1])<<8 | uint32(text[i+2])<<16
			if value&0x800000 != 0 {
				rank++
			}
			cp := value & 0x1fffff
			if entry.sparse != nil {
				entry.sparse[rune(cp)] = rank
			} else if cp < uint32(len(entry.ranks)) {
				entry.ranks[cp] = rank
			}
		}
		entry.groups = rank + 1
	})
	if len(entry.ranks) == 0 && len(entry.sparse) == 0 {
		return nil
	}
	return entry
}

func (o *packedCJKOrder) rank(r rune) (int32, bool) {
	if o.sparse != nil {
		rank, ok := o.sparse[r]
		return rank, ok
	}
	if r < 0 || int(r) >= len(o.ranks) {
		return 0, false
	}
	rank := o.ranks[r]
	return rank, rank >= 0
}

func takeCJKField(data []byte) ([]byte, []byte, bool) {
	size, n := binary.Uvarint(data)
	if n <= 0 || size > uint64(len(data)-n) {
		return nil, data, false
	}
	return data[n : n+int(size)], data[n+int(size):], true
}

type cjkBlock struct {
	order     *packedCJKOrder
	offset    int32
	foldKana  bool
	keepWhole bool
}

type cjkTailoring struct {
	anchor int32
	span   int32
	blocks []cjkBlock
}

func (c *cjkTailoring) keepsWhole(r rune) bool {
	for _, block := range c.blocks {
		if !block.keepWhole {
			continue
		}
		if _, ok := block.order.rank(r); ok {
			return true
		}
	}
	return false
}

func (c *cjkTailoring) weight(r rune) (int32, bool, bool) {
	for _, block := range c.blocks {
		if rank, ok := block.order.rank(r); ok {
			return c.anchor + block.offset + rank, true, block.foldKana
		}
	}
	return 0, false, false
}

var cjkTailoringCache sync.Map

func cjkTailoringFor(tag, collation string, table *collationTable) *cjkTailoring {
	language := strings.ToLower(strings.SplitN(tag, "-", 2)[0])
	possible := false
	switch language {
	case "zh":
		possible = collation == "default" || collation == "pinyin" ||
			collation == "stroke" || collation == "unihan" || collation == "zhuyin"
	case "ja":
		possible = collation == "default" || collation == "unihan"
	case "ko":
		possible = collation == "default" || collation == "searchjl" || collation == "unihan"
	}
	if !possible {
		return nil
	}
	key := tag + "\x00" + collation
	if cached, ok := cjkTailoringCache.Load(key); ok {
		return cached.(*cjkTailoring)
	}
	custom := makeCJKTailoring(tag, language, collation, table)
	if custom == nil {
		return nil
	}
	actual, _ := cjkTailoringCache.LoadOrStore(key, custom)
	return actual.(*cjkTailoring)
}

func makeCJKTailoring(tag, language, collation string, table *collationTable) *cjkTailoring {
	if collation == "default" {
		switch language {
		case "zh":
			collation = DefaultCollation(tag)
		case "ja", "ko":
		default:
			return nil
		}
	}

	anchor := func(r rune) int32 {
		primary, _, _, ok := table.weightsOf(r)
		if !ok {
			return 0
		}
		return primary * weightScale
	}
	packed := func(name string, foldKana, keepWhole bool) (cjkBlock, bool) {
		entry := cjkOrder(name)
		if entry == nil {
			return cjkBlock{}, false
		}
		return cjkBlock{order: entry, foldKana: foldKana, keepWhole: keepWhole}, true
	}
	build := func(at int32, blocks ...cjkBlock) *cjkTailoring {
		out := &cjkTailoring{anchor: at, blocks: blocks}
		for i := range out.blocks {
			out.blocks[i].offset = out.span
			groups := int32(0)
			if out.blocks[i].order != nil {
				groups = out.blocks[i].order.groups
			}
			out.span += groups + 1
		}
		return out
	}

	switch language {
	case "zh":
		var block cjkBlock
		var ok bool
		switch collation {
		case "pinyin":
			block, ok = packed("han-pinyin", false, false)
		case "stroke":
			block, ok = packed("han-stroke", false, false)
		case "zhuyin":
			block, ok = packed("han-zhuyin", false, false)
		case "unihan":
			block, ok = packed("han-unihan", false, false)
		}
		if ok {
			return build(anchor('A'), block)
		}
	case "ja":
		kana, ok := packed("kana-ja", true, true)
		if !ok {
			return nil
		}
		var han cjkBlock
		if collation == "unihan" {
			han, ok = packed("han-unihan", false, false)
		} else if collation == "default" {
			han, ok = packed("han-ja", false, false)
		} else {
			return nil
		}
		if ok {
			return build(anchor('α'), kana, han)
		}
	case "ko":
		if collation == "searchjl" {
			hangul, ok := packed("hangul-searchjl", false, true)
			if !ok {
				return nil
			}
			// Search-by-initial-consonant changes Hangul internally but leaves
			// the script in the root order.
			return build(anchor('ᄀ'), hangul)
		}
		hangul, ok := packed("hangul-ko", false, true)
		if !ok {
			return nil
		}
		var han cjkBlock
		if collation == "unihan" {
			han, ok = packed("han-unihan", false, false)
		} else if collation == "default" {
			han, ok = packed("han-ko", false, false)
		} else {
			return nil
		}
		if ok {
			return build(anchor('A'), hangul, han)
		}
	}
	return nil
}
