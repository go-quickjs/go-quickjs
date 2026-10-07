// Package inspector serves the Chrome DevTools Protocol for go-quickjs
// runtimes made WithDebugger, so that Chrome's DevTools, VS Code and any
// other client of the protocol can attach to one -- set breakpoints, step,
// look at the stack and its variables, evaluate code, see what the console
// writes -- as they attach to Node run with --inspect.
//
// One Server serves any number of runtimes, each a target of its own, which
// it lists at /json/list as Node lists its own: a program's runtime and its
// workers', each attached with Attach. A client opens a target's WebSocket
// URL; more than one may be attached to a target at once.
//
// A runtime belongs to one goroutine, and what a client asks is done on it:
// while the runtime is paused, by the pause, which serves clients until one
// says to go on; while it runs a script, at its next interrupt check; and
// while it waits, as a job its host's loop runs. A host that runs no loop of
// its own -- one that only calls Eval -- has a client's requests done the
// next time a script runs.
//
// A debugger can read and change anything a script can, and run code in
// it. A Server listens where it is told, which should be the loopback
// interface, as Node's does by default, and turns away a request whose Host
// is neither an address nor localhost, against a web page reaching it
// through DNS rebinding.
package inspector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/internal/wsproto"
)

// DefaultAddr is where Node's inspector listens unless told otherwise.
const DefaultAddr = "127.0.0.1:9229"

// Server serves the DevTools protocol for the runtimes attached to it.
type Server struct {
	ln   net.Listener
	http *http.Server

	mu      sync.Mutex
	targets []*Target
	closed  bool
}

// Listen starts a server listening at addr, host:port, DefaultAddr if it
// is empty; port 0 picks one, which Addr then says.
func Listen(addr string) (*Server, error) {
	if addr == "" {
		addr = DefaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln}
	s.http = &http.Server{Handler: http.HandlerFunc(s.serveHTTP)}
	go s.http.Serve(ln)
	return s, nil
}

// Addr is the address the server listens at.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the server and detaches every target from it. A runtime
// paused by one of its clients goes on.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	targets := s.targets
	s.targets = nil
	s.mu.Unlock()
	for _, t := range targets {
		t.Detach()
	}
	return s.http.Close()
}

// Options are what a target is attached with.
type Options struct {
	// Title is what the target is listed as: a program's file name, or a
	// worker's.
	Title string
	// URL is the target's own, which a client shows: the program's file
	// URL, say.
	URL string
	// ScriptURL is the URL a client is told a script has, made from the
	// name it was loaded as -- a file URL from a path, say -- and which
	// the client's breakpoints name it by. Nil tells the name as it is.
	ScriptURL func(name string) string
}

// Target is a runtime attached to a server.
type Target struct {
	s    *Server
	id   string
	opts Options
	rt   *quickjs.Runtime
	// vm is the runtime's VM, kept from when it was attached: the runtime
	// lets go of it when it is closed.
	vm *vm.Runtime

	mu       sync.Mutex
	sessions []*session
	detached bool

	// What follows is the runtime goroutine's.
	pause      *vm.DebugPause
	pauseSeq   int
	resume     vm.StepAction
	resumed    bool
	conditions map[int]string
	owner      map[int]*session
	skipAll    bool
	released   bool
	// breakOnStart says the next pause is the one WaitForDebugger asked
	// for, which Node calls so.
	breakOnStart bool
	console      bool
}

// Attach makes a runtime made WithDebugger a target of the server, which a
// client may attach to until the runtime is closed or Detach is called. It
// is called on the runtime's goroutine.
func (s *Server) Attach(rt *quickjs.Runtime, opts Options) (*Target, error) {
	v := hostaccess.VM(rt)
	if v == nil {
		return nil, quickjs.ErrClosed
	}
	if !v.Debugging() {
		return nil, errors.New("inspector: the runtime was not made WithDebugger")
	}
	if opts.ScriptURL == nil {
		opts.ScriptURL = func(name string) string { return name }
	}
	t := &Target{s: s, id: newID(), opts: opts, rt: rt, vm: v,
		conditions: map[int]string{}, owner: map[int]*session{}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("inspector: the server is closed")
	}
	s.targets = append(s.targets, t)
	s.mu.Unlock()
	v.SetDebugHandler(t)
	rt.OnClose(t.runtimeClosed)
	return t, nil
}

// ID is the target's ID, the last part of its WebSocket URL.
func (t *Target) ID() string { return t.id }

// WebSocketURL is where a client attaches to the target.
func (t *Target) WebSocketURL() string { return "ws://" + t.s.Addr() + "/" + t.id }

// DevToolsURL opens Chrome's DevTools on the target, pasted into Chrome.
func (t *Target) DevToolsURL() string {
	return "devtools://devtools/bundled/js_app.html?experiments=true&v8only=true&ws=" + t.s.Addr() + "/" + t.id
}

// Detach ends the target: its clients are disconnected, the breakpoints
// they set removed, and a pause they made goes on. It may be called from
// any goroutine.
func (t *Target) Detach() {
	if !t.unlist() {
		return
	}
	t.vm.DebugPost(func() {
		t.vm.SetDebugHandler(nil)
		for id := range t.owner {
			t.vm.RemoveBreakpoint(id)
		}
		t.owner = map[int]*session{}
		t.released = true
	})
}

// unlist takes the target off its server's list and disconnects its
// clients, and reports whether it was the first to.
func (t *Target) unlist() bool {
	t.mu.Lock()
	if t.detached {
		t.mu.Unlock()
		return false
	}
	t.detached = true
	sessions := t.sessions
	t.sessions = nil
	t.mu.Unlock()
	t.s.mu.Lock()
	for i, o := range t.s.targets {
		if o == t {
			t.s.targets = append(t.s.targets[:i:i], t.s.targets[i+1:]...)
			break
		}
	}
	t.s.mu.Unlock()
	for _, c := range sessions {
		c.conn.SendClose(1001, "the target is gone")
		c.conn.Close()
	}
	return true
}

// runtimeClosed is the runtime closing, on its goroutine: the target goes,
// and nothing more is asked of the runtime.
func (t *Target) runtimeClosed() {
	t.unlist()
	t.released = true
}

// WaitForDebugger blocks the runtime's goroutine, serving clients, until
// one says to run -- Runtime.runIfWaitingForDebugger, as Node's
// --inspect-wait and --inspect-brk do -- or the runtime closes, or ctx is
// done. With pause set, the program then stops at its first statement.
func (t *Target) WaitForDebugger(ctx context.Context, pause bool) error {
	for !t.released {
		select {
		case <-t.vm.DebugReady():
			t.vm.DebugDrain()
		case <-t.vm.DebugClosed():
			return quickjs.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if pause && !t.isDetached() {
		t.breakOnStart = true
		t.vm.PauseAtNextStatement()
	}
	return nil
}

func (t *Target) isDetached() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.detached
}

// clients are the target's sessions now.
func (t *Target) clients() []*session {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*session(nil), t.sessions...)
}

// newID is a target's ID, a random UUID as Node's are.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// serveHTTP answers the discovery endpoints and upgrades a target's
// WebSocket.
func (s *Server) serveHTTP(w http.ResponseWriter, req *http.Request) {
	if !allowedHost(req.Host) {
		http.Error(w, "Host header is not an IP address or localhost", http.StatusForbidden)
		return
	}
	switch strings.TrimSuffix(req.URL.Path, "/") {
	case "/json", "/json/list":
		s.writeJSON(w, s.list(req.Host))
		return
	case "/json/version":
		s.writeJSON(w, map[string]string{"Browser": "go-quickjs", "Protocol-Version": "1.3"})
		return
	}
	id := strings.TrimPrefix(req.URL.Path, "/")
	s.mu.Lock()
	var t *Target
	for _, o := range s.targets {
		if o.id == id {
			t = o
		}
	}
	s.mu.Unlock()
	if t == nil || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		http.NotFound(w, req)
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	hj, ok := w.(http.Hijacker)
	if key == "" || !ok {
		http.Error(w, "not a WebSocket handshake", http.StatusBadRequest)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsproto.Accept(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return
	}
	t.serve(wsproto.NewConn(conn, rw.Reader, false, 256<<20))
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	b, _ := json.MarshalIndent(v, "", "  ")
	w.Write(b)
}

// list is /json/list: every target, as Node lists its own.
func (s *Server) list(host string) []map[string]string {
	if host == "" {
		host = s.Addr()
	}
	s.mu.Lock()
	targets := append([]*Target(nil), s.targets...)
	s.mu.Unlock()
	out := []map[string]string{}
	for _, t := range targets {
		ws := host + "/" + t.id
		out = append(out, map[string]string{
			"description":               "go-quickjs instance",
			"devtoolsFrontendUrl":       "devtools://devtools/bundled/js_app.html?experiments=true&v8only=true&ws=" + ws,
			"devtoolsFrontendUrlCompat": "devtools://devtools/bundled/inspector.html?experiments=true&v8only=true&ws=" + ws,
			"faviconUrl":                "https://nodejs.org/static/images/favicons/favicon.ico",
			"id":                        t.id,
			"title":                     t.opts.Title,
			"type":                      "node",
			"url":                       t.opts.URL,
			"webSocketDebuggerUrl":      "ws://" + ws,
		})
	}
	return out
}

// allowedHost reports whether a request's Host names an address or
// localhost, as Node's inspector asks.
func allowedHost(host string) bool {
	if host == "" {
		return true
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.EqualFold(h, "localhost") || strings.EqualFold(h, "localhost.") || net.ParseIP(h) != nil
}

// serve runs a client's connection: what it asks is done on the runtime's
// goroutine, in the order it asked.
func (t *Target) serve(conn *wsproto.Conn) {
	c := &session{t: t, conn: conn, objects: map[string]*remote{}}
	t.mu.Lock()
	if t.detached {
		t.mu.Unlock()
		conn.SendClose(1001, "the target is gone")
		conn.Close()
		return
	}
	t.sessions = append(t.sessions, c)
	t.mu.Unlock()
	go func() {
		defer t.drop(c)
		for {
			op, data, err := conn.ReadMessage()
			if err != nil || op == wsproto.OpClose {
				return
			}
			var m message
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			t.vm.DebugPost(func() { c.handle(m) })
		}
	}()
}

// drop is a client gone: the breakpoints it set go, and a pause only it
// was attending goes on.
func (t *Target) drop(c *session) {
	conn := c.conn
	conn.Close()
	t.mu.Lock()
	for i, o := range t.sessions {
		if o == c {
			t.sessions = append(t.sessions[:i:i], t.sessions[i+1:]...)
			break
		}
	}
	t.mu.Unlock()
	t.vm.DebugPost(func() {
		for id, o := range t.owner {
			if o == c {
				t.vm.RemoveBreakpoint(id)
				delete(t.owner, id)
				delete(t.conditions, id)
			}
		}
		c.gone = true
	})
}

// message is one of a client's requests.
type message struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// session is one client's connection.
type session struct {
	t    *Target
	conn *wsproto.Conn
	// What follows is the runtime goroutine's.
	gone            bool
	runtimeEnabled  bool
	debuggerEnabled bool
	objects         map[string]*remote
	nextObject      int
}

// send writes a message to the client.
func (c *session) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"method": "Inspector.error", "params": map[string]string{"message": err.Error()}})
	}
	c.conn.WriteMessage(wsproto.OpText, b)
}

// event tells the client of something.
func (c *session) event(method string, params any) {
	c.send(map[string]any{"method": method, "params": params})
}

// protocolError is a request refused.
type protocolError struct {
	code int
	msg  string
}

func (e *protocolError) Error() string { return e.msg }

func errorf(format string, args ...any) error {
	return &protocolError{-32000, fmt.Sprintf(format, args...)}
}

// handle does what a client asked and answers it.
func (c *session) handle(m message) {
	if c.gone {
		return
	}
	result, err := c.dispatch(m.Method, m.Params)
	if err != nil {
		code := -32000
		var pe *protocolError
		if errors.As(err, &pe) {
			code = pe.code
		}
		c.send(map[string]any{"id": m.ID, "error": map[string]any{"code": code, "message": err.Error()}})
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	c.send(map[string]any{"id": m.ID, "result": result})
}
