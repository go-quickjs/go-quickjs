// Command goprog is wasmbench's real-program workload, built for
// GOOS=wasip1 GOARCH=wasm: each subcommand does a program's kind of work
// with the standard library, checks what it computed, and prints a line
// that depends on all of it.
//
//	goprog sha256 MIB    hash MIB MiB, a 1 MiB buffer at a time
//	goprog sort N        sort.Ints of N pseudo-random ints
//	goprog json N        encoding/json round trips of N records
//	goprog regexp N      regexp matches and replacements over N lines
//	goprog gzip KIB      compress/gzip of KIB KiB of text, and back
//	goprog interp N      a bytecode interpreter's loop, N iterations
//
// Everything is made from fixed seeds, so the output is the same on every
// platform and engine.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fail("usage: goprog sha256|sort|json|regexp|gzip|interp N")
	}
	n, err := strconv.Atoi(os.Args[2])
	if err != nil || n <= 0 {
		fail("bad size " + os.Args[2])
	}
	switch os.Args[1] {
	case "sha256":
		sha(n)
	case "sort":
		sortInts(n)
	case "json":
		jsonRoundTrips(n)
	case "regexp":
		regexps(n)
	case "gzip":
		gzipRoundTrip(n)
	case "interp":
		interp(n)
	default:
		fail("unknown workload " + os.Args[1])
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

// sha hashes mib MiB: a 1 MiB buffer of pseudo-random bytes, whose first
// 32 bytes become the hash before it is hashed again.
func sha(mib int) {
	buf := make([]byte, 1<<20)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range buf {
		buf[i] = byte(r.Uint32())
	}
	var sum [32]byte
	for range mib {
		sum = sha256.Sum256(buf)
		copy(buf, sum[:])
	}
	fmt.Printf("sha256 %d MiB %s\n", mib, hex.EncodeToString(sum[:]))
}

// sortInts sorts n pseudo-random ints and checks the order.
func sortInts(n int) {
	r := rand.New(rand.NewPCG(3, 4))
	a := make([]int, n)
	var sum int
	for i := range a {
		a[i] = int(r.Int32())
		sum += a[i]
	}
	sort.Ints(a)
	h := crc32.NewIEEE()
	var after int
	for i, v := range a {
		if i > 0 && a[i-1] > v {
			fail("not sorted")
		}
		after += v
		if i%1024 == 0 {
			fmt.Fprint(h, v)
		}
	}
	if after != sum {
		fail("sorted ints are not the same ints")
	}
	fmt.Printf("sort %d min %d max %d crc %08x\n", n, a[0], a[n-1], h.Sum32())
}

// record is what the json workload encodes and decodes.
type record struct {
	ID     int                `json:"id"`
	Name   string             `json:"name"`
	Email  string             `json:"email"`
	Active bool               `json:"active"`
	Score  float64            `json:"score"`
	Tags   []string           `json:"tags"`
	Attrs  map[string]string  `json:"attrs"`
	Point  struct{ X, Y int } `json:"point"`
}

// jsonRoundTrips encodes n records, decodes them, checks they are the same,
// and does it again from the decoded ones, ten times.
func jsonRoundTrips(n int) {
	r := rand.New(rand.NewPCG(5, 6))
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	recs := make([]record, n)
	for i := range recs {
		rec := &recs[i]
		rec.ID = i
		rec.Name = words[r.IntN(len(words))] + " " + words[r.IntN(len(words))]
		rec.Email = fmt.Sprintf("user%d@example.com", r.IntN(100000))
		rec.Active = r.IntN(2) == 0
		rec.Score = float64(r.IntN(1000000)) / 1000
		for range r.IntN(4) {
			rec.Tags = append(rec.Tags, words[r.IntN(len(words))])
		}
		rec.Attrs = map[string]string{"k" + strconv.Itoa(r.IntN(10)): "v\"\n<" + strconv.Itoa(i)}
		rec.Point.X, rec.Point.Y = r.IntN(1000)-500, r.IntN(1000)-500
	}
	var total int
	var data []byte
	for range 10 {
		var err error
		data, err = json.Marshal(recs)
		if err != nil {
			fail(err.Error())
		}
		var back []record
		if err := json.Unmarshal(data, &back); err != nil {
			fail(err.Error())
		}
		if !reflect.DeepEqual(back, recs) {
			fail("json round trip changed the records")
		}
		recs = back
		total += len(data)
	}
	fmt.Printf("json %d bytes %d crc %08x\n", n, total, crc32.ChecksumIEEE(data))
}

// regexps matches e-mail addresses, dates and words over n lines of a log,
// and rewrites the dates.
func regexps(n int) {
	r := rand.New(rand.NewPCG(7, 8))
	paths := []string{"/", "/index.html", "/api/v1/users", "/static/app.js", "/login", "/search?q=go"}
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%d user%d@host%d.example.org GET %s 2026-%02d-%02d status=%d\n",
			i, r.IntN(5000), r.IntN(50), paths[r.IntN(len(paths))], 1+r.IntN(12), 1+r.IntN(28), 200+100*r.IntN(4))
	}
	text := b.String()
	email := regexp.MustCompile(`[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`)
	date := regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)
	errors := regexp.MustCompile(`(?m)^\d+ .* status=(4|5)\d\d$`)
	api := regexp.MustCompile(`GET /api/v\d+/(\w+)`)
	emails := len(email.FindAllStringIndex(text, -1))
	errs := len(errors.FindAllStringIndex(text, -1))
	apis := len(api.FindAllStringSubmatch(text, -1))
	rewritten := date.ReplaceAllString(text, "$3/$2/$1")
	if len(rewritten) != len(text) {
		fail("date rewrite changed the length")
	}
	fmt.Printf("regexp %d emails %d errors %d api %d crc %08x\n", n, emails, errs, apis, crc32.ChecksumIEEE([]byte(rewritten)))
}

// gzipRoundTrip compresses kib KiB of text made of words, decompresses it,
// and checks it is the same.
func gzipRoundTrip(kib int) {
	r := rand.New(rand.NewPCG(9, 10))
	words := strings.Fields("the quick brown fox jumps over the lazy dog while a wasm engine " +
		"interprets bytecode compiles functions to native code and collects garbage")
	var b bytes.Buffer
	for b.Len() < kib<<10 {
		b.WriteString(words[r.IntN(len(words))])
		if r.IntN(12) == 0 {
			b.WriteString(strconv.Itoa(r.IntN(1 << 20)))
			b.WriteByte('\n')
		} else {
			b.WriteByte(' ')
		}
	}
	src := b.Bytes()[:kib<<10]
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	if _, err := w.Write(src); err != nil {
		fail(err.Error())
	}
	if err := w.Close(); err != nil {
		fail(err.Error())
	}
	rd, err := gzip.NewReader(&z)
	if err != nil {
		fail(err.Error())
	}
	back, err := io.ReadAll(rd)
	if err != nil {
		fail(err.Error())
	}
	if !bytes.Equal(back, src) {
		fail("gzip round trip changed the data")
	}
	// The compressed size is left out: it is compress/flate's, and changes
	// with the Go release.
	fmt.Printf("gzip %d KiB round trip crc %08x\n", kib, crc32.ChecksumIEEE(src))
}

// The interpreter's instructions.
const (
	opPush  = iota // push the operand
	opLoad         // push local[operand]
	opStore        // pop into local[operand]
	opAdd
	opSub
	opMul
	opMod
	opLt   // pop b, a; push a < b
	opJz   // pop; jump to operand if zero
	opJmp  // jump to operand
	opHalt // stop; the result is local 0
)

type instr struct{ op, arg int }

// interp runs, on a stack machine, a program that sums i*i%7 + i%3 for i
// below n, and checks the sum against the same computed in Go.
func interp(n int) {
	const sum, i = 0, 1
	prog := []instr{
		{opPush, 0}, {opStore, sum},
		{opPush, 0}, {opStore, i},
		// 4: loop head
		{opLoad, i}, {opPush, n}, {opLt, 0}, {opJz, 25},
		{opLoad, sum},
		{opLoad, i}, {opLoad, i}, {opMul, 0}, {opPush, 7}, {opMod, 0}, {opAdd, 0},
		{opLoad, i}, {opPush, 3}, {opMod, 0}, {opAdd, 0},
		{opStore, sum},
		{opLoad, i}, {opPush, 1}, {opAdd, 0}, {opStore, i},
		{opJmp, 4},
		// 25:
		{opHalt, 0},
	}
	var locals [2]int
	stack := make([]int, 0, 16)
	steps := 0
	for pc := 0; ; pc++ {
		steps++
		in := prog[pc]
		switch in.op {
		case opPush:
			stack = append(stack, in.arg)
		case opLoad:
			stack = append(stack, locals[in.arg])
		case opStore:
			locals[in.arg] = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		case opAdd, opSub, opMul, opMod, opLt:
			a, b := stack[len(stack)-2], stack[len(stack)-1]
			stack = stack[:len(stack)-2]
			var v int
			switch in.op {
			case opAdd:
				v = a + b
			case opSub:
				v = a - b
			case opMul:
				v = a * b
			case opMod:
				v = a % b
			case opLt:
				if a < b {
					v = 1
				}
			}
			stack = append(stack, v)
		case opJz:
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if v == 0 {
				pc = in.arg - 1
			}
		case opJmp:
			pc = in.arg - 1
		case opHalt:
			want := 0
			for k := range n {
				want += k*k%7 + k%3
			}
			if locals[sum] != want {
				fail(fmt.Sprintf("interpreter computed %d, want %d", locals[sum], want))
			}
			fmt.Printf("interp %d sum %d steps %d\n", n, locals[sum], steps)
			return
		}
	}
}
