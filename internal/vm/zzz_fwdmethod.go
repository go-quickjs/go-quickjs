package vm

import "unsafe"

// fwdCache is the cache of leafForward's read of its method, this.k, on
// o: its site's, as long as the site has not given up -- one shape, or a
// few -- and once it has, the entry for o's shape in a table of caches of
// the forwarder's own (fwdSite), filled if it has not been. A template
// like Class.create's makes the constructor of every class, and its
// method's read is then one site of more shapes than a site's entries
// hold: the realm's shared cache (sharedProp) answered every
// construction, where the table's entry answers as a monomorphic site
// does. An object that keeps its prototype in its table (objPolyProto) is
// not filled in: the body's own read, whose site decides how the code's
// classes keep theirs (sharedProtos), is left to make it. Out of
// leafForward, which only calls it, and of the files before the layout
// lock's anchors (zzz_*), so that nothing they hold moves.
//
//go:noinline
func (r *Runtime) fwdCache(cl *closure, site int, o *Object, key Atom) *propCache {
	c := &cl.ic[site]
	if c.fills < maxCacheFills {
		return c
	}
	t := r.fwdSite(cl, o)
	if t.holder(o) == nil && t.fills < maxCacheFills && o.flags&objPolyProto == 0 {
		r.fillPropCache(t, o, key, false)
	}
	return t
}

// fwdWays is how many entries a forwarder's table of method reads has.
const fwdWays = 16

// fwdSite is the entry of cl's table of its method's reads (fwdCache)
// for o's shape: a property cache of its own, which fills and answers as
// any site's does (fillPropCache, cachedData) -- its holder's and its
// prototypes' layouts checked, or its chain's cell. Two shapes that meet
// in an entry take turns, and past maxCacheFills it gives up to the
// realm's shared cache as a site does. The table is made at the first read
// that needs it, so that a template that is no forwarder holds a nil.
func (r *Runtime) fwdSite(cl *closure, o *Object) *propCache {
	t := cl.fwd
	if t == nil {
		t = new([fwdWays]propCache)
		for i := range t {
			t[i].shape, t[i].ownShape = noShape, noShape
		}
		cl.fwd = t
	}
	return &t[uintptr(unsafe.Pointer(o.shape))>>5%fwdWays]
}
