package inspector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/wsproto"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// client speaks the protocol to a target as DevTools does.
type client struct {
	t      *testing.T
	conn   *wsproto.Conn
	nextID int64

	mu      sync.Mutex
	replies map[int64]chan map[string]any
	events  chan map[string]any
}

func dial(t *testing.T, wsURL string) *client {
	t.Helper()
	addr, path, _ := strings.Cut(strings.TrimPrefix(wsURL, "ws://"), "/")
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := wsproto.Key()
	fmt.Fprintf(nc, "GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, addr, key)
	br := bufio.NewReader(nc)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %v %v", res, err)
	}
	c := &client{t: t, conn: wsproto.NewConn(nc, br, true, 0), replies: map[int64]chan map[string]any{},
		events: make(chan map[string]any, 1024)}
	go func() {
		defer close(c.events)
		for {
			op, data, err := c.conn.ReadMessage()
			if err != nil || op == wsproto.OpClose {
				return
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			if id, ok := m["id"].(float64); ok {
				c.mu.Lock()
				ch := c.replies[int64(id)]
				c.mu.Unlock()
				ch <- m
				continue
			}
			c.events <- m
		}
	}()
	t.Cleanup(c.conn.Close)
	return c
}

// call sends a request and waits for its answer.
func (c *client) call(method string, params map[string]any) map[string]any {
	c.t.Helper()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan map[string]any, 1)
	c.replies[id] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := c.conn.WriteMessage(wsproto.OpText, b); err != nil {
		c.t.Fatal(err)
	}
	select {
	case m := <-ch:
		if e, ok := m["error"]; ok {
			c.t.Fatalf("%s: %v", method, e)
		}
		res, _ := m["result"].(map[string]any)
		return res
	case <-time.After(10 * time.Second):
		c.t.Fatalf("%s: no answer", method)
	}
	return nil
}

// await is the next event of a method, those before it passed over.
func (c *client) await(method string) map[string]any {
	c.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-c.events:
			if !ok {
				c.t.Fatalf("closed waiting for %s", method)
			}
			if m["method"] == method {
				p, _ := m["params"].(map[string]any)
				return p
			}
		case <-timeout:
			c.t.Fatalf("no %s", method)
		}
	}
}

func get(m map[string]any, path ...any) any {
	var v any = m
	for _, k := range path {
		switch k := k.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

// newTarget is a runtime made WithDebugger, with a console, attached to a
// server of its own.
func newTarget(t *testing.T) (*quickjs.Runtime, *Server, *Target) {
	t.Helper()
	rt := quickjs.New(quickjs.WithDebugger())
	t.Cleanup(func() { rt.Close() })
	if err := stdlib.Console(rt, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	srv, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	tg, err := srv.Attach(rt, Options{Title: "test", URL: "file:///test.js"})
	if err != nil {
		t.Fatal(err)
	}
	return rt, srv, tg
}

const program = `function add(a, b) {
  const sum = a + b;
  return sum;
}
console.log("start", 1);
let total = add(2, 3);
total * 10;
`

// TestInspectorSession is a debugging session as DevTools runs one: the
// target listed, a breakpoint set before the program runs, the pause with
// its frames and scopes, evaluation in a frame, a step, the console's
// output, and the program going on to its result.
func TestInspectorSession(t *testing.T) {
	rt, srv, tg := newTarget(t)

	res, err := http.Get("http://" + srv.Addr() + "/json/list")
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]string
	json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list) != 1 || list[0]["webSocketDebuggerUrl"] != tg.WebSocketURL() || list[0]["type"] != "node" {
		t.Fatalf("list: %v", list)
	}

	done := make(chan []string, 1)
	go func() {
		var got []string
		c := dial(t, tg.WebSocketURL())
		c.call("Runtime.enable", nil)
		ctx := c.await("Runtime.executionContextCreated")
		got = append(got, fmt.Sprint("context ", get(ctx, "context", "id")))
		c.call("Debugger.enable", nil)
		bp := c.call("Debugger.setBreakpointByUrl", map[string]any{"url": "calc.js", "lineNumber": 1})
		c.call("Runtime.runIfWaitingForDebugger", nil)

		parsed := c.await("Debugger.scriptParsed")
		got = append(got, fmt.Sprint("parsed ", parsed["url"], " lines ", parsed["startLine"], "-", parsed["endLine"]))
		resolved := c.await("Debugger.breakpointResolved")
		got = append(got, fmt.Sprint("resolved ", resolved["breakpointId"] == bp["breakpointId"], " line ", get(resolved, "location", "lineNumber")))

		logged := c.await("Runtime.consoleAPICalled")
		got = append(got, fmt.Sprint("console ", logged["type"], " ", get(logged, "args", 0, "value"), " ", get(logged, "args", 1, "value"),
			" at line ", get(logged, "stackTrace", "callFrames", 0, "lineNumber")))

		p := c.await("Debugger.paused")
		top := get(p, "callFrames", 0).(map[string]any)
		got = append(got, fmt.Sprint("paused ", top["functionName"], " line ", get(top, "location", "lineNumber"),
			" hit ", get(p, "hitBreakpoints", 0) == bp["breakpointId"]))

		r := c.call("Debugger.evaluateOnCallFrame", map[string]any{"callFrameId": top["callFrameId"], "expression": "a * 100 + b"})
		got = append(got, fmt.Sprint("eval ", get(r, "result", "value")))

		local := get(top, "scopeChain", 0, "object", "objectId")
		props := c.call("Runtime.getProperties", map[string]any{"objectId": local, "ownProperties": true})
		var names []string
		for _, e := range props["result"].([]any) {
			e := e.(map[string]any)
			val := any("<unavailable>")
			if v, ok := e["value"].(map[string]any); ok {
				val = v["value"]
			}
			names = append(names, fmt.Sprint(e["name"], "=", val))
		}
		got = append(got, "local "+strings.Join(names, " "))

		thrown := c.call("Debugger.evaluateOnCallFrame", map[string]any{"callFrameId": top["callFrameId"], "expression": "nope.x"})
		got = append(got, fmt.Sprint("thrown ", get(thrown, "exceptionDetails", "exception", "className")))

		c.call("Debugger.stepOver", nil)
		c.await("Debugger.resumed")
		p = c.await("Debugger.paused")
		got = append(got, fmt.Sprint("stepped to line ", get(p, "callFrames", 0, "location", "lineNumber")))
		c.call("Debugger.resume", nil)
		c.await("Debugger.resumed")
		done <- got
	}()

	if err := tg.WaitForDebugger(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	v, err := rt.EvalFile("calc.js", program)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "50" {
		t.Errorf("result %s", v)
	}
	want := []string{
		"context 1",
		"parsed calc.js lines 0-7",
		"resolved true line 1",
		"console log start 1 at line 4",
		"paused add line 1 hit true",
		"eval 203",
		"local a=2 b=3 sum=<unavailable>",
		"thrown ReferenceError",
		"stepped to line 2",
	}
	select {
	case got := <-done:
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not finish")
	}
}

// TestInspectorClientGone pins a client disconnecting while the runtime is
// paused for it: the pause goes on, and its breakpoints are gone.
func TestInspectorClientGone(t *testing.T) {
	rt, _, tg := newTarget(t)
	go func() {
		c := dial(t, tg.WebSocketURL())
		c.call("Debugger.enable", nil)
		c.call("Debugger.setBreakpointByUrl", map[string]any{"url": "g.js", "lineNumber": 2})
		c.call("Runtime.runIfWaitingForDebugger", nil)
		c.await("Debugger.paused")
		c.conn.Close()
	}()
	if err := tg.WaitForDebugger(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	v, err := rt.EvalFile("g.js", "let n = 0;\ndebugger;\nn = 1;\nn")
	if err != nil || v.String() != "1" {
		t.Fatalf("%v %v", v, err)
	}
	// The breakpoint on line 3 went with its client: a second run stops
	// nowhere.
	if v, err := rt.EvalFile("g.js", "var m = 1;\nm = 2;\nm = 3;\nm"); err != nil || v.String() != "3" {
		t.Fatalf("%v %v", v, err)
	}
}

// TestInspectorRuntimeClosed pins a runtime closed with a client attached:
// the target leaves the list, and the client is disconnected.
func TestInspectorRuntimeClosed(t *testing.T) {
	rt, srv, tg := newTarget(t)
	c := dial(t, tg.WebSocketURL())
	rt.Close()
	select {
	case _, ok := <-c.events:
		if ok {
			for range c.events {
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client was not disconnected")
	}
	if l := srv.list(""); len(l) != 0 {
		t.Errorf("listed after Close: %v", l)
	}
}

// TestInspectorHost pins the Host a request must name: an address or
// localhost, not a name a web page could have pointed here.
func TestInspectorHost(t *testing.T) {
	_, srv, _ := newTarget(t)
	for host, want := range map[string]int{"localhost:9229": 200, "127.0.0.1": 200, "[::1]:1": 200, "evil.example": 403} {
		req, _ := http.NewRequest("GET", "http://"+srv.Addr()+"/json/list", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("Host %s: %d, want %d", host, res.StatusCode, want)
		}
	}
}

// TestInspectorServerClose pins the server closed while a client has the
// runtime paused: the pause goes on.
func TestInspectorServerClose(t *testing.T) {
	rt, srv, tg := newTarget(t)
	go func() {
		c := dial(t, tg.WebSocketURL())
		c.call("Debugger.enable", nil)
		c.call("Runtime.runIfWaitingForDebugger", nil)
		c.await("Debugger.paused")
		srv.Close()
	}()
	if err := tg.WaitForDebugger(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if v, err := rt.EvalFile("s.js", "debugger;\n'went on'"); err != nil || v.String() != "went on" {
		t.Fatalf("%v %v", v, err)
	}
}
