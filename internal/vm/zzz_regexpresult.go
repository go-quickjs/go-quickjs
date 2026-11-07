package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// A match's array and its strings, made together. buildMatchResult made
// the array with room for its index, input and groups, its first four
// elements and the String of what matched, and the String of each group
// apart: the array, its elements past four, and the groups' Strings, one
// allocation where a single group is a piece of the subject (ownPiece) and
// one for all of them where two or more are, were up to three allocations
// for every match -- a URL's pattern of eleven groups, 576 bytes in three --
// and making them, a URL's exec spent more than its match did. Here the
// pieces are counted first, and the array of up to four elements is made
// with room for each of their Strings, in one allocation sized to them
// (matchBlock); a larger one, of up to sixteen, with its elements in its
// allocation and its pieces' Strings in one beside it, each its size: one of
// more than 512 bytes, which Go gives a header and a size class four times
// coarser, was no cheaper made as one. As V8 makes a result and its sliced
// strings by bumping one pointer, an exec of the URL's pattern that read
// every element went from 660 to 600 ns.

// matchBlock is a match's array, with room for its index, input and
// groups, its elements, E, an array of Values, and its pieces' Strings, S,
// an array of Strings.
type matchBlock[E, S any] struct {
	Object
	side   arrayExt
	fields [3]Value
	inline E
	strs   S
}

// matchBlockMax is the most elements a match's array is made with in one
// allocation; one of more is buildMatchResult's.
const matchBlockMax = 16

// newMatchBlock makes a matchBlock[E, S] with proto, n elements and k
// Strings, which its E and S have room for.
func newMatchBlock[E, S any](proto *Object, n, k int) (*Object, []String) {
	b := new(matchBlock[E, S])
	b.shape, b.class = rootFor(ClassArray, proto), ClassArray
	b.flags = objExtensible | objArrayLengthWritable | objInlineProps | objHasExt
	b.setRoom(&b.fields[0], len(b.fields))
	b.side.elems = unsafe.Slice((*Value)(unsafe.Pointer(&b.inline)), n)
	if k == 0 {
		// S is then [0]String, the block's last field, whose address is the
		// allocation's end: a slice of it, though empty, is a pointer past
		// the block, which checkptr refuses.
		return &b.Object, nil
	}
	return &b.Object, unsafe.Slice((*String)(unsafe.Pointer(&b.strs)), k)
}

// matchBlockOf is a match's array of n elements, 1 to matchBlockMax, and
// room for k Strings, 0 to n: in the array's allocation for up to four
// elements, beside it for more.
func matchBlockOf[_ any](proto *Object, n, k int) (*Object, []String) {
	switch n {
	case 1:
		return newMatchBlock[[1]Value, [1]String](proto, n, k)
	case 2:
		if k <= 1 {
			return newMatchBlock[[2]Value, [1]String](proto, n, k)
		}
		return newMatchBlock[[2]Value, [2]String](proto, n, k)
	case 3:
		switch {
		case k <= 1:
			return newMatchBlock[[3]Value, [1]String](proto, n, k)
		case k == 2:
			return newMatchBlock[[3]Value, [2]String](proto, n, k)
		}
		return newMatchBlock[[3]Value, [3]String](proto, n, k)
	case 4:
		switch {
		case k <= 1:
			return newMatchBlock[[4]Value, [1]String](proto, n, k)
		case k == 2:
			return newMatchBlock[[4]Value, [2]String](proto, n, k)
		case k == 3:
			return newMatchBlock[[4]Value, [3]String](proto, n, k)
		}
		return newMatchBlock[[4]Value, [4]String](proto, n, k)
	}
	var arr *Object
	switch {
	case n <= 8:
		arr, _ = newMatchBlock[[8]Value, [0]String](proto, n, 0)
	case n <= 12:
		arr, _ = newMatchBlock[[12]Value, [0]String](proto, n, 0)
	default:
		arr, _ = newMatchBlock[[16]Value, [0]String](proto, n, 0)
	}
	return arr, make([]String, k)
}

// matchResultOf is buildMatchResult's array, with its strings made with it
// (matchBlockOf): the array of a match of a pattern without the d flag, of
// fewer than matchBlockMax groups, once the first match has given the
// realm the layout of its index, input and groups (matchLayout), which it
// is made with as buildMatchResult makes it. exec's leaf (regexpLeaf's
// match), which every tier calls, makes its arrays here.
func matchResultOf[_ any](r *Runtime, re *regexp.Regexp, caps []int, input *String) *Object {
	n := len(caps) / 2
	t := r.matchLayout
	// Under a memory limit buildMatchResult cuts by its rule (kept).
	if t == nil || t.shape.proto != r.proto.array || n > matchBlockMax || re.Flags()&regexp.FlagHasIndices != 0 || r.meter != nil {
		return r.buildMatchResult(re, caps, input)
	}
	k := 0
	for i := 0; i < len(caps); i += 2 {
		if lo, hi := caps[i], caps[i+1]; lo >= 0 && hi > lo && (lo > 0 || hi < input.length) {
			k++
		}
	}
	arr, strs := matchBlockOf[struct{}](r.proto.array, n, k)
	elems := arr.extAt().elems
	for i := range elems {
		lo, hi := caps[2*i], caps[2*i+1]
		switch {
		case lo < 0 || hi < 0:
			// A group that did not participate reads as undefined, which
			// is distinct from having matched the empty string.
			elems[i] = Undefined
		case hi > lo && (lo > 0 || hi < input.length):
			elems[i] = Str(input.substringInto(&strs[0], lo, hi))
			strs = strs[1:]
		default:
			elems[i] = Str(input.Substring(lo, hi))
		}
	}
	groups := r.groupsObject(r.groupsLayoutOf(re), elems)
	arr.setVals(arr.vals()[:3])
	arr.shape = t.shape
	arr.vals()[0] = Int(caps[0])
	arr.vals()[1] = Str(input)
	arr.vals()[2] = groups
	return arr
}
