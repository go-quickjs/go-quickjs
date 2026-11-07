// Command jsonscanbench says what the scan of long JSON strings does on the
// machine it runs on: whether it is right, how fast it reads against a loop
// that reads a word, and what JSON.parse and JSON.stringify come to. It is for
// a machine the project has no other way to measure: built with -tags purego
// it runs the same with the scan off, which is the comparison.
package main

import (
	"flag"
	"fmt"
	"math/bits"
	"os"
	"runtime"
	"strings"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/jsonscan"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

func main() {
	dur := flag.Duration("t", 100*time.Millisecond, "time for each measurement")
	flag.Parse()

	long := strings.Repeat("a", 4096)
	fmt.Printf("%s/%s %s, %d CPUs, scan %s\n\n", runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(),
		map[bool]string{true: "on", false: "OFF (purego)"}[jsonscan.ASCII(long) > 0])

	bad := check()
	fmt.Println()
	if jsonscan.ASCII(long) > 0 {
		scans(*dur)
		fmt.Println()
	}
	engine(*dur)
	if bad {
		os.Exit(1)
	}
}

func stopsASCII(c byte) bool { return c == '"' || c == '\\' || c < 0x20 || c >= 0x80 }

func stopsWide(c byte) bool { return c == '"' || c == '\\' || c < 0x20 || c == 0xED }

// check holds what a scan says to a plain loop, for each byte at each place.
func check() (bad bool) {
	on := jsonscan.ASCII(strings.Repeat("a", 4096)) > 0
	cases := 0
	test := func(s string, at int, stops func(byte) bool, name string, first int) {
		cases++
		ok := at >= 0 && at <= len(s)
		for i := 0; ok && i < at; i++ {
			ok = !stops(s[i])
		}
		if ok && on && at < len(s)-31 {
			ok = stops(s[at])
		}
		if !ok && !bad {
			bad = true
			fmt.Printf("WRONG: %s stopped at %d of %d, byte %#x at %d\n", name, at, len(s), s[min(first, len(s)-1)], first)
		}
	}
	for n := jsonscan.MinLength; n < 2*jsonscan.MinLength+34; n++ {
		b := []byte(strings.Repeat("a", n+32))
		for shift := range 17 {
			text := b[shift : shift+n]
			for at := 0; at < n; at++ {
				for c := range 256 {
					text[at] = byte(c)
					s := string(text)
					test(s, jsonscan.ASCII(s), stopsASCII, "ASCII", at)
					test(s, jsonscan.Wide(s, 0xED), stopsWide, "Wide", at)
				}
				text[at] = 'a'
			}
		}
	}
	if bad {
		fmt.Printf("correctness: FAILED over %d scans\n", cases)
	} else {
		fmt.Printf("correctness: ok, %d scans\n", cases)
	}
	return bad
}

// wordLoop is what is left where there is no scan: the parser's own loop.
func wordLoop(s string) int {
	const (
		ones   = 0x0101010101010101
		high   = 0x8080808080808080
		quote  = 0x2222222222222222
		slash  = 0x5C5C5C5C5C5C5C5C
		spaces = 0x2020202020202020
	)
	i := 0
	for ; i+8 <= len(s); i += 8 {
		w := wtf8.Load64(s, i)
		if m := (w | (w ^ quote - ones) | (w ^ slash - ones) | (w - spaces)) & high; m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
	}
	return i
}

var sink int

// measure is the ns an op of f takes, the best of a few runs of about d.
func measure(d time.Duration, f func()) float64 {
	n := 1
	for {
		t := time.Now()
		for range n {
			f()
		}
		// Long enough for a clock that steps, as Windows' does by 15.6 ms.
		if e := time.Since(t); e >= d {
			break
		}
		n *= 2
	}
	best := 1e18
	for range 5 {
		t := time.Now()
		for range n {
			f()
		}
		if ns := float64(time.Since(t).Nanoseconds()) / float64(n); ns < best {
			best = ns
		}
	}
	return best
}

func scans(d time.Duration) {
	fmt.Printf("%10s %12s %12s %12s %12s %7s\n", "bytes", "scan ns", "scan GB/s", "word ns", "word GB/s", "x word")
	for _, n := range []int{64, 96, 128, 256, 1024, 4096, 65536, 1 << 20, 16 << 20, 256 << 20} {
		s := strings.Repeat("ten chars.", n/10+1)[:n]
		scan := measure(d/2, func() { sink += jsonscan.ASCII(s) })
		word := measure(d/2, func() { sink += wordLoop(s) })
		fmt.Printf("%10d %12.1f %12.2f %12.1f %12.2f %7.2f\n", n, scan, float64(n)/scan, word, float64(n)/word, word/scan)
	}
}

const script = `
var long = "ten chars.".repeat(6554), longText = JSON.stringify(long);
var str1k = "x".repeat(1000), texts1k = JSON.stringify(Array.from({length: 100}, (_, i) => str1k + i));
var strs1k = JSON.parse(texts1k);
var str200 = "y".repeat(200), texts200 = JSON.stringify(Array.from({length: 100}, (_, i) => str200 + i));
var strs200 = JSON.parse(texts200);
var short = JSON.stringify(Array.from({length: 100}, (_, i) => "a plain string " + i)), shorts = JSON.parse(short);
var records = [];
for (var i = 0; i < 100; i++)
	records.push({id: i, name: "item " + i, price: i * 1.25, active: (i & 1) == 0, tags: ["a", "bb", "ccc"],
		size: {width: i % 100, height: 20.5, depth: null}});
var recordsText = JSON.stringify(records);
var global_res;
function parse(text, n) { for (var j = 0; j < n; j++) global_res = JSON.parse(text); }
function write(v, n) { for (var j = 0; j < n; j++) global_res = JSON.stringify(v); }
`

func engine(d time.Duration) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(script); err != nil {
		fmt.Println("script:", err)
		os.Exit(1)
	}
	fmt.Printf("%-34s %14s\n", "through the engine", "ns an op")
	for _, c := range []struct {
		name, code string
		ops        float64
	}{
		{"JSON.parse 65 KB string", "parse(longText, 200)", 200},
		{"JSON.stringify 65 KB string", "write(long, 200)", 200},
		{"JSON.parse 100 strings of 1 KB", "parse(texts1k, 200)", 200},
		{"JSON.stringify 100 strings of 1 KB", "write(strs1k, 200)", 200},
		{"JSON.parse 100 strings of 200 B", "parse(texts200, 500)", 500},
		{"JSON.stringify 100 strings of 200 B", "write(strs200, 500)", 500},
		{"JSON.parse 100 short strings", "parse(short, 2000)", 2000},
		{"JSON.stringify 100 short strings", "write(shorts, 2000)", 2000},
		{"JSON.parse 100 records", "parse(recordsText, 500)", 500},
		{"JSON.stringify 100 records", "write(records, 500)", 500},
	} {
		ns := measure(d, func() {
			if _, err := rt.Eval(c.code); err != nil {
				fmt.Println(c.name+":", err)
				os.Exit(1)
			}
		})
		fmt.Printf("%-34s %14.0f\n", c.name, ns/c.ops)
	}
}
