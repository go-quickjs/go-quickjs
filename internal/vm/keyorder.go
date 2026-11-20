package vm

import (
	"slices"
	"strings"
	"sync/atomic"
)

// The own keys of the built-ins are listed in V8's order, in both modes:
// the standard leaves the order of a built-in's properties to the
// implementation, and Node lists them so (testdata/builtin_keys.golden).
// The engine makes them in an order of its own -- by kind, by proposal, a
// namespace's in another init than its constructor's -- so each object's
// keys are given their places first (keyOrder, at the object's making or
// before its first definition), and the code that defines each keeps its
// place (setOwnRaw finds it), as Function.prototype's are given theirs: no
// definition moves. A key Node has that the engine has not is left out; one
// the engine has that Node has not -- a proposal Node has not shipped -- is
// made where its definition makes it, after the others.
//
// As the leaves' (regexpleaf.go), the functions are generic, instantiated
// with struct{} alone: laid out after every function and closure of the
// package, they move none of them.

// keyOrder gives c, the built-in name -- a constructor's or a namespace's
// own name -- and p, the constructor's prototype, either nil where it is not
// being made, their keys' places in V8's order (reserveKeys), where they
// differ from the engine's: one look at the name for both.
func keyOrder[_ any](r *Runtime, c, p *Object, name string) {
	switch name {
	case "%AsyncGeneratorPrototype%":
		if c != nil {
			reserveKeys[struct{}](r, c, 0, "constructor,next,return,throw,@@toStringTag")
		}
	case "%GeneratorPrototype%":
		if c != nil {
			reserveKeys[struct{}](r, c, 1, "constructor,next,return,throw,@@toStringTag")
		}
	case "%TypedArray%":
		if p != nil {
			reserveKeys[struct{}](r, p, 2, "constructor,buffer,byteLength,byteOffset,length,entries,keys,values,at,copyWithin,every,fill,filter,find,findIndex,findLast,findLastIndex,forEach,includes,indexOf,join,lastIndexOf,map,reverse,reduce,reduceRight,set,slice,some,sort,subarray,toReversed,toSorted,with,toLocaleString,toString,@@toStringTag,@@iterator")
		}
	case "AggregateError":
		if p != nil {
			reserveKeys[struct{}](r, p, 3, "constructor,name,message")
		}
	case "Array":
		if p != nil {
			reserveKeys[struct{}](r, p, 4, "length,constructor,at,concat,copyWithin,fill,find,findIndex,findLast,findLastIndex,lastIndexOf,pop,push,reverse,shift,unshift,slice,sort,splice,includes,indexOf,join,keys,entries,values,forEach,filter,flat,flatMap,map,every,some,reduce,reduceRight,toReversed,toSorted,toSpliced,with,toLocaleString,toString,@@iterator,@@unscopables")
		}
		if c != nil {
			reserveKeys[struct{}](r, c, 5, "length,name,prototype,isArray,from,fromAsync,of,@@species")
		}
	case "ArrayBuffer":
		if p != nil {
			reserveKeys[struct{}](r, p, 6, "constructor,byteLength,slice,maxByteLength,resizable,resize,transfer,transferToFixedLength,detached,@@toStringTag")
		}
	case "AsyncDisposableStack":
		if p != nil {
			reserveKeys[struct{}](r, p, 7, "constructor,use,disposeAsync,adopt,defer,move,disposed,@@asyncDispose,@@toStringTag")
		}
	case "Atomics":
		if c != nil {
			reserveKeys[struct{}](r, c, 8, "load,store,add,sub,and,or,xor,exchange,compareExchange,isLockFree,wait,waitAsync,notify,pause,@@toStringTag")
		}
	case "BigInt":
		if p != nil {
			reserveKeys[struct{}](r, p, 9, "constructor,toLocaleString,toString,valueOf,@@toStringTag")
		}
		if c != nil {
			reserveKeys[struct{}](r, c, 10, "length,name,prototype,asUintN,asIntN")
		}
	case "Collator":
		if p != nil {
			reserveKeys[struct{}](r, p, 11, "constructor,resolvedOptions,compare,@@toStringTag")
		}
	case "Date":
		if p != nil {
			reserveKeys[struct{}](r, p, 12, "constructor,toString,toDateString,toTimeString,toISOString,toUTCString,toGMTString,getDate,setDate,getDay,getFullYear,setFullYear,getHours,setHours,getMilliseconds,setMilliseconds,getMinutes,setMinutes,getMonth,setMonth,getSeconds,setSeconds,getTime,setTime,getTimezoneOffset,getUTCDate,setUTCDate,getUTCDay,getUTCFullYear,setUTCFullYear,getUTCHours,setUTCHours,getUTCMilliseconds,setUTCMilliseconds,getUTCMinutes,setUTCMinutes,getUTCMonth,setUTCMonth,getUTCSeconds,setUTCSeconds,valueOf,getYear,setYear,toJSON,toLocaleString,toLocaleDateString,toLocaleTimeString,toTemporalInstant,@@toPrimitive")
		}
	case "DateTimeFormat":
		if p != nil {
			reserveKeys[struct{}](r, p, 13, "constructor,resolvedOptions,formatToParts,format,formatRange,formatRangeToParts,@@toStringTag")
		}
	case "DisplayNames":
		if p != nil {
			reserveKeys[struct{}](r, p, 14, "constructor,resolvedOptions,of,@@toStringTag")
		}
	case "DisposableStack":
		if p != nil {
			reserveKeys[struct{}](r, p, 15, "constructor,use,dispose,adopt,defer,move,disposed,@@dispose,@@toStringTag")
		}
	case "Duration":
		if p != nil {
			reserveKeys[struct{}](r, p, 16, "constructor,years,months,weeks,days,hours,minutes,seconds,milliseconds,microseconds,nanoseconds,sign,blank,with,negated,abs,add,subtract,round,total,toLocaleString,toString,toJSON,valueOf,@@toStringTag")
		}
	case "DurationFormat":
		if p != nil {
			reserveKeys[struct{}](r, p, 17, "constructor,resolvedOptions,format,formatToParts,@@toStringTag")
		}
	case "Error":
		if p != nil {
			reserveKeys[struct{}](r, p, 18, "constructor,name,message,toString")
		}
		if c != nil {
			reserveKeys[struct{}](r, c, 19, "length,name,prototype,captureStackTrace,isError,stackTraceLimit")
		}
	case "EvalError":
		if p != nil {
			reserveKeys[struct{}](r, p, 20, "constructor,name,message")
		}
	case "Instant":
		if p != nil {
			reserveKeys[struct{}](r, p, 21, "constructor,epochMilliseconds,epochNanoseconds,add,subtract,until,since,round,equals,toLocaleString,toString,toJSON,valueOf,toZonedDateTimeISO,@@toStringTag")
		}
		if c != nil {
			reserveKeys[struct{}](r, c, 22, "length,name,prototype,from,fromEpochMilliseconds,fromEpochNanoseconds,compare")
		}
	case "Intl":
		if c != nil {
			reserveKeys[struct{}](r, c, 23, "getCanonicalLocales,supportedValuesOf,DateTimeFormat,NumberFormat,Collator,PluralRules,RelativeTimeFormat,ListFormat,Locale,DisplayNames,Segmenter,DurationFormat,@@toStringTag")
		}
	case "Iterator":
		if p != nil {
			reserveKeys[struct{}](r, p, 24, "constructor,reduce,toArray,forEach,some,every,find,map,filter,take,drop,flatMap,@@iterator,@@toStringTag,@@dispose")
		}
		if c != nil {
			reserveKeys[struct{}](r, c, 25, "length,name,prototype,from,concat")
		}
	case "JSON":
		if c != nil {
			reserveKeys[struct{}](r, c, 26, "parse,stringify,rawJSON,isRawJSON,@@toStringTag")
		}
	case "ListFormat":
		if p != nil {
			reserveKeys[struct{}](r, p, 27, "constructor,resolvedOptions,format,formatToParts,@@toStringTag")
		}
	case "Locale":
		if p != nil {
			reserveKeys[struct{}](r, p, 28, "constructor,toString,maximize,minimize,language,script,region,baseName,calendar,caseFirst,collation,firstDayOfWeek,hourCycle,numeric,numberingSystem,getCalendars,getCollations,getHourCycles,getNumberingSystems,getTimeZones,getTextInfo,getWeekInfo,@@toStringTag")
		}
	case "Map":
		if p != nil {
			reserveKeys[struct{}](r, p, 29, "constructor,get,set,has,delete,clear,entries,forEach,keys,size,values,getOrInsert,getOrInsertComputed,@@toStringTag,@@iterator")
		}
	case "Math":
		if c != nil {
			reserveKeys[struct{}](r, c, 30, "abs,acos,acosh,asin,asinh,atan,atanh,atan2,ceil,cbrt,expm1,clz32,cos,cosh,exp,floor,f16round,fround,hypot,imul,log,log1p,log2,log10,max,min,pow,random,round,sign,sin,sinh,sqrt,tan,tanh,trunc,E,LN10,LN2,LOG10E,LOG2E,PI,SQRT1_2,SQRT2,@@toStringTag")
		}
	case "Number":
		if p != nil {
			reserveKeys[struct{}](r, p, 31, "constructor,toExponential,toFixed,toPrecision,toString,valueOf,toLocaleString")
		}
		if c != nil {
			reserveKeys[struct{}](r, c, 32, "length,name,prototype,isFinite,isInteger,isNaN,isSafeInteger,parseFloat,parseInt,MAX_VALUE,MIN_VALUE,NaN,NEGATIVE_INFINITY,POSITIVE_INFINITY,MAX_SAFE_INTEGER,MIN_SAFE_INTEGER,EPSILON")
		}
	case "NumberFormat":
		if p != nil {
			reserveKeys[struct{}](r, p, 33, "constructor,resolvedOptions,formatToParts,format,formatRange,formatRangeToParts,@@toStringTag")
		}
	case "Object":
		if p != nil {
			reserveKeys[struct{}](r, p, 34, "constructor,__defineGetter__,__defineSetter__,hasOwnProperty,__lookupGetter__,__lookupSetter__,isPrototypeOf,propertyIsEnumerable,toString,valueOf,__proto__,toLocaleString")
		}
		if c != nil {
			reserveKeys[struct{}](r, c, 35, "length,name,prototype,assign,getOwnPropertyDescriptor,getOwnPropertyDescriptors,getOwnPropertyNames,getOwnPropertySymbols,hasOwn,is,preventExtensions,seal,create,defineProperties,defineProperty,freeze,getPrototypeOf,setPrototypeOf,isExtensible,isFrozen,isSealed,keys,entries,fromEntries,values,groupBy")
		}
	case "PlainDate":
		if p != nil {
			reserveKeys[struct{}](r, p, 36, "constructor,era,eraYear,calendarId,year,month,monthCode,day,dayOfWeek,dayOfYear,weekOfYear,yearOfWeek,daysInWeek,daysInMonth,daysInYear,monthsInYear,inLeapYear,toPlainYearMonth,toPlainMonthDay,add,subtract,with,withCalendar,until,since,equals,toPlainDateTime,toZonedDateTime,toString,toLocaleString,toJSON,valueOf,@@toStringTag")
		}
	case "PlainDateTime":
		if p != nil {
			reserveKeys[struct{}](r, p, 37, "constructor,calendarId,era,eraYear,year,month,monthCode,day,hour,minute,second,millisecond,microsecond,nanosecond,dayOfWeek,dayOfYear,weekOfYear,yearOfWeek,daysInWeek,daysInMonth,daysInYear,monthsInYear,inLeapYear,with,withCalendar,withPlainTime,add,subtract,until,since,round,equals,toLocaleString,toJSON,toString,valueOf,toZonedDateTime,toPlainDate,toPlainTime,@@toStringTag")
		}
	case "PlainMonthDay":
		if p != nil {
			reserveKeys[struct{}](r, p, 38, "constructor,calendarId,monthCode,day,with,equals,toLocaleString,toString,toJSON,valueOf,toPlainDate,@@toStringTag")
		}
	case "PlainTime":
		if p != nil {
			reserveKeys[struct{}](r, p, 39, "constructor,hour,minute,second,millisecond,microsecond,nanosecond,add,subtract,with,until,since,round,equals,toLocaleString,toString,toJSON,valueOf,@@toStringTag")
		}
	case "PlainYearMonth":
		if p != nil {
			reserveKeys[struct{}](r, p, 40, "constructor,calendarId,era,eraYear,year,month,monthCode,daysInYear,daysInMonth,monthsInYear,inLeapYear,with,add,subtract,until,since,equals,toLocaleString,toString,toJSON,valueOf,toPlainDate,@@toStringTag")
		}
	case "PluralRules":
		if p != nil {
			reserveKeys[struct{}](r, p, 41, "constructor,resolvedOptions,select,selectRange,@@toStringTag")
		}
	case "Promise":
		if c != nil {
			reserveKeys[struct{}](r, c, 42, "length,name,prototype,all,allSettled,any,race,resolve,reject,withResolvers,try,@@species")
		}
	case "RangeError":
		if p != nil {
			reserveKeys[struct{}](r, p, 43, "constructor,name,message")
		}
	case "ReferenceError":
		if p != nil {
			reserveKeys[struct{}](r, p, 44, "constructor,name,message")
		}
	case "Reflect":
		if c != nil {
			reserveKeys[struct{}](r, c, 45, "defineProperty,deleteProperty,apply,construct,get,getOwnPropertyDescriptor,getPrototypeOf,has,isExtensible,ownKeys,preventExtensions,set,setPrototypeOf,@@toStringTag")
		}
	case "RegExp":
		if p != nil {
			reserveKeys[struct{}](r, p, 46, "constructor,exec,dotAll,flags,global,hasIndices,ignoreCase,multiline,source,sticky,unicode,unicodeSets,compile,toString,test,@@match,@@matchAll,@@replace,@@search,@@split")
		}
	case "RelativeTimeFormat":
		if p != nil {
			reserveKeys[struct{}](r, p, 47, "constructor,resolvedOptions,format,formatToParts,@@toStringTag")
		}
	case "Segmenter":
		if p != nil {
			reserveKeys[struct{}](r, p, 48, "constructor,resolvedOptions,segment,@@toStringTag")
		}
	case "Set":
		if p != nil {
			reserveKeys[struct{}](r, p, 49, "constructor,has,add,delete,difference,clear,entries,forEach,intersection,isSubsetOf,isSupersetOf,isDisjointFrom,size,symmetricDifference,union,values,keys,@@toStringTag,@@iterator")
		}
	case "SharedArrayBuffer":
		if p != nil {
			reserveKeys[struct{}](r, p, 50, "constructor,byteLength,slice,maxByteLength,growable,grow,@@toStringTag")
		}
	case "String":
		if p != nil {
			reserveKeys[struct{}](r, p, 51, "length,constructor,anchor,at,big,blink,bold,charAt,charCodeAt,codePointAt,concat,endsWith,fontcolor,fontsize,fixed,includes,indexOf,isWellFormed,italics,lastIndexOf,link,localeCompare,match,matchAll,normalize,padEnd,padStart,repeat,replace,replaceAll,search,slice,small,split,strike,sub,substr,substring,sup,startsWith,toString,toWellFormed,trim,trimStart,trimLeft,trimEnd,trimRight,toLocaleLowerCase,toLocaleUpperCase,toLowerCase,toUpperCase,valueOf,@@iterator")
		}
	case "SuppressedError":
		if p != nil {
			reserveKeys[struct{}](r, p, 52, "constructor,name,message")
		}
	case "SyntaxError":
		if p != nil {
			reserveKeys[struct{}](r, p, 53, "constructor,name,message")
		}
	case "Temporal":
		if c != nil {
			reserveKeys[struct{}](r, c, 54, "Now,PlainDate,PlainTime,PlainDateTime,ZonedDateTime,Duration,Instant,PlainYearMonth,PlainMonthDay,@@toStringTag")
		}
	case "Temporal.Now":
		if c != nil {
			reserveKeys[struct{}](r, c, 55, "instant,timeZoneId,plainDateTimeISO,zonedDateTimeISO,plainDateISO,plainTimeISO,@@toStringTag")
		}
	case "TypeError":
		if p != nil {
			reserveKeys[struct{}](r, p, 56, "constructor,name,message")
		}
	case "URIError":
		if p != nil {
			reserveKeys[struct{}](r, p, 57, "constructor,name,message")
		}
	case "Uint8Array":
		if p != nil {
			reserveKeys[struct{}](r, p, 58, "constructor,BYTES_PER_ELEMENT,toBase64,setFromBase64,toHex,setFromHex")
		}
	case "WeakMap":
		if p != nil {
			reserveKeys[struct{}](r, p, 59, "constructor,delete,get,set,has,getOrInsert,getOrInsertComputed,@@toStringTag")
		}
	case "WeakSet":
		if p != nil {
			reserveKeys[struct{}](r, p, 60, "constructor,delete,has,add,@@toStringTag")
		}
	case "ZonedDateTime":
		if p != nil {
			reserveKeys[struct{}](r, p, 61, "constructor,timeZoneId,calendarId,era,eraYear,year,month,monthCode,day,hour,minute,second,millisecond,microsecond,nanosecond,epochMilliseconds,epochNanoseconds,dayOfWeek,dayOfYear,weekOfYear,yearOfWeek,hoursInDay,daysInWeek,daysInMonth,daysInYear,monthsInYear,inLeapYear,offsetNanoseconds,offset,with,withCalendar,withPlainTime,withTimeZone,add,subtract,until,since,round,equals,toLocaleString,toString,toJSON,valueOf,startOfDay,getTimeZoneTransition,toInstant,toPlainDate,toPlainTime,toPlainDateTime,@@toStringTag")
		}
	}
}

// keyOrderLists is how many lists of keys keyOrder has.
const keyOrderLists = 62

// keyOrderKey is a key of a list of keyOrder's: a name's atom, or, where
// sym is not 0, the well-known symbol wellKnownAt says, which is a
// runtime's own.
type keyOrderKey struct {
	a   Atom
	sym uint8
}

// keyOrderAtoms is the lists' keys as atoms of a base of the atom table
// (atomBase), which every runtime made on that base shares: a list's
// names, interned once for all of them, where each is the base's. A
// list's interning, at each runtime's making, had cost a tenth of it.
type keyOrderAtoms struct {
	base  *atomBase
	lists [keyOrderLists]atomic.Pointer[[]keyOrderKey]
}

// keyOrderCache is the lists' atoms of the published base (sharedAtoms),
// each list made by the first runtime on it that reserves it.
var keyOrderCache atomic.Pointer[keyOrderAtoms]

// reserveKeys gives o each of keys, list n's names separated by commas, it
// has not, in their order, a placeholder its definition overwrites in
// place: writable and configurable undefined, which no key keeps once the
// realm is made. A key "@@name" is the well-known symbol Symbol.name. A
// function's length and name, which it makes when first asked, and an
// array's or a string wrapper's length, which it synthesizes, are where
// they are. Only the keys o had before are looked for: a list names each
// key once.
func reserveKeys[_ any](r *Runtime, o *Object, n int, keys string) {
	list := keyOrderListOf[struct{}](r, n, keys)
	fn := o.fn() != nil
	synthLength := o.class == ClassArray || o.class == ClassStringWrapper
	had := int32(o.valn)
	o.reserveProps(int(o.valn) + len(list))
	for _, k := range list {
		a := k.a
		switch {
		case k.sym != 0:
			s := wellKnownAt[struct{}](r, k.sym)
			if s == nil {
				continue
			}
			a = r.atoms.internSymbol(s)
		case fn && (a == atomLength || a == atomName || a == atomPrototype), synthLength && a == atomLength:
			continue
		}
		if had != 0 && slices.ContainsFunc(o.shape.keys[:had], func(p propKey) bool { return p.key == a }) {
			continue
		}
		o.appendProp(Property{key: a, flags: propWritable | propConfigurable, value: Undefined})
	}
}

// keyOrderListOf is list n of keyOrder's, keys, as atoms: the base's
// (keyOrderCache), made if it has not been and every name is the base's,
// or the runtime's own.
func keyOrderListOf[_ any](r *Runtime, n int, keys string) []keyOrderKey {
	b := r.atoms.base
	c := keyOrderCache.Load()
	if b != nil && (c == nil || c.base != b) {
		// The base's lists, none made yet.
		keyOrderCache.CompareAndSwap(c, &keyOrderAtoms{base: b})
		c = keyOrderCache.Load()
	}
	if b != nil && c.base == b {
		if l := c.lists[n].Load(); l != nil {
			return *l
		}
	}
	list := make([]keyOrderKey, 0, strings.Count(keys, ",")+1)
	shared := b != nil
	for keys != "" {
		k, rest, _ := strings.Cut(keys, ",")
		keys = rest
		if len(k) > 2 && k[0] == '@' && k[1] == '@' {
			if i := wellKnownIndex[struct{}](k[2:]); i != 0 {
				list = append(list, keyOrderKey{sym: i})
			}
			continue
		}
		a := r.atoms.intern(k)
		if shared {
			_, shared = b.byName[k]
		}
		list = append(list, keyOrderKey{a: a})
	}
	if shared && c.base == b {
		c.lists[n].Store(&list)
	}
	return list
}

// wellKnownIndex is the well-known symbol Symbol.name's number, of those
// the built-ins' keys are (wellKnownAt), or 0.
func wellKnownIndex[_ any](name string) uint8 {
	switch name {
	case "iterator":
		return 1
	case "asyncIterator":
		return 2
	case "toPrimitive":
		return 3
	case "toStringTag":
		return 4
	case "species":
		return 5
	case "unscopables":
		return 6
	case "match":
		return 7
	case "matchAll":
		return 8
	case "replace":
		return 9
	case "search":
		return 10
	case "split":
		return 11
	case "dispose":
		return 12
	case "asyncDispose":
		return 13
	}
	return 0
}

// wellKnownAt is r's well-known symbol number i (wellKnownIndex).
func wellKnownAt[_ any](r *Runtime, i uint8) *Symbol {
	w := &r.wellKnown
	switch i {
	case 1:
		return w.iterator
	case 2:
		return w.asyncIterator
	case 3:
		return w.toPrimitive
	case 4:
		return w.toStringTag
	case 5:
		return w.species
	case 6:
		return w.unscopables
	case 7:
		return w.match
	case 8:
		return w.matchAll
	case 9:
		return w.replace
	case 10:
		return w.search
	case 11:
		return w.split
	case 12:
		return w.dispose
	case 13:
		return w.asyncDispose
	}
	return nil
}
