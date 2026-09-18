package stdlib

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// WebSockets describes the sockets a runtime may open and accept.
//
// A socket is a connection that stays open, so it is network access of the
// longest-lived kind: the zero value opens nothing, and Allow decides for every
// address a script tries to reach.
type WebSockets struct {
	// Loop is where a message is delivered, and is required: a socket that
	// nothing is listening on is a socket that does nothing.
	Loop *Loop
	// Allow is asked before a socket is opened, with the URL it would open.
	// Nil refuses everything, which is what a zero value means.
	Allow func(target *url.URL) error
	// Serve is the server sockets are accepted on. Without it a script can
	// open a socket but not answer one, since there is nothing listening.
	Serve *Serve
	// MaxMessageBytes bounds one message. Zero means 16 MB.
	MaxMessageBytes int
	// HandshakeTimeout bounds how long opening one may take. Zero means 30s.
	HandshakeTimeout time.Duration
	// TLS is the configuration for a wss connection. Nil uses the defaults.
	TLS *tls.Config
}

// Sockets installs WebSocket, and upgradeWebSocket where there is a server to
// accept one on.
//
//	const socket = new WebSocket("wss://example.com/feed")
//	socket.addEventListener("message", (e) => console.log(e.data))
//	socket.addEventListener("open", () => socket.send("hello"))
//
// On the other side, a handler answers a request by taking it over:
//
//	serve({port: 8080}, (request) => {
//	    const {socket, response} = upgradeWebSocket(request)
//	    socket.onmessage = (e) => socket.send("you said " + e.data)
//	    return response
//	})
//
// What arrives is a message, not a stream of bytes: the protocol's frames, its
// fragments and its pings are handled here, and what the script sees is what
// the other end sent.
func Sockets(rt *quickjs.Runtime, cfg *WebSockets) error {
	if cfg == nil {
		cfg = &WebSockets{}
	}
	if cfg.Loop == nil {
		return errors.New("stdlib: sockets need an event loop to deliver on")
	}
	h := &wsHost{rt: rt, cfg: cfg, pending: map[string]*socket{}}

	host := rt.NewObject()
	if err := setAll(host, map[string]any{
		"connect": h.connect,
		"reserve": h.reserve,
		"serving": cfg.Serve != nil,
	}); err != nil {
		return err
	}
	if cfg.Serve != nil {
		// The server hands a taken-over connection back here, which is the
		// only way a socket arrives from the outside.
		cfg.Serve.upgrade = h.attach
	}

	api, err := evalWithHost(rt, "<websocket>", webSocketJS, host)
	if err != nil {
		return err
	}
	for _, name := range []string{"WebSocket", "upgradeWebSocket"} {
		v, err := api.Get(name)
		if err != nil {
			return err
		}
		if err := rt.Set(name, v); err != nil {
			return err
		}
	}
	return nil
}

type wsHost struct {
	rt  *quickjs.Runtime
	cfg *WebSockets
	// pending holds the sockets a handler has asked for but the connection has
	// not reached yet, by the token the answer carries.
	pending map[string]*socket
	nextID  int64
}

// socket is one WebSocket as the host sees it.
//
// Everything that touches the runtime happens on the loop; the connection is
// read and written on goroutines of its own, and the two meet at Post.
type socket struct {
	rt   *quickjs.Runtime
	loop *Loop
	host *wsHost

	// opened settles when the handshake has finished, one way or the other.
	opened *quickjs.Promise
	conn   *wsConn

	mu     sync.Mutex
	queue  []outgoing
	signal chan struct{}
	closed bool

	// reading is true while a read is outstanding, since only one may be.
	reading bool
	ended   bool
	// released says the hold this socket put on the loop has been given back.
	released bool
}

type outgoing struct {
	op   byte
	data []byte
}

func (h *wsHost) limit() int {
	if h.cfg.MaxMessageBytes > 0 {
		return h.cfg.MaxMessageBytes
	}
	return 16 << 20
}

// connect opens a socket to a URL and hands back the object the script drives
// it through. The dialling happens on another goroutine, so the call returns
// before the connection exists and the script waits on opened.
func (h *wsHost) connect(rawURL string, protocols quickjs.Value) (quickjs.Value, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return quickjs.Value{}, h.rt.Throw(h.rt.NewError("SyntaxError",
			"that is not a URL: "+rawURL))
	}
	switch target.Scheme {
	case "ws", "wss":
	default:
		return quickjs.Value{}, h.rt.Throw(h.rt.NewError("SyntaxError",
			"a socket needs a ws: or wss: URL, not "+target.Scheme+":"))
	}
	if h.cfg.Allow == nil {
		return quickjs.Value{}, h.rt.Throw(h.rt.NewError("Error",
			"opening a socket is not allowed"))
	}
	if err := h.cfg.Allow(target); err != nil {
		return quickjs.Value{}, h.rt.Throw(h.rt.NewError("Error", err.Error()))
	}

	s := h.newSocket()
	names := stringsOf(protocols)
	timeout := h.cfg.HandshakeTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	tlsCfg := h.cfg.TLS
	limit := h.limit()

	loop := h.cfg.Loop
	loop.Begin()
	go func() {
		defer loop.Done()
		conn, protocol, err := dialWebSocket(target, names, timeout, tlsCfg, limit)
		loop.Post(func() {
			if err != nil {
				s.opened.RejectError(err)
				s.finish()
				return
			}
			s.start(conn)
			o := s.rt.NewObject()
			o.Set("protocol", protocol)
			s.opened.Resolve(o)
		})
	}()
	return s.value()
}

// reserve makes a socket that has no connection yet, for a handler that means
// to take one over. The token comes back with the answer, which is how the
// connection finds its way to this socket.
func (h *wsHost) reserve() (quickjs.Value, error) {
	h.nextID++
	token := "ws-" + strconv.FormatInt(h.nextID, 10)
	s := h.newSocket()
	h.pending[token] = s

	v, err := s.value()
	if err != nil {
		return quickjs.Value{}, err
	}
	out := h.rt.NewObject()
	if err := errors.Join(out.Set("token", token), out.Set("handle", v)); err != nil {
		return quickjs.Value{}, err
	}
	return out, nil
}

// attach is what the server calls once it has taken a connection over. It runs
// on the loop, like everything else that touches a socket's runtime side.
func (h *wsHost) attach(token string, conn net.Conn, br *bufio.Reader, protocol string) {
	s := h.pending[token]
	delete(h.pending, token)
	if s == nil {
		conn.Close()
		return
	}
	s.start(newWSConn(conn, br, false, h.limit()))
	o := s.rt.NewObject()
	o.Set("protocol", protocol)
	s.opened.Resolve(o)
}

func (h *wsHost) newSocket() *socket {
	return &socket{
		rt:     h.rt,
		loop:   h.cfg.Loop,
		host:   h,
		opened: h.rt.NewPromise(),
		signal: make(chan struct{}, 1),
	}
}

// value is the object the script holds: the handshake, the next message, and
// the two ways to end it.
func (s *socket) value() (quickjs.Value, error) {
	o := s.rt.NewObject()
	if err := errors.Join(
		o.Set("opened", s.opened.Value()),
		o.Set("receive", s.receive),
		o.Set("send", s.send),
		o.Set("close", s.closeWith),
	); err != nil {
		return quickjs.Value{}, err
	}
	return o, nil
}

// start takes ownership of an open connection and begins writing whatever the
// script has queued.
func (s *socket) start(conn *wsConn) {
	s.conn = conn
	// The loop is held open while the socket is: a program whose last act is
	// to open one is waiting for what comes back.
	s.loop.Begin()
	go s.writing()
}

// writing is the one goroutine that writes to the connection, so that messages
// leave in the order the script sent them.
func (s *socket) writing() {
	for {
		s.mu.Lock()
		queue := s.queue
		s.queue = nil
		closed := s.closed
		s.mu.Unlock()

		for _, m := range queue {
			if err := s.conn.writeMessage(m.op, m.data); err != nil {
				s.conn.close()
				return
			}
		}
		if closed {
			return
		}
		<-s.signal
	}
}

// send queues a message. The script's bytes are copied, since the writing
// happens elsewhere and what it holds is its own.
func (s *socket) send(data quickjs.Value) error {
	if s.conn == nil {
		return s.rt.Throw(s.rt.NewError("Error", "this socket is not open yet"))
	}
	op := byte(opText)
	var payload []byte
	if b, ok := data.Bytes(); ok {
		op = opBinary
		payload = append([]byte(nil), b...)
	} else {
		payload = []byte(data.String())
	}
	s.enqueue(outgoing{op: op, data: payload})
	return nil
}

func (s *socket) enqueue(m outgoing) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, m)
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// receive answers with the next message, or null when the socket has ended.
func (s *socket) receive() *quickjs.Promise {
	p := s.rt.NewPromise()
	if s.ended || s.conn == nil {
		p.Resolve(nil)
		return p
	}
	if s.reading {
		p.RejectError(errors.New("this socket is already being read"))
		return p
	}
	s.reading = true

	loop, conn := s.loop, s.conn
	loop.Begin()
	go func() {
		defer loop.Done()
		op, payload, err := conn.readMessage()
		loop.Post(func() {
			s.reading = false
			if err != nil {
				s.finish()
				// A connection that ended without a close frame ended badly,
				// and the script is told which.
				p.RejectError(err)
				return
			}
			out := s.rt.NewObject()
			switch op {
			case opClose:
				code, reason := closeInfo(payload)
				s.finish()
				out.Set("type", "close")
				out.Set("code", code)
				out.Set("reason", reason)
			case opBinary:
				out.Set("type", "binary")
				out.Set("data", s.rt.NewBytes(payload))
			default:
				out.Set("type", "text")
				out.Set("data", string(payload))
			}
			p.Resolve(out)
		})
	}()
	return p
}

// closeWith says goodbye and stops.
func (s *socket) closeWith(code quickjs.Value, reason quickjs.Value) {
	if s.conn == nil {
		s.finish()
		return
	}
	n := 1000
	if code.Kind() == quickjs.KindNumber {
		n = code.Int()
	}
	text := ""
	if reason.Kind() == quickjs.KindString {
		text = reason.String()
	}
	conn := s.conn
	s.mu.Lock()
	already := s.closed
	s.mu.Unlock()
	if already {
		return
	}
	// The goodbye goes through the queue so that it follows whatever was sent
	// before it rather than overtaking it.
	payload := make([]byte, 0, 2+len(text))
	payload = append(payload, byte(n>>8), byte(n))
	payload = append(payload, text...)
	s.enqueue(outgoing{op: opClose, data: payload})

	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
	// The connection is dropped shortly after, whatever the other end does
	// with the goodbye.
	loop := s.loop
	loop.Begin()
	go func() {
		defer loop.Done()
		time.Sleep(50 * time.Millisecond)
		conn.close()
	}()
	s.releaseLoop()
	s.ended = true
}

// finish marks the socket over and lets the loop go.
func (s *socket) finish() {
	if s.ended {
		return
	}
	s.ended = true
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
	if s.conn != nil {
		s.conn.close()
	}
	s.releaseLoop()
}

// releaseLoop undoes the Begin that start made, once.
func (s *socket) releaseLoop() {
	if s.conn == nil || s.released {
		return
	}
	s.released = true
	s.loop.Done()
}

// dialWebSocket opens the connection and does the handshake. It touches no
// JavaScript value, which is what lets it run on another goroutine.
func dialWebSocket(target *url.URL, protocols []string, timeout time.Duration, tlsCfg *tls.Config, limit int) (*wsConn, string, error) {
	host := target.Host
	if target.Port() == "" {
		if target.Scheme == "wss" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}

	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if target.Scheme == "wss" {
		conn, err = tls.DialWithDialer(dialer, "tcp", host, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, "", err
	}
	conn.SetDeadline(time.Now().Add(timeout))

	key, err := wsKey()
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	path := target.RequestURI()
	if path == "" {
		path = "/"
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", target.Host)
	req.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	if len(protocols) > 0 {
		fmt.Fprintf(&req, "Sec-WebSocket-Protocol: %s\r\n", strings.Join(protocols, ", "))
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, "", err
	}

	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, "", fmt.Errorf("the server answered %s rather than opening a socket", res.Status)
	}
	if !strings.EqualFold(res.Header.Get("Upgrade"), "websocket") ||
		res.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		conn.Close()
		return nil, "", errors.New("the server's answer is not a websocket handshake")
	}
	// The deadline was for the handshake; what follows has no time limit of
	// its own, since a socket is meant to stay open.
	conn.SetDeadline(time.Time{})
	return newWSConn(conn, br, true, limit), res.Header.Get("Sec-WebSocket-Protocol"), nil
}

// webSocketJS is the interface a program written for a browser expects.
const webSocketJS = `(function (host) {
  "use strict";

  const CONNECTING = 0, OPEN = 1, CLOSING = 2, CLOSED = 3;

  class WebSocket extends EventTarget {
    constructor(url, protocols) {
      super();
      this.url = url === null ? "" : String(url);
      this.readyState = CONNECTING;
      this.protocol = "";
      this.extensions = "";
      this.bufferedAmount = 0;
      // The web defaults to a Blob, which is a copy of a copy for the common
      // case of reading the bytes; this hands over the bytes.
      this.binaryType = "arraybuffer";
      this.onopen = null;
      this.onmessage = null;
      this.onerror = null;
      this.onclose = null;
      if (url !== null) {
        this._drive(host.connect(this.url, protocols === undefined ? []
          : (Array.isArray(protocols) ? protocols : [String(protocols)])));
      }
    }

    // _drive is the whole life of the socket: the handshake, then every
    // message until it ends, then the one close event.
    async _drive(handle) {
      this._handle = handle;
      let ending = null;
      try {
        const opened = await handle.opened;
        this.protocol = (opened && opened.protocol) || "";
        this.readyState = OPEN;
        this._fire("open", {});
        for (;;) {
          const message = await handle.receive();
          if (message === null || message === undefined) break;
          if (message.type === "close") {
            ending = {code: message.code, reason: message.reason, clean: true};
            break;
          }
          this._fire("message", {
            data: message.type === "text" ? message.data : this._binary(message.data),
            origin: this.url,
          });
        }
      } catch (e) {
        this._fire("error", {error: e, message: String((e && e.message) || e)});
        ending = {code: 1006, reason: String((e && e.message) || e), clean: false};
      }
      this.readyState = CLOSED;
      // A socket that was closed from this end reports what this end said;
      // 1005 means neither end said anything at all.
      const end = ending || this._closing || {code: 1005, reason: "", clean: true};
      this._fire("close", {code: end.code, reason: end.reason, wasClean: end.clean});
    }

    _binary(bytes) {
      if (this.binaryType === "blob" && typeof Blob !== "undefined") {
        return new Blob([bytes]);
      }
      return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
    }

    // dispatchEvent calls the onwhatever property itself, so this does not.
    _fire(type, props) {
      const event = new Event(type);
      Object.assign(event, props);
      this.dispatchEvent(event);
    }

    send(data) {
      if (this.readyState === CONNECTING) {
        throw new DOMException_("the socket is not open yet");
      }
      if (this.readyState !== OPEN) return;
      this._handle.send(data);
    }

    close(code, reason) {
      if (this.readyState === CLOSED || this.readyState === CLOSING) return;
      this.readyState = CLOSING;
      this._closing = {
        code: code === undefined ? 1000 : Number(code),
        reason: reason === undefined ? "" : String(reason),
        clean: true,
      };
      this._handle.close(this._closing.code, this._closing.reason);
    }
  }

  // The browsers throw an InvalidStateError here; this runtime has no DOM
  // exceptions, so it throws the nearest thing it has.
  function DOMException_(message) {
    const e = new Error(message);
    e.name = "InvalidStateError";
    return e;
  }

  WebSocket.CONNECTING = CONNECTING;
  WebSocket.OPEN = OPEN;
  WebSocket.CLOSING = CLOSING;
  WebSocket.CLOSED = CLOSED;
  for (const [name, value] of Object.entries(
      {CONNECTING, OPEN, CLOSING, CLOSED})) {
    Object.defineProperty(WebSocket.prototype, name, {value, writable: false});
  }

  // upgradeWebSocket takes a request over: the answer tells the server to stop
  // treating the connection as a request and hand it to the socket.
  function upgradeWebSocket(request, options = {}) {
    if (!host.serving) {
      throw new TypeError("this runtime is not serving, so there is nothing to upgrade");
    }
    const headers = request && request.headers;
    const wanted = headers && headers.get("upgrade");
    if (!wanted || String(wanted).toLowerCase() !== "websocket") {
      throw new TypeError("this request did not ask for a socket");
    }
    const reserved = host.reserve();
    const socket = new WebSocket(null);
    socket._drive(reserved.handle);

    const response = new Response(null, {status: 101});
    // The token is what the connection is handed over with; it is on the
    // response because the response is what the handler gives back.
    Object.defineProperty(response, "upgrade", {value: reserved.token});
    if (options.protocol) {
      Object.defineProperty(response, "protocol", {value: String(options.protocol)});
    }
    return {socket, response};
  }

  return {WebSocket, upgradeWebSocket};
})`
