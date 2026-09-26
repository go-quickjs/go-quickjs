package vm

// rangePieces is a range of numbers or dates as go-intl wrote it, one piece
// at a time, along with where each piece came from: the start, the end, or
// the two of them, as formatRangeToParts reports it.
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
