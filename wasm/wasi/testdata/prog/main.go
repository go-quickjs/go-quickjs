// Command prog is built for GOOS=wasip1 by the package's tests and run on
// the engine. Its first argument picks what it does; it prints what it
// finds, and exits 1 with a message on what it did not expect.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fail("no command")
	}
	arg := os.Args[2:]
	switch os.Args[1] {
	case "args":
		for _, a := range arg {
			fmt.Println("arg", a)
		}
		env := os.Environ()
		sort.Strings(env)
		for _, e := range env {
			fmt.Println("env", e)
		}
		fmt.Println("HOME", os.Getenv("HOME"))
	case "stdio":
		fmt.Fprint(os.Stdout, "to stdout\n")
		fmt.Fprint(os.Stderr, "to stderr\n")
	case "exit":
		var code int
		fmt.Sscan(arg[0], &code)
		fmt.Println("exiting")
		os.Exit(code)
	case "stdin":
		b, err := io.ReadAll(os.Stdin)
		check(err)
		fmt.Printf("read %d: %s\n", len(b), strings.ToUpper(string(b)))
	case "files":
		files(arg[0])
	case "readonly":
		readonly(arg[0])
	case "open":
		for _, p := range arg {
			_, err := os.ReadFile(p)
			fmt.Printf("%s: %v\n", p, errText(err))
		}
	case "clock":
		now := time.Now()
		fmt.Println("year", now.Year() >= 2024)
		start := time.Now()
		time.Sleep(50 * time.Millisecond)
		fmt.Println("slept", time.Since(start) >= 50*time.Millisecond)
	case "rand":
		var b [32]byte
		_, err := rand.Read(b[:])
		check(err)
		fmt.Println(hex.EncodeToString(b[:]))
	case "goroutines":
		goroutines()
	default:
		fail("unknown command " + os.Args[1])
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "FAIL:", msg)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func expect(cond bool, format string, args ...any) {
	if !cond {
		fail(fmt.Sprintf(format, args...))
	}
}

// errText returns err's message without the path, "ok" for nil.
func errText(err error) string {
	if err == nil {
		return "ok"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}

// files exercises a writable directory.
func files(dir string) {
	p := func(name string) string { return filepath.Join(dir, name) }

	check(os.WriteFile(p("a.txt"), []byte("hello, world\n"), 0o644))
	b, err := os.ReadFile(p("a.txt"))
	check(err)
	expect(string(b) == "hello, world\n", "read back %q", b)

	f, err := os.OpenFile(p("a.txt"), os.O_RDWR, 0)
	check(err)
	off, err := f.Seek(7, io.SeekStart)
	check(err)
	expect(off == 7, "seek to %d", off)
	_, err = f.Write([]byte("WORLD"))
	check(err)
	off, err = f.Seek(0, io.SeekCurrent)
	check(err)
	expect(off == 12, "offset after write %d", off)
	buf := make([]byte, 5)
	_, err = f.ReadAt(buf, 0)
	check(err)
	expect(string(buf) == "hello", "pread %q", buf)
	_, err = f.WriteAt([]byte("J"), 0)
	check(err)
	fi, err := f.Stat()
	check(err)
	expect(fi.Size() == 13 && fi.Mode().IsRegular(), "fstat size %d mode %v", fi.Size(), fi.Mode())
	check(f.Truncate(5))
	check(f.Sync())
	check(f.Close())
	b, err = os.ReadFile(p("a.txt"))
	check(err)
	expect(string(b) == "Jello", "after truncate %q", b)

	af, err := os.OpenFile(p("a.txt"), os.O_WRONLY|os.O_APPEND, 0)
	check(err)
	_, err = af.Write([]byte("+1"))
	check(err)
	check(af.Close())
	b, _ = os.ReadFile(p("a.txt"))
	expect(string(b) == "Jello+1", "after append %q", b)

	check(os.Mkdir(p("sub"), 0o755))
	expect(errors.Is(os.Mkdir(p("sub"), 0o755), fs.ErrExist), "mkdir twice")
	check(os.WriteFile(p("sub/b.txt"), []byte("bee"), 0o644))
	check(os.MkdirAll(p("sub/deep/er"), 0o755))
	st, err := os.Stat(p("sub"))
	check(err)
	expect(st.IsDir(), "sub is not a directory")

	ents, err := os.ReadDir(p("sub"))
	check(err)
	var names []string
	for _, e := range ents {
		names = append(names, fmt.Sprintf("%s:%v", e.Name(), e.IsDir()))
	}
	fmt.Println("readdir", strings.Join(names, " "))

	check(os.Rename(p("sub/b.txt"), p("c.txt")))
	_, err = os.Stat(p("sub/b.txt"))
	expect(errors.Is(err, fs.ErrNotExist), "renamed file still there: %v", err)
	b, _ = os.ReadFile(p("c.txt"))
	expect(string(b) == "bee", "renamed content %q", b)

	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	check(os.Chtimes(p("c.txt"), mt, mt))
	st, err = os.Stat(p("c.txt"))
	check(err)
	expect(st.ModTime().Equal(mt), "mtime %v", st.ModTime())

	fmt.Println("rmdir non-empty:", errText(os.Remove(p("sub/deep"))))
	check(os.RemoveAll(p("sub")))
	check(os.Remove(p("c.txt")))
	_, err = os.Stat(p("c.txt"))
	fmt.Println("stat removed:", errText(err))
	fmt.Println("open missing:", errText(func() error { _, err := os.Open(p("nope")); return err }()))
	fmt.Println("excl:", errText(func() error {
		_, err := os.OpenFile(p("a.txt"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		return err
	}()))

	// A big file, written and read in pieces.
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<14)
	check(os.WriteFile(p("big"), big, 0o644))
	b, err = os.ReadFile(p("big"))
	check(err)
	expect(bytes.Equal(b, big), "big file differs")
	check(os.Remove(p("big")))

	ents, err = os.ReadDir(dir)
	check(err)
	names = names[:0]
	for _, e := range ents {
		names = append(names, e.Name())
	}
	fmt.Println("left", strings.Join(names, " "))
	fmt.Println("files ok")
}

// readonly exercises a read-only file system.
func readonly(dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	check(err)
	fmt.Printf("hello.txt: %q\n", b)
	ents, err := os.ReadDir(dir)
	check(err)
	for _, e := range ents {
		fmt.Println("entry", e.Name(), e.IsDir())
	}
	b, err = os.ReadFile(filepath.Join(dir, "sub/inner.txt"))
	check(err)
	fmt.Printf("inner: %q\n", b)
	fi, err := os.Stat(filepath.Join(dir, "hello.txt"))
	check(err)
	fmt.Println("size", fi.Size())
	f, err := os.Open(filepath.Join(dir, "hello.txt"))
	check(err)
	_, err = f.Seek(6, io.SeekStart)
	check(err)
	rest, err := io.ReadAll(f)
	check(err)
	fmt.Printf("after seek: %q\n", rest)
	f.Close()
	fmt.Println("write:", errText(os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)))
	fmt.Println("mkdir:", errText(os.Mkdir(filepath.Join(dir, "d"), 0o755)))
	fmt.Println("remove:", errText(os.Remove(filepath.Join(dir, "hello.txt"))))
}

// goroutines runs goroutines that sleep, which the Go runtime waits for
// with poll_oneoff, and that hand values over channels.
func goroutines() {
	var wg sync.WaitGroup
	results := make(chan int, 5)
	start := time.Now()
	for i := 5; i >= 1; i-- {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 10 * time.Millisecond)
			results <- i
		}(i)
	}
	wg.Wait()
	close(results)
	var order []string
	for r := range results {
		order = append(order, fmt.Sprint(r))
	}
	fmt.Println("order", strings.Join(order, " "))

	// Three ticks, however coarse the host's timers: the timeout only
	// catches a ticker that never fires.
	ticker := time.NewTicker(5 * time.Millisecond)
	ticks := 0
	timeout := time.After(5 * time.Second)
loop:
	for ticks < 3 {
		select {
		case <-ticker.C:
			ticks++
		case <-timeout:
			break loop
		}
	}
	ticker.Stop()
	fmt.Println("ticked", ticks >= 3)
	// The longest sleep, 50 ms, then three ticks of 5: timers fire late,
	// never early.
	fmt.Println("elapsed", time.Since(start) >= 65*time.Millisecond)
}
