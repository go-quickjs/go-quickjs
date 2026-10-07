package inspector

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The target's side of the VM's DebugHandler: what it reports, and the
// pause, which serves the clients until one says to go on.

// debuggers are the clients that enabled the Debugger domain.
func (t *Target) debuggers() []*session {
	var out []*session
	for _, c := range t.clients() {
		if c.debuggerEnabled && !c.gone {
			out = append(out, c)
		}
	}
	return out
}

// ScriptParsed tells the debugging clients of a script.
func (t *Target) ScriptParsed(s *vm.DebugScript) {
	for _, c := range t.debuggers() {
		c.event("Debugger.scriptParsed", t.scriptInfo(s))
	}
}

// BreakpointResolved tells the debugging clients where a breakpoint set by
// a URL took effect in a script loaded since.
func (t *Target) BreakpointResolved(id int, at vm.DebugLocation) {
	for _, c := range t.debuggers() {
		c.event("Debugger.breakpointResolved", map[string]any{
			"breakpointId": strconv.Itoa(id), "location": location(at),
		})
	}
}

// Paused tells the debugging clients the code stopped, and serves them
// until one says how to go on -- or none is left, the target is detached
// or the runtime closed, when the code goes on.
func (t *Target) Paused(p *vm.DebugPause) vm.StepAction {
	if t.skipAll || len(t.debuggers()) == 0 {
		return vm.Continue
	}
	if p.Reason == vm.PauseBreakpoint && !t.conditionsHold(p) {
		return vm.Continue
	}
	t.pause, t.resumed = p, false
	t.pauseSeq++
	for _, c := range t.debuggers() {
		c.event("Debugger.paused", c.pausedParams(p))
	}
	defer func() { t.breakOnStart = false }()
	for !t.resumed {
		if len(t.debuggers()) == 0 || t.isDetached() {
			t.resume = vm.Continue
			break
		}
		select {
		case <-t.vm.DebugReady():
			t.vm.DebugDrain()
		case <-t.vm.DebugClosed():
			t.resume, t.resumed = vm.Continue, true
		}
	}
	t.pause = nil
	for _, c := range t.debuggers() {
		c.releaseGroup("backtrace")
		c.event("Debugger.resumed", map[string]any{})
	}
	return t.resume
}

// conditionsHold reports whether a breakpoint the code stopped at has no
// condition, or one that holds there.
func (t *Target) conditionsHold(p *vm.DebugPause) bool {
	for _, id := range p.Breakpoints {
		cond := t.conditions[id]
		if cond == "" {
			return true
		}
		v, err := p.Frames[0].Evaluate(cond)
		if err == nil && v.Truthy() {
			return true
		}
	}
	return len(p.Breakpoints) == 0
}

// goOn ends a pause, as a client's resume or step says.
func (t *Target) goOn(a vm.StepAction) error {
	if t.pause == nil {
		return errorf("Can only perform operation while paused.")
	}
	t.resume, t.resumed = a, true
	return nil
}

// pausedParams are Debugger.paused's, with the frames' objects the
// client's.
func (c *session) pausedParams(p *vm.DebugPause) map[string]any {
	t := c.t
	frames := []any{}
	for i, f := range p.Frames {
		loc := f.Location()
		var chain []any
		for _, sc := range f.Scopes() {
			kind := map[vm.ScopeKind]string{vm.ScopeLocal: "local", vm.ScopeClosure: "closure",
				vm.ScopeWith: "with", vm.ScopeScript: "script", vm.ScopeGlobal: "global"}[sc.Kind]
			var obj map[string]any
			if sc.Object != nil {
				obj = c.remote(vm.Obj(sc.Object), "backtrace")
			} else {
				obj = c.scopeObject(f, sc)
			}
			entry := map[string]any{"type": kind, "object": obj}
			if sc.Kind == vm.ScopeLocal && f.FunctionName() != "" {
				entry["name"] = f.FunctionName()
			}
			chain = append(chain, entry)
		}
		frame := map[string]any{
			"callFrameId":    strconv.Itoa(t.pauseSeq) + ":" + strconv.Itoa(i),
			"functionName":   f.FunctionName(),
			"location":       location(loc),
			"url":            t.urlOf(loc.Script),
			"scopeChain":     chain,
			"this":           c.remote(f.This(), "backtrace"),
			"canBeRestarted": false,
		}
		frames = append(frames, frame)
	}
	params := map[string]any{"callFrames": frames, "reason": "other"}
	switch p.Reason {
	case vm.PauseException:
		params["reason"] = "exception"
		data := c.remote(p.Exception, "backtrace")
		data["uncaught"] = !p.Caught
		params["data"] = data
	case vm.PauseRequested:
		if c.t.breakOnStart {
			params["reason"] = "Break on start"
		}
	}
	if len(p.Breakpoints) > 0 {
		var ids []string
		for _, id := range p.Breakpoints {
			ids = append(ids, strconv.Itoa(id))
		}
		params["hitBreakpoints"] = ids
	}
	return params
}

// frame is a frame of the current pause, by the ID a client was given.
func (t *Target) frame(id string) (*vm.DebugFrame, error) {
	if t.pause == nil {
		return nil, errorf("Can only perform operation while paused.")
	}
	seq, i, ok := strings.Cut(id, ":")
	n, err1 := strconv.Atoi(i)
	if !ok || seq != strconv.Itoa(t.pauseSeq) || err1 != nil || n < 0 || n >= len(t.pause.Frames) {
		return nil, errorf("Invalid call frame id")
	}
	return t.pause.Frames[n], nil
}

// location is a place in a script as the protocol has it, its line and
// column counted from 0.
func location(at vm.DebugLocation) map[string]any {
	id := ""
	if at.Script != nil {
		id = strconv.Itoa(at.Script.ID)
	}
	return map[string]any{"scriptId": id, "lineNumber": at.Line - 1, "columnNumber": at.Column - 1}
}

// urlOf is the URL a client is told a script has.
func (t *Target) urlOf(s *vm.DebugScript) string {
	switch {
	case s == nil || s.Name == "":
		return ""
	case s.HasSourceURL:
		// A name the code gave itself is told as it is, as V8 tells it.
		return s.Name
	}
	return t.opts.ScriptURL(s.Name)
}

// scriptInfo is Debugger.scriptParsed's params for a script.
func (t *Target) scriptInfo(s *vm.DebugScript) map[string]any {
	src := s.Source()
	startLine, startCol := s.Offset()
	lines := strings.Split(src, "\n")
	endLine := startLine + len(lines) - 1
	endCol := len(utf16.Encode([]rune(lines[len(lines)-1])))
	if len(lines) == 1 {
		endCol += startCol
	}
	sum := sha256.Sum256([]byte(src))
	info := map[string]any{
		"scriptId": strconv.Itoa(s.ID), "url": t.urlOf(s),
		"startLine": startLine, "startColumn": startCol, "endLine": endLine, "endColumn": endCol,
		"executionContextId": 1, "hash": hex.EncodeToString(sum[:]),
		"length": len(utf16.Encode([]rune(src))), "scriptLanguage": "JavaScript",
		"sourceMapURL": s.SourceMapURL, "hasSourceURL": s.HasSourceURL,
	}
	return info
}
