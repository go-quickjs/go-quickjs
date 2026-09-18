package vm

// Ranges: one number or date written against another.
//
// A range is not the two of them with a dash between. "Jan 1 – 5, 2024" says
// the year once and the month once, because what the two ends have in common
// is said once and what differs is said twice. So a range is made by writing
// both ends out, keeping the pieces they agree on, and putting the mark this
// language uses between the ones they do not.

// rangePieces is the two ends of a range written as one, along with where each
// piece came from: the start, the end, or the two of them.
type rangePieces struct {
	kinds   []string
	values  []string
	sources []string
}

func (p *rangePieces) add(kind, value, source string) {
	p.kinds = append(p.kinds, kind)
	p.values = append(p.values, value)
	p.sources = append(p.sources, source)
}

func (p *rangePieces) text() string {
	out := ""
	for _, v := range p.values {
		out += v
	}
	return out
}

// mergeRange writes the two ends of a range as one, with the mark between
// them. Where the two agree at the start or at the end, the pieces they agree
// on are written once and marked as shared.
func mergeRange(start, end []pieceOf, separator string) *rangePieces {
	out := &rangePieces{}

	// How far the two agree from the back. What comes before the last thing
	// they disagree about is written twice: "Jan 1 - Feb 5, 2024" says the
	// month twice because the day after it differs, and the year once.
	back := 0
	for back < len(start) && back < len(end) &&
		start[len(start)-1-back] == end[len(end)-1-back] {
		back++
	}
	// Once a later field is shared, an identical leading field can be shared
	// as well: "Jan 3 - 5, 2019". Do not do this when the last differing
	// field is at the end, since "Mar 4, 2019 - Mar 4, 2020" repeats the date.
	front := 0
	if back > 0 {
		for front < len(start)-back && front < len(end)-back && start[front] == end[front] {
			front++
		}
	}

	for _, piece := range start[:front] {
		out.add(piece.kind, piece.value, "shared")
	}
	for _, piece := range start[front : len(start)-back] {
		out.add(piece.kind, piece.value, "startRange")
	}
	out.add("literal", separator, "shared")
	for _, piece := range end[front : len(end)-back] {
		out.add(piece.kind, piece.value, "endRange")
	}
	for _, piece := range start[len(start)-back:] {
		out.add(piece.kind, piece.value, "shared")
	}
	return out
}

// mergeRangeAffixes writes a number range with the affixes the two ends share
// said once. A currency written after its number is shared by the range; a
// currency written before it is shared only when an explicit sign is shared
// too. ICU's automatic range collapse uses that distinction, so English says
// "$3 - $5" but "-$3-5", while Portuguese says "3 - 5 EUR".
func mergeRangeAffixes(start, end []pieceOf, separator string) (*rangePieces, bool) {
	front := commonAffixPrefix(start, end)
	back := commonAffixSuffix(start[front:], end[front:])

	// A prefix on its own is repeated unless it includes the common sign.
	if front > 0 && !hasSign(start[:front]) {
		front = 0
		back = commonAffixSuffix(start, end)
	}
	if front == 0 && back == 0 {
		return nil, false
	}

	out := &rangePieces{}
	for _, piece := range start[:front] {
		out.add(piece.kind, piece.value, "shared")
	}
	for _, piece := range start[front : len(start)-back] {
		out.add(piece.kind, piece.value, "startRange")
	}
	out.add("literal", separator, "shared")
	for _, piece := range end[front : len(end)-back] {
		out.add(piece.kind, piece.value, "endRange")
	}
	for _, piece := range start[len(start)-back:] {
		out.add(piece.kind, piece.value, "shared")
	}
	return out, true
}

func commonAffixPrefix(a, b []pieceOf) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] && !numberRangeCore(a[n].kind) {
		n++
	}
	return n
}

func commonAffixSuffix(a, b []pieceOf) int {
	n := 0
	for n < len(a) && n < len(b) &&
		a[len(a)-1-n] == b[len(b)-1-n] && !numberRangeCore(a[len(a)-1-n].kind) {
		n++
	}
	return n
}

func numberRangeCore(kind string) bool {
	switch kind {
	case "integer", "group", "decimal", "fraction", "compact", "exponentSeparator",
		"exponentMinusSign", "exponentInteger", "nan", "infinity":
		return true
	}
	return false
}

func hasSign(pieces []pieceOf) bool {
	for _, piece := range pieces {
		if piece.kind == "plusSign" || piece.kind == "minusSign" {
			return true
		}
	}
	return false
}

// joinRange writes the two ends out in full with the mark between them, which
// is what a number does: what they have in common is said twice.
func joinRange(start, end []pieceOf, separator string) *rangePieces {
	out := &rangePieces{}
	for _, piece := range start {
		out.add(piece.kind, piece.value, "startRange")
	}
	out.add("literal", separator, "shared")
	for _, piece := range end {
		out.add(piece.kind, piece.value, "endRange")
	}
	return out
}

// pieceOf is a formatted piece, of a number or of a date, in the one shape a
// range can work with.
type pieceOf struct {
	kind  string
	value string
}

func numberPiecesOf(pieces []numberPiece) []pieceOf {
	out := make([]pieceOf, len(pieces))
	for i, p := range pieces {
		out[i] = pieceOf{p.kind, p.value}
	}
	return out
}

func datePiecesOf(pieces []datePiece) []pieceOf {
	out := make([]pieceOf, len(pieces))
	for i, p := range pieces {
		out[i] = pieceOf{p.kind, p.value}
	}
	return out
}

// sameRange is what is written when the two ends come to the same thing: the
// one value, said to be approximate.
func sameRange(pieces []pieceOf, approximately string) *rangePieces {
	out := &rangePieces{}
	if approximately != "" {
		out.add("approximatelySign", approximately, "shared")
	}
	for _, piece := range pieces {
		out.add(piece.kind, piece.value, "shared")
	}
	return out
}
