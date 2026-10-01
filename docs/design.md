# Design notes

A few decisions worth knowing about if you read the source.

**Values are NaN-boxed into 16 bytes.** A `Value` is a `float64` plus an
untyped pointer. Real numbers live in the float; every other kind is a quiet
NaN whose payload encodes the type, and the pointer is what that type refers to.
Nothing allocates, and a type check is a mask and compare. The invariant this
rests on is that a genuine NaN must never collide with a tag, so every number
is normalized on the way in.

**The interpreter allocates nothing per call.** One contiguous slice backs both
locals and operands; a frame is a window into it. Neither that slice nor the
frame stack is ever grown, which is what makes it safe for a closure to hold a
pointer into a live frame and for the interpreter to hold a `*frame` across
nested calls. Exhausting either is the stack limit, reported as the same
"maximum call stack size exceeded" a browser gives.

**Strings are UTF-8 with an ASCII fast path, and concatenation builds a rope.**
Repeated `s += x` is how scripts build large strings, and copying each time is
quadratic. Ropes defer the copy; flattening is iterative because a rope from a
200,000-iteration loop is 200,000 deep.

**Lone surrogates are fully supported.** A JavaScript string is a sequence of
UTF-16 code units with no well-formedness requirement, so `"\uD83D"` is an
ordinary one-code-unit string. Go cannot represent one — `WriteRune` substitutes
U+FFFD — so `internal/wtf8` encodes them in WTF-8. Well-formed text is
bit-identical to UTF-8.

**The regexp engine backtracks, because it has to.** Go's `regexp` is RE2, which
buys linear time by refusing backreferences, lookahead and lookbehind — exactly
what JavaScript requires. Backtracking brings an exponential worst case, so the
matcher gives up after a step budget rather than letting a hostile pattern stall
the host.

**Generators save their frame rather than running on a goroutine.** A goroutine
per generator is simpler but leaks one for every generator never exhausted.
Saving the frame works because `yield` is only valid inside the generator's own
body, so at suspension its frame is the top of the stack.

**Promise reactions never run synchronously.** Settling queues them; the queue
drains between turns. That ordering guarantee is the point of the design.

**The Array methods read through the property protocol, not the element
slice.** Nearly all of them are generic — `Array.prototype.map.call(arguments,
f)` is supposed to work — and a hole has to fall through to the prototype while
an index redefined as an accessor has to be called. There is a fast path for the
case that dominates, a dense array whose element really is there: such an
element shadows anything on the prototype and cannot be an accessor, so taking
it directly is not observable.

**An arrow function captures its surroundings when the closure is made.** It has
no `this`, `new.target`, `super` or `arguments` of its own, so they are read
from the creating frame and the arrow ignores whatever its own call supplies.
Nesting needs no extra work: an arrow created inside another has already
inherited them.

**The compiler knows whether a value is wanted.** A statement's expression
leaves a value the statement would then discard, and in a loop body that is an
instruction an iteration: `t += x` stores into a local and keeps a copy to
drop, `i++` keeps the value it had before, `o.x = v` carries the value back out
past the store. Each is compiled without that, and a comparison a branch tests
directly becomes one instruction rather than two. A counted loop of the kind
every program contains lost a fifth of its instructions to this.

**An object is allocated the size it is about to be.** An object literal says
how many properties it will be given, a constructor's body is walked for the
ones it assigns to `this`, and an array literal knows its length -- so the
table or the element storage travels in the object's own allocation rather than
being a second one. A closure is one allocation too: the function object, its
function data, the closure and the bindings it captures are laid out together.

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
[test262]: https://github.com/tc39/test262
[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
[go-intl]: https://github.com/go-quickjs/go-intl
[go-intl-compat]: https://github.com/go-quickjs/go-intl/blob/main/compat.go
[arm-aor]: https://github.com/ARM-software/optimized-routines
