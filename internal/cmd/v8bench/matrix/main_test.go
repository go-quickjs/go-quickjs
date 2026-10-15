package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runners' outputs read as their figures: a fixed run's milliseconds,
// a micro-benchmark's nanoseconds, the suite's scores and composite.
func TestFigures(t *testing.T) {
	names, vals := figures(`go-quickjs, fixed
Richards           14.0 ms        0.2 MB        1835 allocs
DeltaBlue          24.0 ms        1.4 MB       11172 allocs
TOTAL            1587.3 ms      373.3 MB     2929726 allocs
peak heap 167.7 MB, live after GC 9.3 MB`, "v8")
	if strings.Join(names, ",") != "Richards,DeltaBlue,TOTAL" || vals["DeltaBlue"] != 24 || vals["TOTAL"] != 1587.3 {
		t.Errorf("fixed: %v %v", names, vals)
	}
	names, vals = figures(`qjs.exe, micro
TEST                             N   TIME (ns)
empty_loop                33554432       5.349
func_call                  2097152      30.730
TOTAL            2522.1 ms`, "micro")
	if strings.Join(names, ",") != "empty_loop,func_call" || vals["func_call"] != 30.73 {
		t.Errorf("micro: %v %v", names, vals)
	}
	names, vals = figures(`Richards: 794
DeltaBlue: 790
----
Score (version 7): 1543`, "score")
	if strings.Join(names, ",") != "Richards,DeltaBlue,TOTAL" || vals["TOTAL"] != 1543 {
		t.Errorf("score: %v %v", names, vals)
	}
}

// The tables of a directory's runs: each benchmark's median over the runs,
// each go-quickjs column over QuickJS's, the micro-benchmarks' fastest and
// largest gaps.
func TestRender(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, v := range []string{"10.0", "30.0", "20.0"} {
		write("gq-v8-"+string(rune('0'+i))+".txt", "Richards  "+v+" ms\nTOTAL  "+v+" ms\n")
		write("qjs-v8-"+string(rune('0'+i))+".txt", "Richards  40.0 ms\nTOTAL  40.0 ms\n")
	}
	write("gq-micro-0.txt", "a  1  6.0\nb  1  2.0\n")
	write("gq-micro-1.txt", "a  1  5.0\nb  1  3.0\n")
	write("qjs-micro-0.txt", "a  1  2.5\nb  1  4.0\n")
	write("gq-score-0.txt", "Richards: 794\nScore (version 7): 1543\n")
	out, err := render(dir, 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Richards | 20.0 | 40.0 | 0.50x |",
		"| **TOTAL** | **20.0** | **40.0** | **0.50x** |",
		"| `a` | 5.00 ns | 2.50 ns | 2.00x |",
		"| `b` | 2.00 ns | 4.00 ns | 0.50x |",
		"takes 1.00 times QuickJS's time",
		"| **Composite** | 1,543 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if _, err := render(t.TempDir(), 12); err == nil {
		t.Error("an empty directory rendered")
	}
}

func TestGrouped(t *testing.T) {
	for v, want := range map[float64]string{7: "7", 999.6: "1,000", 1234567: "1,234,567"} {
		if got := grouped(v); got != want {
			t.Errorf("grouped(%v) = %s, want %s", v, got, want)
		}
	}
}
