package stdlib_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// TestHelperHeartbeat is not a test: it is the program TestCloseStopsHostWork
// starts, which writes to a file until it is killed.
func TestHelperHeartbeat(t *testing.T) {
	path := os.Getenv("GO_QUICKJS_HEARTBEAT")
	if path == "" {
		t.Skip("run as a helper")
	}
	for {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.WriteString(".")
			f.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRuntimeContext pins that a runtime's context ends when it is closed,
// and a loop's when it or its runtime is.
func TestRuntimeContext(t *testing.T) {
	rt := quickjs.New()
	loop := stdlib.NewLoop(rt)
	if rt.Context().Err() != nil || loop.Context().Err() != nil {
		t.Fatal("a new runtime's context has ended")
	}
	rt.Close()
	if rt.Context().Err() == nil || loop.Context().Err() == nil {
		t.Error("closing the runtime left a context running")
	}

	rt = quickjs.New()
	defer rt.Close()
	loop = stdlib.NewLoop(rt)
	loop.Close()
	if loop.Context().Err() == nil {
		t.Error("closing the loop left its context running")
	}
	if rt.Context().Err() != nil {
		t.Error("closing the loop ended the runtime's context")
	}
}

// TestCloseStopsHostWork pins that closing a runtime stops what the standard
// library started for it: a request is abandoned, a program killed, and a
// server stops listening.
func TestCloseStopsHostWork(t *testing.T) {
	abandoned := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(abandoned)
	}))
	defer remote.Close()

	heartbeat := filepath.Join(t.TempDir(), "beat")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	rt := quickjs.New()
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{
		Loop:  loop,
		Fetch: &stdlib.Fetch{Allow: func(*http.Request) error { return nil }},
		Serve: &stdlib.Serve{Allow: func(string) error { return nil }},
		Run: &stdlib.Run{
			Allow: func(string, []string) error { return nil },
			Env:   map[string]string{"GO_QUICKJS_HEARTBEAT": heartbeat, "SYSTEMROOT": os.Getenv("SYSTEMROOT")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(fmt.Sprintf(`
		fetch(%q).catch(() => {});
		import("child_process").then(({default: cp}) => cp.execFile(%q, ["-test.run=^TestHelperHeartbeat$"]).catch(() => {}));
		serve({port: 0, hostname: "127.0.0.1"}, () => new Response("hi")).port`,
		remote.URL, exe))
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(v.Int())

	// Run until the program is beating, then close the runtime.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for size(heartbeat) == 0 {
		if ctx.Err() != nil {
			t.Fatal("the program never started")
		}
		short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
		loop.Run(short)
		stop()
	}
	rt.Close()

	select {
	case <-abandoned:
	case <-ctx.Done():
		t.Error("the request outlived the runtime")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		before := size(heartbeat)
		time.Sleep(200 * time.Millisecond)
		if size(heartbeat) == before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the program outlived the runtime")
		}
	}
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the server outlived the runtime")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func size(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
