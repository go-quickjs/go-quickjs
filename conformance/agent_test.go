package conformance_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostjobs"
	"github.com/go-quickjs/go-quickjs/internal/sharedmem"
)

// $262.agent: concurrent agents, each a runtime of its own on a goroutine of
// its own, which the test's main agent shares memory with by broadcasting a
// SharedArrayBuffer, and which report back as strings.

// agentPool is the agents one test started.
type agentPool struct {
	newRuntime func() *quickjs.Runtime
	ctx        context.Context
	cancel     context.CancelFunc
	start      time.Time

	mu      sync.Mutex
	agents  []*agent
	wg      sync.WaitGroup
	reports chan string
}

// agent is one running agent.
type agent struct {
	inbox chan broadcastMsg
	// taken is signalled when the agent has retrieved a broadcast.
	taken chan struct{}
	// done is closed when the agent stops.
	done chan struct{}
}

// broadcastMsg is a broadcast: shared memory and an Int32 or BigInt, written
// as a literal the agent can evaluate.
type broadcastMsg struct {
	mem any
	id  string
}

func newAgentPool(newRuntime func() *quickjs.Runtime) *agentPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &agentPool{
		newRuntime: newRuntime, ctx: ctx, cancel: cancel, start: time.Now(),
		reports: make(chan string, 1024),
	}
}

// stop ends every agent and waits for them to be done.
func (p *agentPool) stop() {
	p.cancel()
	p.wg.Wait()
}

// now is monotonicNow: milliseconds since the pool was made.
func (p *agentPool) now() float64 {
	return float64(time.Since(p.start).Microseconds()) / 1000
}

// sleep blocks for ms milliseconds, or until the test ends.
func (p *agentPool) sleep(ms float64) {
	select {
	case <-time.After(time.Duration(ms * float64(time.Millisecond))):
	case <-p.ctx.Done():
	}
}

// install gives the main agent's runtime the functions its $262.agent is
// made of.
func (p *agentPool) install(rt *quickjs.Runtime) error {
	for name, fn := range map[string]any{
		"agentStart": func(src string) error { return p.startAgent(src) },
		"agentBroadcast": func(sab, id quickjs.Value) error {
			return p.broadcast(sab, id)
		},
		"agentGetReport": func() any {
			select {
			case r := <-p.reports:
				return r
			default:
				return nil
			}
		},
		"agentSleep":        p.sleep,
		"agentMonotonicNow": p.now,
	} {
		if err := rt.Set(name, fn); err != nil {
			return err
		}
	}
	return nil
}

// agentMain is what a main agent's $262 has as agent.
const agentMain = `{
	start: agentStart,
	broadcast: agentBroadcast,
	getReport: agentGetReport,
	sleep: agentSleep,
	monotonicNow: agentMonotonicNow,
}`

// startAgent runs src in a new agent, returning once it has run.
func (p *agentPool) startAgent(src string) error {
	a := &agent{inbox: make(chan broadcastMsg), taken: make(chan struct{}), done: make(chan struct{})}
	started := make(chan error, 1)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(a.done)
		p.run(a, src, started)
	}()
	select {
	case err := <-started:
		if err != nil {
			return err
		}
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	p.mu.Lock()
	p.agents = append(p.agents, a)
	p.mu.Unlock()
	return nil
}

// run is an agent's goroutine: it runs the agent's script, then hands each
// broadcast to the function the script asked to receive them with.
func (p *agentPool) run(a *agent, src string, started chan<- error) {
	rt := p.newRuntime()
	defer rt.Close()
	var receiver quickjs.Value
	for name, fn := range map[string]any{
		"agentReceiveBroadcast": func(f quickjs.Value) { receiver = f },
		"agentReport": func(s string) {
			select {
			case p.reports <- s:
			case <-p.ctx.Done():
			}
		},
		"agentLeaving":      func() {},
		"agentSleep":        p.sleep,
		"agentMonotonicNow": p.now,
	} {
		if err := rt.Set(name, fn); err != nil {
			started <- err
			return
		}
	}
	if _, err := rt.EvalContext(p.ctx, `var $262 = { agent: {
		receiveBroadcast: agentReceiveBroadcast,
		report: function (value) { agentReport(String(value)); },
		leaving: agentLeaving,
		sleep: agentSleep,
		monotonicNow: agentMonotonicNow,
	} };`); err != nil {
		started <- err
		return
	}
	if _, err := rt.EvalContext(p.ctx, src); err != nil {
		started <- fmt.Errorf("agent: %w", err)
		return
	}
	started <- nil
	for {
		select {
		case <-hostjobs.Ready(rt):
			// A waitAsync another agent settled, run with the test's time.
			rt.EvalContext(p.ctx, "undefined")
		case msg := <-a.inbox:
			sab, err := sharedmem.Attach(rt, msg.mem)
			a.taken <- struct{}{}
			if err != nil || !receiver.IsFunction() {
				continue
			}
			id, err := rt.EvalContext(p.ctx, msg.id)
			if err != nil {
				continue
			}
			if _, err := receiver.Call(sab.(quickjs.Value), id); err != nil {
				continue
			}
			rt.RunJobs()
		case <-p.ctx.Done():
			return
		}
	}
}

// broadcast shares a SharedArrayBuffer and an id with every agent, returning
// once each has retrieved them.
func (p *agentPool) broadcast(sab, id quickjs.Value) error {
	mem, ok, err := sharedmem.Share(sab)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("broadcast requires a SharedArrayBuffer")
	}
	literal := id.String()
	if id.Kind() == quickjs.KindBigInt {
		literal += "n"
	}
	p.mu.Lock()
	agents := append([]*agent(nil), p.agents...)
	p.mu.Unlock()
	msg := broadcastMsg{mem: mem, id: literal}
	for _, a := range agents {
		select {
		case a.inbox <- msg:
			<-a.taken
		case <-a.done:
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}
	return nil
}
