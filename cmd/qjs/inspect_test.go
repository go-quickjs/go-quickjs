package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/wsproto"
)

// TestInspectBrk runs a program under --inspect-brk as a debugger attaches
// to node's: qjs says where it listens, waits, and stops at the program's
// first statement once told to run, the file named by its file URL; the
// debugger evaluates there and lets it go on to its end.
func TestInspectBrk(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "prog.js")
	if err := os.WriteFile(file, []byte("let x = 1;\nx = x + 1;\nconsole.log('x is', x);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errR, errW := io.Pipe()
	var out bytes.Buffer
	code := make(chan int, 1)
	go func() {
		code <- run([]string{"--inspect-brk=127.0.0.1:0", file}, strings.NewReader(""), &out, errW)
		errW.Close()
	}()
	lines := bufio.NewScanner(errR)
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "Debugger listening on ws://") {
		t.Fatalf("first line: %q", lines.Text())
	}
	ws := strings.TrimPrefix(lines.Text(), "Debugger listening on ")
	go io.Copy(io.Discard, errR)

	addr, path, _ := strings.Cut(strings.TrimPrefix(ws, "ws://"), "/")
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	key, _ := wsproto.Key()
	fmt.Fprintf(nc, "GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\n\r\n", path, addr, key)
	br := bufio.NewReader(nc)
	if res, err := http.ReadResponse(br, nil); err != nil || res.StatusCode != 101 {
		t.Fatalf("handshake: %v", err)
	}
	conn := wsproto.NewConn(nc, br, true, 0)
	id := 0
	send := func(method string, params map[string]any) int {
		id++
		b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		conn.WriteMessage(wsproto.OpText, b)
		return id
	}
	// next is the next message that is the answer to id, or the event
	// method.
	next := func(want any) map[string]any {
		nc.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("waiting for %v: %v", want, err)
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			if n, ok := want.(int); ok && m["id"] == float64(n) || m["method"] == want {
				return m
			}
		}
	}
	send("Runtime.enable", nil)
	next("Runtime.executionContextCreated")
	send("Debugger.enable", nil)
	next(send("Runtime.runIfWaitingForDebugger", nil))
	paused := next("Debugger.paused")["params"].(map[string]any)
	top := paused["callFrames"].([]any)[0].(map[string]any)
	loc := top["location"].(map[string]any)
	if loc["lineNumber"] != float64(0) || !strings.HasPrefix(top["url"].(string), "file:///") ||
		!strings.HasSuffix(top["url"].(string), "/prog.js") {
		t.Errorf("paused at %v in %v", loc, top["url"])
	}
	r := next(send("Debugger.evaluateOnCallFrame", map[string]any{"callFrameId": top["callFrameId"], "expression": "typeof x"}))
	if got := r["result"].(map[string]any)["result"].(map[string]any)["value"]; got != "undefined" {
		t.Errorf("typeof x before its declaration ran: %v", got)
	}
	send("Debugger.resume", nil)
	select {
	case c := <-code:
		if c != 0 || strings.TrimSpace(out.String()) != "x is 2" {
			t.Errorf("exit %d, output %q", c, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the program did not finish")
	}
}
