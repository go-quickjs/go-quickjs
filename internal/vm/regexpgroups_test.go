package vm

import "testing"

// The groups objects of a pattern's matches after the first share the
// layout the first one had, a template, until one changes its own; a pattern with more names
// than a table scans has none to share, and makes each object as the first.
func TestGroupsLayoutShared(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		function d(s) { return /(?<y>\d+)-(?<m>\d+)/.exec(s).groups }
		var g0 = d("0-0"), g1 = d("1-2"), g2 = d("3-4"), g3 = d("5-6")
		g3.z = 1
		var many = /(?<a>.)(?<b>.)(?<c>.)(?<d>.)(?<e>.)(?<f>.)(?<g>.)(?<h>.)(?<i>.)/
		var h1 = many.exec("123456789").groups, h2 = many.exec("abcdefghi").groups`)); err != nil {
		t.Fatal(err)
	}
	g1, g2, g3 := globalObject(r, "g1"), globalObject(r, "g2"), globalObject(r, "g3")
	if g1.shape != g2.shape || g1.shape.unique || !g1.shape.loose {
		t.Fatalf("groups objects do not share a template: %p %p", g1.shape, g2.shape)
	}
	if g3.shape == g1.shape || g1.shape.n != 2 || len(g3.vals) != 3 {
		t.Fatalf("a groups object given a name changed the template: %d keys", g1.shape.n)
	}
	if h1, h2 := globalObject(r, "h1"), globalObject(r, "h2"); h1.shape == h2.shape || h2.shape.n != 9 {
		t.Fatalf("groups objects of nine names share a layout %p", h1.shape)
	}
}
