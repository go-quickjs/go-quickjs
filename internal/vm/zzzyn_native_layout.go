//go:build quickjs_nativelayout

package vm

import (
	"reflect"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// NativeLayout is what the native handlers' generator
// (internal/vm/internal/nativegen) reads of the package's private layout,
// as plain numbers by name: offsets, each field's by "Type.field" (a
// promoted field's too), sizes by type, and the constants the handlers
// test values and states against. Its helper (nativegen/cmd/layout),
// built with the quickjs_nativelayout tag that alone builds this file,
// prints them for the generator, which never imports the package.
//
// It builds on every architecture, with the types it reads
// (zzzyn_native_ctx.go), so that the helper runs on any host: Go lays
// these structs out alike on every 64-bit one, so a 64-bit host's layout
// is amd64's and arm64's (nativegen.ReadLayout). What a host cannot give
// is a build's whose files its own GOOS and GOARCH leave out: the JIT's,
// whose fields the Runtime has only where the JIT is built, which
// jitBuilt says.
func NativeLayout() (offsets, sizes map[string]uintptr, consts map[string]uint64) {
	offsets, sizes = map[string]uintptr{}, map[string]uintptr{}
	for _, t := range []reflect.Type{
		reflect.TypeFor[nativeCtx](),
		reflect.TypeFor[nativeRec](),
		reflect.TypeFor[Runtime](),
		reflect.TypeFor[regState](),
		reflect.TypeFor[regCode](),
		reflect.TypeFor[regCallCache](),
		reflect.TypeFor[frame](),
		reflect.TypeFor[closure](),
		reflect.TypeFor[closureCode](),
		reflect.TypeFor[funcData](),
		reflect.TypeFor[Object](),
		reflect.TypeFor[objectExt](),
		reflect.TypeFor[shape](),
		reflect.TypeFor[propIndex](),
		reflect.TypeFor[propCache](),
		reflect.TypeFor[propPoly](),
		reflect.TypeFor[protoCell](),
		reflect.TypeFor[primEntry](),
		reflect.TypeFor[hasEntry](),
		reflect.TypeFor[String](),
		reflect.TypeFor[typedArrayData](),
		reflect.TypeFor[iterObject](),
		reflect.TypeFor[iterState](),
		reflect.TypeFor[forInObject](),
		reflect.TypeFor[forInState](),
		reflect.TypeFor[keyAtom](),
		reflect.TypeFor[Symbol](),
		reflect.TypeFor[propKey](),
		reflect.TypeFor[generatorObject](),
		reflect.TypeFor[generator](),
		reflect.TypeFor[Realm](),
		reflect.TypeFor[upvalue](),
		reflect.TypeFor[regSwitchTable](),
		reflect.TypeFor[bytecode.SwitchCase](),
	} {
		sizes[t.Name()] = t.Size()
		depth := map[string]int{}
		for _, f := range reflect.VisibleFields(t) {
			// A field reached through a pointer has no offset in t.
			off, in, direct := uintptr(0), t, true
			for _, i := range f.Index {
				if in.Kind() != reflect.Struct {
					direct = false
					break
				}
				off += in.Field(i).Offset
				in = in.Field(i).Type
			}
			name := t.Name() + "." + f.Name
			if !direct || f.Name == "_" {
				continue
			}
			if d, ok := depth[name]; ok && d <= len(f.Index) {
				continue
			}
			depth[name] = len(f.Index)
			offsets[name] = off
		}
	}
	consts = map[string]uint64{
		// jitBuilt says the build has the JIT, whose fields the Runtime
		// and a closure's code have in it alone.
		"jitBuilt": 0,

		"rtBase": uint64(rtBase),
		"ccBase": uint64(ccBase),

		"tagBase":         tagBase,
		"shortIntBase":    shortIntBase,
		"shortBigBase":    shortBigBase,
		"shortBigLowBits": shortBigLowBits,
		"heapBigBits":     heapBigBits,

		"KindUndefined": uint64(KindUndefined),
		"KindNull":      uint64(KindNull),
		"KindBool":      uint64(KindBool),
		"KindString":    uint64(KindString),
		"KindSymbol":    uint64(KindSymbol),
		"KindObject":    uint64(KindObject),
		"KindBigInt":    uint64(KindBigInt),

		"ClassObject":     uint64(ClassObject),
		"ClassArray":      uint64(ClassArray),
		"ClassTypedArray": uint64(ClassTypedArray),
		"ClassCursor":     uint64(ClassCursor),
		"ClassGenerator":  uint64(ClassGenerator),

		"genSuspendedYield": uint64(genSuspendedYield),
		"genExecuting":      uint64(genExecuting),
		"genCompleted":      uint64(genCompleted),
		"regFrameGenStep":   uint64(regFrameGenStep),

		"objExtensible":          uint64(objExtensible),
		"objHasExt":              uint64(objHasExt),
		"objHasData":             uint64(objHasData),
		"objHasSparseElements":   uint64(objHasSparseElements),
		"objSharedElems":         uint64(objSharedElems),
		"objPolyProto":           uint64(objPolyProto),
		"objMappedArguments":     uint64(objMappedArguments),
		"objArrayLengthWritable": uint64(objArrayLengthWritable),
		"objHTMLDDA":             uint64(objHTMLDDA),

		"elemInt8":         uint64(elemInt8),
		"elemUint8":        uint64(elemUint8),
		"elemUint8Clamped": uint64(elemUint8Clamped),
		"elemInt16":        uint64(elemInt16),
		"elemUint16":       uint64(elemUint16),
		"elemInt32":        uint64(elemInt32),
		"elemUint32":       uint64(elemUint32),
		"elemFloat32":      uint64(elemFloat32),
		"elemFloat64":      uint64(elemFloat64),
		"elemBigInt64":     uint64(elemBigInt64),
		"elemBigUint64":    uint64(elemBigUint64),

		"elemStrCharCodeAt": uint64(elemStrCharCodeAt),
		"elemPush":          uint64(elemPush),

		"regCallEnter":   uint64(regCallEnter),
		"regCallThis":    uint64(regCallThis),
		"regCallObjThis": uint64(regCallObjThis),
		"regCallNative":  uint64(regCallNative),
		"regCallOut":     uint64(regCallOut),
		"regCallLeaf":    uint64(regCallLeaf),
		"regLeafArg":     uint64(regLeafArg),
		"regLeafCode":    uint64(regLeafCode),
		"regFrameCall":   uint64(regFrameCall),
		"regFrameEnter":  uint64(regFrameEnter),

		"tailCallDepth":   uint64(tailCallDepth),
		"linearScanLimit": uint64(linearScanLimit),
		"atomIndexTag":    uint64(atomIndexTag),
		"hasCacheBits":    uint64(hasCacheBits),
		"hasCacheMul":     uint64(hasCacheMul),
		"keySlotMul":      keySlotMul,
		"intStringsLen":   uint64(len(Runtime{}.intStrings)),

		"sE":     sE,
		"sWin":   sWin,
		"sKind":  sKind,
		"sRC":    sRC,
		"sArgc":  sArgc,
		"sBase":  sBase,
		"sTop":   sTop,
		"sNF":    sNF,
		"sSaved": sSaved,
		"sI":     sI,
		"sEnd":   sEnd,
		"sV0":    sV0,
		"sV1":    sV1,
		"sEnt":   sEnt,
		"sKa":    sKa,
	}
	if jitBuilt {
		consts["jitBuilt"] = 1
	}
	return offsets, sizes, consts
}
