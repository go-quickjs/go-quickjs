package inspector

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The protocol's methods, on the runtime's goroutine.

// dispatch does one request and returns its result.
func (c *session) dispatch(method string, raw json.RawMessage) (any, error) {
	t := c.t
	v := t.vm
	var p params
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &protocolError{-32602, "Invalid parameters: " + err.Error()}
		}
	}
	switch method {
	// --- Runtime ---------------------------------------------------------
	case "Runtime.enable":
		c.runtimeEnabled = true
		t.hookConsole()
		c.event("Runtime.executionContextCreated", map[string]any{"context": map[string]any{
			"id": 1, "origin": "", "name": t.opts.Title, "uniqueId": t.id + "-1",
			"auxData": map[string]any{"isDefault": true},
		}})
		return nil, nil
	case "Runtime.disable":
		c.runtimeEnabled = false
		return nil, nil
	case "Runtime.runIfWaitingForDebugger":
		t.released = true
		return nil, nil
	case "Runtime.evaluate":
		r, err := v.DebugEvaluate(p.Expression)
		return c.evaluated(r, err, p.ObjectGroup, p.ReturnByValue)
	case "Runtime.callFunctionOn":
		return c.callFunctionOn(p)
	case "Runtime.getProperties":
		return c.getProperties(p)
	case "Runtime.releaseObject":
		delete(c.objects, p.ObjectID)
		return nil, nil
	case "Runtime.releaseObjectGroup":
		c.releaseGroup(p.ObjectGroup)
		return nil, nil
	case "Runtime.compileScript":
		if err := v.DebugCompile(p.Expression); err != nil {
			return map[string]any{"exceptionDetails": c.exceptionDetails(err, "")}, nil
		}
		return map[string]any{}, nil
	case "Runtime.globalLexicalScopeNames":
		names := v.DebugGlobalLexicalNames()
		if names == nil {
			names = []string{}
		}
		return map[string]any{"names": names}, nil
	case "Runtime.getIsolateId":
		return map[string]any{"id": t.id}, nil
	case "Runtime.getHeapUsage":
		return map[string]any{"usedSize": 0, "totalSize": 0}, nil
	case "Runtime.discardConsoleEntries", "Runtime.setAsyncCallStackDepth",
		"Runtime.setMaxCallStackSizeToCapture", "Runtime.setCustomObjectFormatterEnabled":
		return nil, nil

	// --- Debugger --------------------------------------------------------
	case "Debugger.enable":
		c.debuggerEnabled = true
		for _, s := range v.DebugScripts() {
			c.event("Debugger.scriptParsed", t.scriptInfo(s))
		}
		if t.pause != nil {
			c.event("Debugger.paused", c.pausedParams(t.pause))
		}
		return map[string]any{"debuggerId": t.id}, nil
	case "Debugger.disable":
		c.debuggerEnabled = false
		return nil, nil
	case "Debugger.setBreakpointByUrl":
		return c.setBreakpointByURL(p)
	case "Debugger.setBreakpoint":
		s := c.script(p.Location.ScriptID)
		if s == nil {
			return nil, errorf("Script not found")
		}
		id, at, ok := v.SetScriptBreakpoint(s, p.Location.LineNumber+1, p.Location.ColumnNumber+1)
		if !ok {
			return nil, errorf("Could not resolve breakpoint")
		}
		c.own(id, p.Condition)
		return map[string]any{"breakpointId": strconv.Itoa(id), "actualLocation": location(at)}, nil
	case "Debugger.removeBreakpoint":
		id, err := strconv.Atoi(p.BreakpointID)
		if err != nil {
			return nil, errorf("Breakpoint not found")
		}
		v.RemoveBreakpoint(id)
		delete(t.owner, id)
		delete(t.conditions, id)
		return nil, nil
	case "Debugger.getPossibleBreakpoints":
		return c.possibleBreakpoints(p)
	case "Debugger.getScriptSource":
		s := c.script(p.ScriptID)
		if s == nil {
			return nil, errorf("No script for id: %s", p.ScriptID)
		}
		return map[string]any{"scriptSource": s.Source()}, nil
	case "Debugger.pause":
		v.PauseAtNextStatement()
		return nil, nil
	case "Debugger.resume":
		return nil, t.goOn(vm.Continue)
	case "Debugger.stepOver":
		return nil, t.goOn(vm.StepOver)
	case "Debugger.stepInto":
		return nil, t.goOn(vm.StepInto)
	case "Debugger.stepOut":
		return nil, t.goOn(vm.StepOut)
	case "Debugger.setPauseOnExceptions":
		mode := map[string]vm.ExceptionPause{"none": vm.PauseOnNoExceptions,
			"uncaught": vm.PauseOnUncaughtExceptions, "all": vm.PauseOnAllExceptions,
			"caught": vm.PauseOnAllExceptions}[p.State]
		v.SetPauseOnExceptions(mode)
		return nil, nil
	case "Debugger.evaluateOnCallFrame":
		f, err := t.frame(p.CallFrameID)
		if err != nil {
			return nil, err
		}
		r, err := f.Evaluate(p.Expression)
		return c.evaluated(r, err, p.ObjectGroup, p.ReturnByValue)
	case "Debugger.setVariableValue":
		f, err := t.frame(p.CallFrameID)
		if err != nil {
			return nil, err
		}
		val, err := c.argument(p.NewValue)
		if err != nil {
			return nil, err
		}
		if err := f.SetVariable(p.VariableName, val); err != nil {
			return nil, errorf("%s", err.Error())
		}
		return nil, nil
	case "Debugger.setBreakpointsActive":
		v.SetBreakpointsActive(p.Active)
		return nil, nil
	case "Debugger.setSkipAllPauses":
		t.skipAll = p.Skip
		return nil, nil
	case "Debugger.setAsyncCallStackDepth", "Debugger.setBlackboxPatterns",
		"Debugger.setBlackboxExecutionContexts", "Debugger.setBlackboxedRanges",
		"Debugger.setInstrumentationBreakpoint", "Debugger.removeInstrumentationBreakpoint":
		return nil, nil

	// --- What clients ask of Node, which there is nothing to say to -------
	case "Profiler.enable", "Profiler.disable", "HeapProfiler.enable", "HeapProfiler.disable",
		"Console.enable", "Console.disable", "Log.enable", "Log.disable",
		"NodeRuntime.enable", "NodeRuntime.disable", "NodeRuntime.notifyWhenWaitingForDisconnect",
		"NodeWorker.enable", "NodeWorker.disable", "Target.setAutoAttach", "Target.setDiscoverTargets",
		"Network.enable", "Network.disable", "Inspector.enable":
		return nil, nil
	case "Schema.getDomains":
		return map[string]any{"domains": []any{
			map[string]string{"name": "Runtime", "version": "1.3"},
			map[string]string{"name": "Debugger", "version": "1.3"},
		}}, nil
	}
	if strings.HasPrefix(method, "Profiler.") || strings.HasPrefix(method, "HeapProfiler.") {
		// Profiling is out of scope: a host profiles with Go's own tools.
		return nil, errorf("go-quickjs does not profile scripts through the inspector; profile the host with Go's pprof")
	}
	return nil, &protocolError{-32601, "'" + method + "' wasn't found"}
}

// params are every request's, the ones it has set.
type params struct {
	Expression    string          `json:"expression"`
	ObjectGroup   string          `json:"objectGroup"`
	ReturnByValue bool            `json:"returnByValue"`
	ObjectID      string          `json:"objectId"`
	OwnProperties bool            `json:"ownProperties"`
	AccessorsOnly bool            `json:"accessorPropertiesOnly"`
	FunctionDecl  string          `json:"functionDeclaration"`
	Arguments     []callArgument  `json:"arguments"`
	LineNumber    int             `json:"lineNumber"`
	ColumnNumber  int             `json:"columnNumber"`
	URL           string          `json:"url"`
	URLRegex      string          `json:"urlRegex"`
	Condition     string          `json:"condition"`
	Location      locationParam   `json:"location"`
	Start         locationParam   `json:"start"`
	End           *locationParam  `json:"end"`
	BreakpointID  string          `json:"breakpointId"`
	ScriptID      string          `json:"scriptId"`
	State         string          `json:"state"`
	CallFrameID   string          `json:"callFrameId"`
	VariableName  string          `json:"variableName"`
	NewValue      callArgument    `json:"newValue"`
	Active        bool            `json:"active"`
	Skip          bool            `json:"skip"`
	Extra         json.RawMessage `json:"-"`
}

type locationParam struct {
	ScriptID     string `json:"scriptId"`
	LineNumber   int    `json:"lineNumber"`
	ColumnNumber int    `json:"columnNumber"`
}

type callArgument struct {
	Value               json.RawMessage `json:"value"`
	UnserializableValue string          `json:"unserializableValue"`
	ObjectID            string          `json:"objectId"`
}

// script is a script by the ID a client was given.
func (c *session) script(id string) *vm.DebugScript {
	for _, s := range c.t.vm.DebugScripts() {
		if strconv.Itoa(s.ID) == id {
			return s
		}
	}
	return nil
}

// own records who set a breakpoint, and its condition.
func (c *session) own(id int, cond string) {
	c.t.owner[id] = c
	if cond != "" {
		c.t.conditions[id] = cond
	}
}

func (c *session) setBreakpointByURL(p params) (any, error) {
	t := c.t
	if p.URL == "" && p.URLRegex == "" {
		return nil, errorf("Either url or urlRegex must be specified.")
	}
	match, err := vm.URLMatcher(p.URL, p.URLRegex)
	if err != nil {
		return nil, errorf("Incorrect url regex")
	}
	id, at := t.vm.SetBreakpointMatching(func(s *vm.DebugScript) bool {
		return match(t.urlOf(s))
	}, p.LineNumber+1, p.ColumnNumber+1)
	c.own(id, p.Condition)
	locs := []any{}
	for _, l := range at {
		locs = append(locs, location(l))
	}
	return map[string]any{"breakpointId": strconv.Itoa(id), "locations": locs}, nil
}

func (c *session) possibleBreakpoints(p params) (any, error) {
	s := c.script(p.Start.ScriptID)
	if s == nil {
		return nil, errorf("Script not found")
	}
	before := func(line, col int, l locationParam) bool {
		return line-1 < l.LineNumber || line-1 == l.LineNumber && col-1 < l.ColumnNumber
	}
	locs := []any{}
	for _, l := range s.Locations() {
		if before(l.Line, l.Column, p.Start) {
			continue
		}
		if p.End != nil && !before(l.Line, l.Column, *p.End) {
			continue
		}
		locs = append(locs, location(l))
	}
	return map[string]any{"locations": locs}, nil
}

// evaluated is an evaluation's result, or the exception it threw.
func (c *session) evaluated(v vm.Value, err error, group string, byValue bool) (any, error) {
	if err != nil {
		var thrown *vm.Thrown
		if errors.As(err, &thrown) {
			return map[string]any{
				"result":           c.remote(thrown.Value, group),
				"exceptionDetails": c.exceptionDetails(err, group),
			}, nil
		}
		return map[string]any{
			"result":           map[string]any{"type": "object", "subtype": "error", "className": "Error", "description": err.Error()},
			"exceptionDetails": c.exceptionDetails(err, group),
		}, nil
	}
	if byValue {
		return map[string]any{"result": c.byValue(v)}, nil
	}
	return map[string]any{"result": c.remote(v, group)}, nil
}

// exceptionDetails describes what an evaluation threw.
func (c *session) exceptionDetails(err error, group string) map[string]any {
	d := map[string]any{"exceptionId": 1, "text": "Uncaught", "lineNumber": 0, "columnNumber": 0}
	var thrown *vm.Thrown
	var syntax *quickjs.SyntaxError
	switch {
	case errors.As(err, &thrown):
		d["exception"] = c.remote(thrown.Value, group)
	case errors.As(err, &syntax):
		_, line, col := syntax.Position()
		d["text"] = "Uncaught SyntaxError: " + syntax.Error()
		d["lineNumber"], d["columnNumber"] = max(line-1, 0), max(col-1, 0)
		d["exception"] = map[string]any{"type": "object", "subtype": "error", "className": "SyntaxError", "description": "SyntaxError: " + syntax.Error()}
	default:
		d["text"] = "Uncaught " + err.Error()
	}
	return d
}

func (c *session) callFunctionOn(p params) (any, error) {
	v := c.t.vm
	fn, err := v.DebugEvaluate("(" + p.FunctionDecl + ")")
	if err != nil {
		return c.evaluated(vm.Undefined, err, p.ObjectGroup, false)
	}
	this := vm.Undefined
	if p.ObjectID != "" {
		r := c.objects[p.ObjectID]
		if r == nil {
			return nil, errorf("Could not find object with given id")
		}
		this = r.value
	}
	var args []vm.Value
	for _, a := range p.Arguments {
		x, err := c.argument(a)
		if err != nil {
			return nil, err
		}
		args = append(args, x)
	}
	r, err := v.DebugCall(fn, this, args)
	return c.evaluated(r, err, p.ObjectGroup, p.ReturnByValue)
}

// argument is a value a client handed over: an object it was given, a
// value JSON writes, or one it cannot.
func (c *session) argument(a callArgument) (vm.Value, error) {
	switch {
	case a.ObjectID != "":
		r := c.objects[a.ObjectID]
		if r == nil {
			return vm.Undefined, errorf("Could not find object with given id")
		}
		return r.value, nil
	case a.UnserializableValue != "":
		return c.t.vm.DebugEvaluate(a.UnserializableValue)
	case len(a.Value) == 0:
		return vm.Undefined, nil
	}
	return c.t.vm.DebugEvaluate("(" + string(a.Value) + ")")
}

func (c *session) getProperties(p params) (any, error) {
	r := c.objects[p.ObjectID]
	if r == nil {
		return nil, errorf("Could not find object with given id")
	}
	group := r.group
	result := []any{}
	if r.scope != nil {
		for _, b := range r.scope {
			e := map[string]any{"name": b.Name, "writable": b.Mutable,
				"configurable": false, "enumerable": true, "isOwn": true}
			if !b.Uninitialized {
				// A binding the code has not reached has no value, which a
				// client shows as unavailable, as V8's are.
				e["value"] = c.remote(b.Value, group)
			}
			result = append(result, e)
		}
		return map[string]any{"result": result}, nil
	}
	if !r.value.IsObject() {
		return map[string]any{"result": result}, nil
	}
	v := c.t.vm
	seen := map[string]bool{}
	var internal []any
	for o, own := r.value.Object(), true; o != nil; o, own = o.Proto(), false {
		props, slots := v.DebugProperties(o)
		if own {
			for _, s := range slots {
				internal = append(internal, map[string]any{"name": s.Name, "value": c.remote(s.Value, group)})
			}
		}
		for _, dp := range props {
			key := dp.Name
			if dp.Symbol != nil {
				key = "@@" + key
			}
			if seen[key] || p.AccessorsOnly && !dp.Accessor {
				continue
			}
			seen[key] = true
			e := map[string]any{"name": dp.Name, "configurable": dp.Configurable, "enumerable": dp.Enumerable, "isOwn": own}
			if dp.Accessor {
				e["get"] = c.remoteObject(dp.Getter, group)
				e["set"] = c.remoteObject(dp.Setter, group)
			} else {
				e["value"] = c.remote(dp.Value, group)
				e["writable"] = dp.Writable
			}
			if dp.Symbol != nil {
				e["symbol"] = c.remote(vm.Sym(dp.Symbol), group)
			}
			result = append(result, e)
		}
		if p.OwnProperties {
			break
		}
	}
	out := map[string]any{"result": result}
	if internal != nil {
		out["internalProperties"] = internal
	}
	return out, nil
}

// remote is an object a client was given, by its ID.
type remote struct {
	value vm.Value
	// scope is a local or closure scope's bindings, which are no object.
	scope []vm.DebugBinding
	group string
}

// register gives the client an object, in a group it may release at once.
func (c *session) register(r *remote) string {
	c.nextObject++
	id := strconv.Itoa(c.nextObject)
	c.objects[id] = r
	return id
}

func (c *session) releaseGroup(group string) {
	for id, r := range c.objects {
		if r.group == group {
			delete(c.objects, id)
		}
	}
}

// scopeObject is a frame's local or closure scope as the object a client
// lists the bindings of.
func (c *session) scopeObject(f *vm.DebugFrame, sc vm.DebugScope) map[string]any {
	// The bindings come innermost first; a client lists them in the order
	// they were declared, as V8's are.
	bindings := slices.Clone(sc.Bindings)
	slices.Reverse(bindings)
	id := c.register(&remote{scope: bindings, group: "backtrace"})
	return map[string]any{"type": "object", "className": "Object", "description": "Object", "objectId": id}
}

func (c *session) remoteObject(o *vm.Object, group string) map[string]any {
	if o == nil {
		return map[string]any{"type": "undefined"}
	}
	return c.remote(vm.Obj(o), group)
}

// remote is a value as the protocol's RemoteObject: a primitive by value, an
// object or a symbol by an ID the client can ask more of.
func (c *session) remote(v vm.Value, group string) map[string]any {
	d := c.t.vm.DebugDescribe(v)
	o := map[string]any{"type": d.Type}
	if d.Subtype != "" {
		o["subtype"] = d.Subtype
	}
	switch {
	case v.IsUndefined():
	case v.IsNull():
		o["value"] = nil
	case v.IsBool():
		o["value"] = v.BoolValue()
		o["description"] = d.Description
	case v.IsNumber():
		if d.Unserializable != "" {
			o["unserializableValue"] = d.Unserializable
		} else {
			o["value"] = v.Number()
		}
		o["description"] = d.Description
	case v.IsString():
		o["value"] = d.Description
	case v.IsBigInt():
		o["unserializableValue"] = d.Unserializable
		o["description"] = d.Description
	default:
		if group == "" {
			group = "default"
		}
		o["objectId"] = c.register(&remote{value: v, group: group})
		o["description"] = d.Description
		if d.ClassName != "" {
			o["className"] = d.ClassName
		}
	}
	return o
}

// byValue is a value as JSON writes it, for a client that asked for its
// result by value: an object's enumerable data properties, and nothing a
// getter or a toJSON would say.
func (c *session) byValue(v vm.Value) map[string]any {
	d := c.t.vm.DebugDescribe(v)
	o := map[string]any{"type": d.Type}
	if d.Unserializable != "" {
		o["unserializableValue"] = d.Unserializable
		return o
	}
	o["value"] = c.plain(v, 0, map[*vm.Object]bool{})
	return o
}

func (c *session) plain(v vm.Value, depth int, seen map[*vm.Object]bool) any {
	switch {
	case v.IsUndefined(), v.IsNull(), v.IsSymbol():
		return nil
	case v.IsBool():
		return v.BoolValue()
	case v.IsNumber():
		if x := v.Number(); !math.IsNaN(x) && !math.IsInf(x, 0) {
			return x
		}
		return nil
	case v.IsString():
		return v.String().Go()
	case v.IsBigInt():
		return c.t.vm.DebugDescribe(v).Description
	}
	o := v.Object()
	if seen[o] || depth > 20 || o.IsCallable() {
		return nil
	}
	seen[o] = true
	defer delete(seen, o)
	props, _ := c.t.vm.DebugProperties(o)
	if o.IsArray() {
		out := []any{}
		for _, dp := range props {
			if _, err := strconv.ParseUint(dp.Name, 10, 32); err == nil && !dp.Accessor {
				out = append(out, c.plain(dp.Value, depth+1, seen))
			}
		}
		return out
	}
	out := map[string]any{}
	for _, dp := range props {
		if dp.Enumerable && !dp.Accessor && dp.Symbol == nil {
			out[dp.Name] = c.plain(dp.Value, depth+1, seen)
		}
	}
	return out
}
