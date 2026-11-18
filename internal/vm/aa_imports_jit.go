//go:build (linux || windows) && !android && amd64 && quickjs_jit

// The packages package vm imports, in the order its files imported them
// first at f9829c9f, before its files were renamed. The linker lays out
// the packages linked before vm in the order vm's files, in file-name
// order, first import them, which no pad or placement moves; this file,
// sorted before every other, keeps that order whatever the files are
// called (docs/optimizing.md, "The layout lock"). Each import is a
// group of its own, so that gofmt keeps the order.

package vm

import (
	_ "math"

	_ "strconv"

	_ "strings"

	_ "sync/atomic"

	_ "github.com/go-quickjs/go-quickjs/internal/jsnum"

	_ "github.com/go-quickjs/go-quickjs/internal/fdlibm"

	_ "math/rand/v2"

	_ "slices"

	_ "unicode"

	_ "github.com/go-quickjs/go-quickjs/internal/wtf8"

	_ "encoding/base64"

	_ "github.com/go-quickjs/go-intl"

	_ "math/bits"

	_ "github.com/go-quickjs/go-quickjs/internal/bignum"

	_ "github.com/go-quickjs/go-quickjs/internal/bytecode"

	_ "encoding/binary"

	_ "time"

	_ "github.com/go-quickjs/go-intl/date"

	_ "github.com/go-quickjs/go-intl/temporal"

	_ "bytes"

	_ "unicode/utf8"

	_ "fmt"

	_ "unsafe"

	_ "github.com/go-quickjs/go-quickjs/internal/jsonscan"

	_ "hash/maphash"

	_ "sort"

	_ "math/big"

	_ "errors"

	_ "github.com/go-quickjs/go-quickjs/internal/regexp"

	_ "unicode/utf16"

	_ "cmp"

	_ "weak"

	_ "runtime"

	_ "sync"

	_ "context"

	_ "reflect"

	_ "runtime/metrics"

	_ "regexp"

	_ "github.com/go-quickjs/go-quickjs/internal/sourcemap"

	_ "github.com/go-quickjs/go-quickjs/internal/jit/ssa"

	_ "github.com/go-quickjs/go-quickjs/internal/jit/mir"

	_ "github.com/go-quickjs/go-quickjs/internal/jit"

	_ "github.com/go-quickjs/go-quickjs/internal/jit/abi"

	_ "github.com/go-quickjs/go-quickjs/internal/jit/ir"

	_ "os"

	_ "github.com/go-quickjs/go-quickjs/internal/jit/compile"

	_ "iter"

	_ "golang.org/x/sys/cpu"

	_ "maps"

	_ "github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
)
