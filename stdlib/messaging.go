package stdlib

import (
	"context"
	"errors"
	"sync"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/structclone"
)

// Messaging: structuredClone, the ports of a MessageChannel, and
// BroadcastChannel.
//
// A port's two ends are entangled, and each may be anywhere: in the runtime
// that made the channel, in another runtime a port was posted to, or between
// the two, on its way. What an end is, apart from the script's object for it,
// is a portCore, which the goroutines of both ends share; a message posted
// through one end is serialized, queued on the other end's core, and
// deserialized and dispatched on the goroutine of whichever runtime holds that
// end by then, one message to a task.
//
// A worker talks to the runtime that started it through such a channel, and
// its word that it is running, that it failed, and that it has ended travels
// the same way, so that it arrives after what the worker posted before.

// portCore is one end of a channel.
type portCore struct {
	mu sync.Mutex
	// peer is the end this one is entangled with, until either is closed.
	peer *portCore
	// owner is the runtime's end holding this one, and nil while the port is
	// being posted somewhere.
	owner *portEnd
	// queue is what has arrived and not been dispatched.
	queue []portMsg
	// started records that the owner dispatches messages, which a port does
	// once it has been started; scheduled, that a task to dispatch the next
	// is on its way to the owner.
	started, scheduled bool
	closed             bool
}

// msgKind is what a portMsg says.
type msgKind uint8

const (
	// msgData is a message posted.
	msgData msgKind = iota
	// msgClose is word that the other end has closed -- for a worker's
	// port, that the worker has ended, with its exit code.
	msgClose
	// msgOnline and msgError are a worker's word that it is running, and
	// that it failed, with the error.
	msgOnline
	msgError
)

// portMsg is a message on its way: a serialized value and the ports posted
// with it, or word from the other end.
type portMsg struct {
	kind  msgKind
	data  any
	ports []*portCore
	code  int
}

// portEnd is a runtime's hold on a port: its id, and the script's object.
type portEnd struct {
	m    *messaging
	id   int
	core *portCore
	obj  quickjs.Value
	// reffed is whether a started port keeps the loop running, which it does
	// until it is unref'd; holding, whether it is doing so.
	reffed, holding bool
	// worker is the worker this end is the starting runtime's port to, which
	// hears the worker's word.
	worker *workerHandle
	// channel is a BroadcastChannel's name, for an end that is one.
	channel   string
	broadcast bool
	// unwatch stops a BroadcastChannel's watch on its runtime's end.
	unwatch func() bool
	// work is how what arrives from other goroutines reaches the end's
	// runtime. It does not keep the runtime busy: hold does that for a port
	// that is listening, as node's does.
	work *quickjs.AsyncWork
}

// messaging is a runtime's ports, and the script functions they are made
// and dispatched with.
type messaging struct {
	rt    *quickjs.Runtime
	loop  *Loop
	ports map[int]*portEnd
	next  int
	// halted ends when a worker is terminated, from when what it posts goes
	// nowhere: it would have been stopped by then, were it not between
	// checks for that. It is nil but in a worker.
	halted context.Context

	// What the script provides: a port object for an id, a DOMException, a
	// look at what kind of host object a value is, and the dispatch of a
	// message event; and receiveMessageOnPort, for worker_threads.
	makePort, newDOMException, hostKind, deliver, receive, makeBlob quickjs.Value
}

// domToken is a DOMException, serialized.
type domToken struct{ name, message, stack string }

// blobToken is a Blob, or a File, serialized: its bytes are copied, as a
// Blob's never change.
type blobToken struct {
	bytes        []byte
	typ          string
	file         bool
	name         string
	lastModified float64
}

// messagingHost is the functions webAPIsJS reaches messaging through.
func (m *messaging) host() map[string]any {
	return map[string]any{
		"bindMessaging": func(makePort, newDOMException, hostKind, deliver, receive, makeBlob quickjs.Value) {
			m.makePort, m.newDOMException, m.hostKind, m.deliver = makePort, newDOMException, hostKind, deliver
			m.receive, m.makeBlob = receive, makeBlob
		},
		"clone": func(v quickjs.Value, transfer []quickjs.Value) (quickjs.Value, error) {
			data, ports, err := m.serialize(v, transfer, nil)
			if err != nil {
				return quickjs.Value{}, err
			}
			out, _, err := m.deserialize(data, ports)
			return out, err
		},
		"channel": func() (quickjs.Value, error) {
			a, b := &portCore{}, &portCore{}
			a.peer, b.peer = b, a
			pa, err := m.adopt(a)
			if err != nil {
				return quickjs.Value{}, err
			}
			pb, err := m.adopt(b)
			if err != nil {
				return quickjs.Value{}, err
			}
			return m.rt.NewArray(pa, pb)
		},
		"portPost": func(id int, v quickjs.Value, transfer []quickjs.Value) error {
			if m.isHalted() {
				return nil
			}
			e := m.ports[id]
			if e == nil {
				// A port posted elsewhere, or closed, posts nothing -- but the
				// message is serialized all the same, as the standard has it:
				// what cannot be cloned throws, and what is transferred is
				// detached, and closed if it is a port.
				data, ports, err := m.serialize(v, transfer, nil)
				if err != nil {
					return err
				}
				dropMessages(portMsg{data: data, ports: ports})
				return nil
			}
			data, ports, err := m.serialize(v, transfer, e)
			if err != nil {
				return err
			}
			e.core.mu.Lock()
			peer := e.core.peer
			e.core.mu.Unlock()
			msg := portMsg{data: data, ports: ports}
			if peer == nil {
				// Posted to a port whose other end has closed, the message
				// goes nowhere, and the ports it carries are closed.
				dropMessages(msg)
				return nil
			}
			peer.enqueue(msg)
			return nil
		},
		"portStart": func(id int) {
			if e := m.ports[id]; e != nil {
				e.start()
			}
		},
		"portClose": func(id int) {
			if e := m.ports[id]; e != nil {
				e.close()
			}
		},
		"portRef": func(id int, ref bool) {
			if e := m.ports[id]; e != nil {
				e.reffed = ref
				e.hold()
			}
		},
		"portHasRef": func(id int) bool {
			e := m.ports[id]
			return e != nil && e.holding
		},
		// portReceive takes the next message waiting on a port, as
		// receiveMessageOnPort does, in a list; or null for none.
		"portReceive": func(id int) (any, error) {
			e := m.ports[id]
			if e == nil {
				return nil, nil
			}
			c := e.core
			c.mu.Lock()
			if len(c.queue) == 0 || c.queue[0].kind != msgData {
				c.mu.Unlock()
				return nil, nil
			}
			msg := c.queue[0]
			c.queue = c.queue[1:]
			c.mu.Unlock()
			v, _, err := m.deserialize(msg.data, msg.ports)
			if err != nil {
				return nil, err
			}
			return m.rt.NewArray(v)
		},
		"broadcastOpen": func(name string, obj quickjs.Value) int {
			return m.openBroadcast(name, obj)
		},
		"broadcastPost": func(id int, v quickjs.Value) error {
			e := m.ports[id]
			if e == nil || !e.broadcast {
				return m.rt.Throw(m.newDOMExceptionOr("BroadcastChannel is closed.", "InvalidStateError"))
			}
			if m.isHalted() {
				return nil
			}
			return e.broadcastMessage(v)
		},
	}
}

// isHalted is whether the worker these are the ports of has been
// terminated.
func (m *messaging) isHalted() bool {
	return m.halted != nil && m.halted.Err() != nil
}

// newDOMExceptionOr is a DOMException, or an Error if one cannot be made.
func (m *messaging) newDOMExceptionOr(message, name string) quickjs.Value {
	if e, err := m.newDOMException.Call(message, name); err == nil {
		return e
	}
	return m.rt.NewError("Error", message)
}

// adopt makes core an end of this runtime's, and returns its object.
func (m *messaging) adopt(core *portCore) (quickjs.Value, error) {
	e, err := m.adoptEnd(core)
	if err != nil {
		return quickjs.Value{}, err
	}
	return e.obj, nil
}

// adoptEnd is adopt, returning the end.
func (m *messaging) adoptEnd(core *portCore) (*portEnd, error) {
	m.next++
	e := &portEnd{m: m, id: m.next, core: core, reffed: true, work: m.startWork()}
	obj, err := m.makePort.Call(e.id)
	if err != nil {
		return nil, err
	}
	e.obj = obj
	m.ports[e.id] = e
	core.mu.Lock()
	core.owner, core.started, core.scheduled = e, false, false
	core.mu.Unlock()
	return e, nil
}

// serialize serializes v, transferring what transfer lists, for from --
// the port posting it, or nil for structuredClone. The ports transferred are
// this runtime's no longer.
func (m *messaging) serialize(v quickjs.Value, transfer []quickjs.Value, from *portEnd) (any, []*portCore, error) {
	var moved []*portEnd
	codec := &structclone.Codec{
		Serialize: func(x any) (any, bool, error) {
			kind, err := m.hostKind.Call(x)
			if err != nil || !kind.IsArray() {
				return nil, false, err
			}
			tag, _ := kind.Index(0)
			switch tag.String() {
			case "port":
				return nil, false, m.cloneError("Object that needs transfer was found in message but not listed in transferList")
			case "dom":
				name, _ := kind.Index(1)
				message, _ := kind.Index(2)
				stack, _ := kind.Index(3)
				return domToken{name.String(), message.String(), stack.String()}, true, nil
			case "blob":
				field := func(i int) quickjs.Value { f, _ := kind.Index(i); return f }
				b, _ := field(1).Bytes()
				return blobToken{
					bytes: append([]byte(nil), b...), typ: field(2).String(), file: field(3).Bool(),
					name: field(4).String(), lastModified: field(5).Float(),
				}, true, nil
			}
			return nil, false, nil
		},
		Transfer: func(x any) (any, bool, error) {
			kind, err := m.hostKind.Call(x)
			if err != nil || !kind.IsArray() {
				return nil, false, err
			}
			if tag, _ := kind.Index(0); tag.String() != "port" {
				return nil, false, nil
			}
			id, _ := kind.Index(1)
			e := m.ports[id.Int()]
			switch e {
			case nil:
				return nil, false, m.cloneError("MessagePort in transfer list is already detached")
			case from:
				return nil, false, m.cloneError("Transfer list contains source port")
			}
			for _, p := range moved {
				if p == e {
					return nil, false, m.cloneError("Transfer list contains duplicate MessagePort")
				}
			}
			moved = append(moved, e)
			return e.core, true, nil
		},
	}
	list := make([]any, len(transfer))
	for i, t := range transfer {
		list[i] = t
	}
	data, err := structclone.Serialize(m.rt, v, list, codec)
	if err != nil {
		return nil, nil, m.cloneError(err)
	}
	// Only a clone that succeeded takes the ports: a failure leaves them.
	ports := make([]*portCore, len(moved))
	for i, e := range moved {
		e.detach()
		ports[i] = e.core
	}
	return data, ports, nil
}

// deserialize makes here the value serialize made, and the objects of the
// ports that came with it, in the order they were listed.
func (m *messaging) deserialize(data any, ports []*portCore) (quickjs.Value, []quickjs.Value, error) {
	made := map[*portCore]quickjs.Value{}
	codec := &structclone.Codec{
		Revive: func(token any) (any, error) {
			switch t := token.(type) {
			case *portCore:
				obj, err := m.adopt(t)
				made[t] = obj
				return obj, err
			case domToken:
				return m.newDOMException.Call(t.message, t.name, t.stack)
			case blobToken:
				return m.makeBlob.Call(m.rt.NewBytes(t.bytes), t.typ, t.file, t.name, t.lastModified)
			}
			return nil, errors.New("stdlib: an object of an unknown kind was cloned")
		},
	}
	v, err := structclone.Deserialize(m.rt, data, codec)
	if err != nil {
		return quickjs.Value{}, nil, m.cloneError(err)
	}
	objs := make([]quickjs.Value, len(ports))
	for i, p := range ports {
		obj, ok := made[p]
		if !ok {
			// A port posted without appearing in the message is still the
			// receiver's, in the event's ports.
			if obj, err = m.adopt(p); err != nil {
				return quickjs.Value{}, nil, err
			}
		}
		objs[i] = obj
	}
	return v.(quickjs.Value), objs, nil
}

// cloneError is the exception an error from cloning becomes: a DataCloneError
// is a DOMException.
func (m *messaging) cloneError(err any) error {
	var msg string
	switch e := err.(type) {
	case string:
		msg = e
	case error:
		var dce *structclone.DataCloneError
		if !errors.As(e, &dce) {
			return e
		}
		msg = dce.Message
	}
	exc, err2 := m.newDOMException.Call(msg, "DataCloneError")
	if err2 != nil {
		return err2
	}
	return m.rt.Throw(exc)
}

// --- Ends ----------------------------------------------------------------------

// enqueue queues a message on a port, and has its owner told if it is
// listening -- or, for a close, whether it is listening or not.
func (c *portCore) enqueue(msg portMsg) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		dropMessages(msg)
		return
	}
	c.queue = append(c.queue, msg)
	owner := c.owner
	post := owner != nil && (c.started || msg.kind == msgClose) && !c.scheduled
	if post {
		c.scheduled = true
	}
	c.mu.Unlock()
	if post {
		owner.schedule()
	}
}

// schedule has the end's runtime dispatch the next message, from any
// goroutine. Once the end is no longer the runtime's, or the runtime has
// closed, there is nothing to dispatch to.
func (e *portEnd) schedule() {
	e.work.Post(func(err error) {
		if err == nil {
			e.dispatchNext()
		}
	})
}

// startWork is what one of the runtime's ends hears through: work that does
// not of itself keep the runtime busy.
func (m *messaging) startWork() *quickjs.AsyncWork {
	w := m.rt.StartAsyncWork()
	w.Unref()
	return w
}

// dispatchNext dispatches the message at the head of the queue, on the
// owner's goroutine, and asks for another task for the next.
func (e *portEnd) dispatchNext() {
	c := e.core
	c.mu.Lock()
	if c.owner != e {
		// The port was posted elsewhere after the task was asked for; its new
		// owner has asked for its own.
		c.mu.Unlock()
		return
	}
	var msg portMsg
	var dropped []portMsg
	switch {
	case len(c.queue) == 0 || c.closed:
		c.scheduled = false
		c.mu.Unlock()
		return
	case c.started:
		msg, c.queue = c.queue[0], c.queue[1:]
	default:
		// A port that was never started hears only that it has closed, and
		// what was sent it before is dropped.
		at := -1
		for i, q := range c.queue {
			if q.kind == msgClose {
				at = i
				break
			}
		}
		if at < 0 {
			c.scheduled = false
			c.mu.Unlock()
			return
		}
		dropped = c.queue[:at]
		msg, c.queue = c.queue[at], nil
	}
	c.mu.Unlock()
	dropMessages(dropped...)

	switch msg.kind {
	case msgClose:
		e.closed(msg.code)
	case msgOnline:
		e.worker.online()
	case msgError:
		e.worker.failed(msg.data)
	default:
		e.dispatch(msg)
	}

	c.mu.Lock()
	again := c.owner == e && !c.closed && len(c.queue) > 0 && c.started
	if c.owner == e && !again {
		c.scheduled = false
	}
	c.mu.Unlock()
	if again {
		e.schedule()
	}
}

// dispatch deserializes a message and dispatches it to the port's listeners;
// one that cannot be deserialized is a messageerror.
func (e *portEnd) dispatch(msg portMsg) {
	m := e.m
	v, ports, err := m.deserialize(msg.data, msg.ports)
	if err != nil {
		m.raise(m.deliver.Call(e.obj, "messageerror"))
		return
	}
	items := make([]any, len(ports))
	for i, p := range ports {
		items[i] = p
	}
	list, err := m.rt.NewArray(items...)
	if err != nil {
		return
	}
	m.raise(m.deliver.Call(e.obj, "message", v, list))
}

// raise makes an exception a listener threw uncaught, as one a timer throws
// is: Run returns it. Without a loop there is no one to tell.
func (m *messaging) raise(_ quickjs.Value, err error) {
	if err != nil && m.loop != nil {
		m.loop.fail(err)
	}
}

// start begins dispatching what arrives.
func (e *portEnd) start() {
	c := e.core
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	post := len(c.queue) > 0 && !c.scheduled
	if post {
		c.scheduled = true
	}
	c.mu.Unlock()
	e.hold()
	if post {
		e.schedule()
	}
}

// close disentangles the port. Both ends hear it, as node's do, this one
// first and the other after what this one sent before. A BroadcastChannel
// stops hearing its name.
func (e *portEnd) close() {
	if e.broadcast {
		e.closeBroadcast()
		return
	}
	e.release()
	// The close event is a task of its own, which the program waits for.
	e.m.rt.StartAsyncWork().Complete(func(err error) {
		if err == nil {
			e.m.raise(e.m.deliver.Call(e.obj, "close"))
		}
	})
	e.core.disentangle(0)
}

// disentangle closes a port's core, and tells the other end, after what this
// one sent before: with the exit code, when this is a worker's end.
func (c *portCore) disentangle(code int) {
	c.mu.Lock()
	peer, dropped := c.peer, c.queue
	c.peer, c.closed, c.queue = nil, true, nil
	c.mu.Unlock()
	dropMessages(dropped...)
	if peer != nil {
		peer.mu.Lock()
		peer.peer = nil
		peer.mu.Unlock()
		peer.enqueue(portMsg{kind: msgClose, code: code})
	}
}

// send queues word on the other end of a port, if it has one.
func (c *portCore) send(msg portMsg) {
	c.mu.Lock()
	peer := c.peer
	c.mu.Unlock()
	if peer != nil {
		peer.enqueue(msg)
	}
}

// closed is an end hearing that the other has closed -- or, for the port to
// a worker, that the worker has ended.
func (e *portEnd) closed(code int) {
	c := e.core
	c.mu.Lock()
	dropped := c.queue
	c.closed, c.queue = true, nil
	c.mu.Unlock()
	dropMessages(dropped...)
	e.release()
	if e.worker != nil {
		e.worker.exited(code)
		return
	}
	e.m.raise(e.m.deliver.Call(e.obj, "close"))
}

// shutdown closes every port of a runtime that is ending but keep, as a
// worker's are when it has ended: the other ends hear it, and nothing is held
// open for a runtime that is gone.
func (m *messaging) shutdown(keep *portCore) {
	for _, e := range m.ports {
		switch {
		case e.core == keep:
		case e.broadcast:
			e.closeBroadcast()
		default:
			e.release()
			e.core.disentangle(0)
		}
	}
}

// --- BroadcastChannel ----------------------------------------------------------

// broadcasts is every BroadcastChannel open in the process, by name: those of
// every runtime, which is what lets a worker hear its parent's.
var broadcasts = struct {
	sync.Mutex
	byName map[string]map[*portCore]bool
}{byName: map[string]map[*portCore]bool{}}

// openBroadcast makes obj a BroadcastChannel of a name, returning its id. It
// dispatches what it hears at once, and, as in node, holds the loop until it
// is closed or unref'd.
func (m *messaging) openBroadcast(name string, obj quickjs.Value) int {
	m.next++
	core := &portCore{started: true}
	e := &portEnd{m: m, id: m.next, core: core, obj: obj, reffed: true, channel: name, broadcast: true, work: m.startWork()}
	core.owner = e
	m.ports[e.id] = e
	broadcasts.Lock()
	if broadcasts.byName[name] == nil {
		broadcasts.byName[name] = map[*portCore]bool{}
	}
	broadcasts.byName[name][core] = true
	broadcasts.Unlock()
	// A channel the script never closes stops hearing its name when its
	// runtime is closed: until then it kept the runtime reachable, and what
	// was broadcast to it queued up without end.
	e.unwatch = context.AfterFunc(m.rt.Context(), func() { unregisterBroadcast(name, core) })
	e.hold()
	return e.id
}

// broadcastMessage posts a clone of v to every other channel of the name.
func (e *portEnd) broadcastMessage(v quickjs.Value) error {
	broadcasts.Lock()
	var to []*portCore
	for c := range broadcasts.byName[e.channel] {
		if c != e.core {
			to = append(to, c)
		}
	}
	broadcasts.Unlock()
	// Serialized once, as the standard has it -- a getter in it runs once,
	// however many hear it -- and copied for each: each deserializes its own.
	// What nothing hears is serialized all the same, so what cannot be cloned
	// is still refused.
	data, _, err := e.m.serialize(v, nil, e)
	if err != nil || len(to) == 0 {
		return err
	}
	// Every copy is made before any is sent: one sent may be deserialized,
	// and its bytes moved, while the next is being copied.
	copies := make([]any, len(to))
	copies[0] = data
	for i := 1; i < len(to); i++ {
		copies[i] = structclone.Copy(data)
	}
	for i, c := range to {
		c.enqueue(portMsg{data: copies[i]})
	}
	return nil
}

// closeBroadcast stops a BroadcastChannel hearing its name.
func (e *portEnd) closeBroadcast() {
	e.unwatch()
	unregisterBroadcast(e.channel, e.core)
	e.release()
}

// unregisterBroadcast takes a BroadcastChannel out of those of its name and
// drops what it has not dispatched. It is safe to call from any goroutine.
func unregisterBroadcast(name string, core *portCore) {
	broadcasts.Lock()
	if set := broadcasts.byName[name]; set != nil {
		delete(set, core)
		if len(set) == 0 {
			delete(broadcasts.byName, name)
		}
	}
	broadcasts.Unlock()
	core.mu.Lock()
	dropped := core.queue
	core.closed, core.queue = true, nil
	core.mu.Unlock()
	dropMessages(dropped...)
}

// dropMessages closes the ports inside messages that will never be
// delivered, as node does: their other ends hear that they have closed,
// rather than holding their loops open waiting on ports no runtime has.
func dropMessages(msgs ...portMsg) {
	for _, msg := range msgs {
		for _, c := range msg.ports {
			c.disentangle(0)
		}
	}
}

// detach lets go of a port being posted: its messages wait in its queue for
// whichever runtime it arrives in.
func (e *portEnd) detach() {
	c := e.core
	c.mu.Lock()
	c.owner, c.started, c.scheduled = nil, false, false
	c.mu.Unlock()
	e.release()
}

// release is the end being this runtime's no longer.
func (e *portEnd) release() {
	e.work.Done()
	delete(e.m.ports, e.id)
	e.reffed = false
	e.hold()
}

// hold keeps the loop running while the port is started and ref'd, as a
// listening port keeps node's.
func (e *portEnd) hold() {
	e.core.mu.Lock()
	want := e.reffed && e.core.started && e.core.owner == e && !e.core.closed
	e.core.mu.Unlock()
	if want == e.holding || e.m.loop == nil {
		return
	}
	e.holding = want
	if want {
		e.m.loop.Begin()
	} else {
		e.m.loop.Done()
	}
}
