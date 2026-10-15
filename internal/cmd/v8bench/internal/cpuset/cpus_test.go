package cpuset

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// -cpus reads groups of CPUs and ranges, separated by semicolons, and
// refuses what is not one, or a CPU in two groups.
func TestParseCPUs(t *testing.T) {
	for _, c := range []struct {
		spec string
		want []string
	}{
		{"0-5;6-11", []string{"0-5", "6-11"}},
		{"0-2,6-8;3-5,9-11", []string{"0-2,6-8", "3-5,9-11"}},
		{" 3, 1,2 ; 7 ", []string{"1-3", "7"}},
		{"0-1;;2-3", []string{"0-1", "2-3"}},
	} {
		g, _, err := Parse(c.spec)
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		var got []string
		for _, s := range g {
			got = append(got, s.String())
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.spec, got, c.want)
		}
	}
	for _, spec := range []string{"a-b", "5-2", "0-3;3-5", "1,x"} {
		if _, _, err := Parse(spec); err == nil {
			t.Errorf("%q: no error", spec)
		}
	}
	h, _, err := Parse("halves")
	if err != nil || len(h) != 2 || len(h[0])+len(h[1]) != runtime.NumCPU() {
		t.Errorf("halves: %v, %v", h, err)
	}
	// auto gives two groups or more, apart, whatever the machine.
	a, how, err := Parse("auto")
	if err != nil || len(a) < 2 {
		t.Fatalf("auto: %v (%s), %v", a, how, err)
	}
	t.Logf("auto: %v, %s", a, how)
	seen := map[int]bool{}
	for _, g := range a {
		for _, c := range g {
			if seen[c] || c >= runtime.NumCPU() {
				t.Errorf("auto: CPU %d twice or past the machine's", c)
			}
			seen[c] = true
		}
	}
}

// -cpus auto keeps to the fastest cores where the machine's differ, groups
// them by their level 3 caches, else halves them, and gives each group as
// many CPUs.
func TestAutoGroups(t *testing.T) {
	cpus := func(n int) Set {
		s := make(Set, n)
		for i := range s {
			s[i] = i
		}
		return s
	}
	ryzen := []Set{{0, 1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11}}
	// A hybrid Intel: eight P-cores' sixteen threads, eight E-cores, one cache.
	hybrid := map[int]int{}
	for c := range 24 {
		hybrid[c] = 0
		if c < 16 {
			hybrid[c] = 1
		}
	}
	// Two caches of mixed cores, as AMD's Zen 5 and Zen 5c complexes: the
	// fast ones 0-3 and 8-9.
	mixed := map[int]int{}
	for c := range 12 {
		mixed[c] = 0
		if c < 4 || c == 8 || c == 9 {
			mixed[c] = 1
		}
	}
	for _, c := range []struct {
		name    string
		cache   []Set
		classes map[int]int
		all     Set
		want    []string
	}{
		{"Ryzen 3600", ryzen, nil, cpus(12), []string{"0-5", "6-11"}},
		{"cores alike", ryzen, map[int]int{0: 1, 6: 1}, cpus(12), []string{"0-5", "6-11"}},
		{"one cache", []Set{cpus(8)}, nil, cpus(8), []string{"0-3", "4-7"}},
		{"no caches known", nil, nil, cpus(6), []string{"0-2", "3-5"}},
		{"hybrid Intel", []Set{cpus(24)}, hybrid, cpus(24), []string{"0-7", "8-15"}},
		{"mixed complexes", []Set{{0, 1, 2, 3, 4, 5, 6, 7}, {8, 9, 10, 11}}, mixed, cpus(12), []string{"0-1", "8-9"}},
	} {
		g, how := autoGroups(c.cache, nil, c.classes, c.all)
		var got []string
		for _, s := range g {
			got = append(got, s.String())
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v (%s), want %v", c.name, got, how, c.want)
		}
	}
}

// Where the machine has NUMA nodes, no group spans two, and the labels'
// two are of two nodes.
func TestAutoGroupsNUMA(t *testing.T) {
	span := func(lo, hi int) Set {
		var s Set
		for c := lo; c <= hi; c++ {
			s = append(s, c)
		}
		return s
	}
	// Two sockets of 16 CPUs, one cache each.
	sockets := []Set{span(0, 15), span(16, 31)}
	// An EPYC in NPS4: four nodes of 16 CPUs, each two complexes' caches.
	var nps4Cache, nps4Nodes []Set
	for i := range 8 {
		nps4Cache = append(nps4Cache, span(8*i, 8*i+7))
	}
	for i := range 4 {
		nps4Nodes = append(nps4Nodes, span(16*i, 16*i+15))
	}
	// One cache across two nodes, as sub-NUMA clustering has it, whose CPUs
	// are numbered across them, so that the nodes are not the halves.
	snc := []Set{append(span(0, 3), span(8, 11)...), append(span(4, 7), span(12, 15)...)}
	for _, c := range []struct {
		name         string
		cache, nodes []Set
		all          Set
		want         []string
	}{
		{"two sockets", sockets, sockets, span(0, 31), []string{"0-15", "16-31"}},
		{"EPYC NPS4", nps4Cache, nps4Nodes, span(0, 63), []string{"0-7", "16-23"}},
		{"one cache, two nodes", []Set{span(0, 15)}, snc, span(0, 15), []string{"0-3,8-11", "4-7,12-15"}},
		{"no caches, two nodes", nil, snc, span(0, 15), []string{"0-3,8-11", "4-7,12-15"}},
		{"one node", []Set{span(0, 5), span(6, 11)}, []Set{span(0, 11)}, span(0, 11), []string{"0-5", "6-11"}},
	} {
		g, how := autoGroups(c.cache, c.nodes, nil, c.all)
		got := []string{g[0].String(), g[1].String()}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v (%s), want %v", c.name, got, how, c.want)
		}
	}
}

// Start starts a process on the CPUs it is given, where the OS
// gives a process its own -- the test binary again, which reports the
// CPUs it may run on -- and starts it where the OS does not.
func TestStartPinned(t *testing.T) {
	if os.Getenv("CPUSET_PINNED_CHILD") != "" {
		set, err := Allowed()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("allowed %s\n", set)
		return
	}
	groups, how, err := Parse("auto")
	if err != nil {
		t.Fatal(err)
	}
	want := groups[len(groups)-1]
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestStartPinned$", "-test.v")
	cmd.Env = append(os.Environ(), "CPUSET_PINNED_CHILD=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := Start(cmd, want); err != nil {
		t.Fatalf("on %s (%s): %v", want, how, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("on %s: %v\n%s", want, err, out.String())
	}
	var got string
	for _, line := range strings.Split(out.String(), "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "allowed "); ok {
			got = s
		}
	}
	if got == "" {
		t.Fatalf("the child said no CPUs:\n%s", out.String())
	}
	if CanPin && got != want.String() {
		t.Errorf("pinned to %s (%s), the child may run on %s", want, how, got)
	}
	t.Logf("on %s (%s): the child may run on %s", want, how, got)
}
