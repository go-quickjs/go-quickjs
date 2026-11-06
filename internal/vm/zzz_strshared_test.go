package vm

import (
	"sync"
	"testing"
)

// One compiled program on runtimes of many goroutines: appends, a site's
// room and a for...in's cursor are all learned per runtime, and nothing of
// them is on the bytecode.Function they share, or on the strings the
// runtimes share, as the empty string is. go test -race says the rest.
func TestAppendsAndRoomsOnManyRuntimes(t *testing.T) {
	const src = `
function render(n) {
  var out = '', rows = []
  const push = x => { x !== null && (out += x) }
  for (let i = 0; i < n; i++) {
    rows.push(i)
    for (const k in { a: 1, b: 2 }) push('<' + k + '>')
    push(i % 3 ? 'text' : null)
  }
  var local = ''
  for (let i = 0; i < n; i++) { local += '<td>'; local += i; local += '</td>' }
  return out.length + ':' + local.length + ':' + rows.length
}
var last = ''
for (let i = 0; i < 40; i++) last = render(300)
last
`
	fn := compileForTest(t, src)
	const want = "2600:3490:300" // Node's
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := New(Config{})
			defer r.Close()
			v, err := r.Run(fn)
			if err != nil {
				t.Error(err)
				return
			}
			if got := v.String().Go(); got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		}()
	}
	wg.Wait()
}
