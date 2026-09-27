package stdlib

import (
	"errors"
	"sync"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostjobs"
	"github.com/go-quickjs/go-quickjs/internal/structclone"
)

// Messaging: structuredClone, and the ports of a MessageChannel.
//
// A port's two ends are entangled, and each may be anywhere: in the runtime
// that made the channel, in another runtime a port was posted to, or between
// the two, on its way. What an end is, apart from the script's object for it,
// is a portCore, which the goroutines of both ends share; a message posted
// through one end is serialized, queued on the other end's core, and
// deserialized and dispatched on the goroutine of whichever runtime holds that
// end by then, one message to a task.

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

// portMsg is a message on its way: a serialized value and the ports posted
// with it, or word that the other end has closed.
type portMsg struct {
	data  any
	ports []*portCore
	close bool
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
}

// messaging is a runtime's ports, and the script functions they are made
// and dispatched with.
type messaging struct {
	rt    *quickjs.Runtime
	loop  *Loop
	ports map[int]*portEnd
	next  int

	// What the script provides: a port object for an id, a DOMException, a
	// look at what kind of host object a value is, and the dispatch of a
	// message event.
	makePort, newDOMException, hostKind, deliver quickjs.Value
}

// domToken is a DOMException, serialized.
type domToken struct{ name, message, stack string }

// messagingHost is the functions webAPIsJS reaches messaging through.
func (m *messaging) host() map[string]any {
	return map[string]any{
		"bindMessaging": func(makePort, newDOMException, hostKind, deliver quickjs.Value) {
			m.makePort, m.newDOMException, m.hostKind, m.deliver = makePort, newDOMException, hostKind, deliver
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
			e := m.ports[id]
			if e == nil {
				// A port posted elsewhere, or closed, posts nothing.
				return nil
			}
			data, ports, err := m.serialize(v, transfer, e)
			if err != nil {
				return err
			}
			e.core.mu.Lock()
			peer := e.core.peer
			e.core.mu.Unlock()
			if peer != nil {
				peer.enqueue(portMsg{data: data, ports: ports})
			}
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
	}
}

// adopt makes core an end of this runtime's, and returns its object.
func (m *messaging) adopt(core *portCore) (quickjs.Value, error) {
	m.next++
	e := &portEnd{m: m, id: m.next, core: core, reffed: true}
	obj, err := m.makePort.Call(e.id)
	if err != nil {
		return quickjs.Value{}, err
	}
	e.obj = obj
	m.ports[e.id] = e
	core.mu.Lock()
	core.owner, core.started, core.scheduled = e, false, false
	core.mu.Unlock()
	return obj, nil
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
			switch {
			case e == nil:
				return nil, false, m.cloneError("MessagePort in transfer list is already detached")
			case e == from:
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
		return
	}
	c.queue = append(c.queue, msg)
	owner := c.owner
	post := owner != nil && (c.started || msg.close) && !c.scheduled
	if post {
		c.scheduled = true
	}
	c.mu.Unlock()
	if post {
		owner.schedule()
	}
}

// schedule has the end's runtime dispatch the next message.
func (e *portEnd) schedule() {
	hostjobs.Post(e.m.rt, e.dispatchNext)
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
			if q.close {
				at = i
				break
			}
		}
		if at < 0 {
			c.scheduled = false
			c.mu.Unlock()
			return
		}
		msg, c.queue = c.queue[at], nil
	}
	c.mu.Unlock()

	if msg.close {
		e.closed()
	} else {
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
// first and the other after what this one sent before.
func (e *portEnd) close() {
	c := e.core
	c.mu.Lock()
	peer := c.peer
	c.peer, c.closed, c.queue = nil, true, nil
	c.mu.Unlock()
	e.release()
	hostjobs.Post(e.m.rt, func() { e.m.raise(e.m.deliver.Call(e.obj, "close")) })
	if peer != nil {
		peer.mu.Lock()
		peer.peer = nil
		peer.mu.Unlock()
		peer.enqueue(portMsg{close: true})
	}
}

// closed is an end hearing that the other has closed.
func (e *portEnd) closed() {
	c := e.core
	c.mu.Lock()
	c.closed, c.queue = true, nil
	c.mu.Unlock()
	e.release()
	e.m.raise(e.m.deliver.Call(e.obj, "close"))
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
