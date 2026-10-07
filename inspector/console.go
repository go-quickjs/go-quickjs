package inspector

import (
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// What the console writes, a client sees: the console's methods are
// wrapped, once a client enables the Runtime domain, to tell the clients of
// each call before they do what they did.

// consoleJS wraps a console's methods, calling report with each call's kind
// and arguments. It is evaluated as the module's own code, which a debugger
// neither lists nor stops in.
const consoleJS = `(function (console, report) {
  for (const kind of ["log", "info", "warn", "error", "debug", "dir", "dirxml", "table",
      "trace", "assert", "group", "groupCollapsed", "groupEnd", "clear"]) {
    const original = console[kind];
    if (typeof original !== "function") continue;
    console[kind] = { [kind](...args) {
      if (kind !== "assert" || !args[0]) report(kind, kind === "assert" ? args.slice(1) : args);
      return original.apply(this, args);
    } }[kind];
  }
})`

// consoleTypes are the protocol's names of the console's methods.
var consoleTypes = map[string]string{
	"warn": "warning", "group": "startGroup", "groupCollapsed": "startGroupCollapsed",
	"groupEnd": "endGroup",
}

// hookConsole wraps the runtime's console, the first time a client enables
// the Runtime domain, if the runtime has one.
func (t *Target) hookConsole() {
	if t.console {
		return
	}
	console, err := t.rt.Get("console")
	if err != nil || !console.IsObject() {
		return
	}
	wrap, err := hostaccess.EvalInternal(t.rt, "<inspector>", consoleJS)
	if err != nil {
		return
	}
	report := func(kind string, args quickjs.Value) { t.consoleCalled(kind, args) }
	if _, err := wrap.(quickjs.Value).Call(console, report); err == nil {
		t.console = true
	}
}

// consoleCalled tells the clients that enabled the Runtime domain of a
// console call, with its arguments and where it was made.
func (t *Target) consoleCalled(kind string, args quickjs.Value) {
	typ := kind
	if s, ok := consoleTypes[kind]; ok {
		typ = s
	}
	arr := hostaccess.Unwrap(args)
	n := 0
	if l, err := args.Get("length"); err == nil {
		n = int(l.Float())
	}
	var frames []any
	for _, e := range t.vm.DebugStack(32) {
		loc := location(e.Location)
		frames = append(frames, map[string]any{
			"functionName": e.FunctionName, "scriptId": loc["scriptId"], "url": t.urlOf(e.Location.Script),
			"lineNumber": loc["lineNumber"], "columnNumber": loc["columnNumber"],
		})
	}
	now := float64(time.Now().UnixNano()) / 1e6
	for _, c := range t.clients() {
		if !c.runtimeEnabled || c.gone {
			continue
		}
		var list []any
		for i := 0; i < n; i++ {
			v, err := t.vm.GetProp(arr, t.vm.Intern(itoa(i)))
			if err != nil {
				v = vm.Undefined
			}
			list = append(list, c.remote(v, "console"))
		}
		params := map[string]any{"type": typ, "args": list, "executionContextId": 1, "timestamp": now}
		if frames != nil {
			params["stackTrace"] = map[string]any{"callFrames": frames}
		}
		c.event("Runtime.consoleAPICalled", params)
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}
