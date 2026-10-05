package stdlib

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Resolver is what DNS asks names of. *net.Resolver is one -- the system's,
// or Go's own with a Dial of the host's choosing -- and a host can put
// anything else in front of it: a cache, DNS over HTTPS, a fixed table.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupNS(ctx context.Context, name string) ([]*net.NS, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// DNS describes the name resolution a runtime is given: the "dns" and
// "dns/promises" modules.
//
// Looking a name up reaches the network, and it is a way out of it as well:
// whatever answers for a domain sees every name asked of it. A host handing
// this to code it did not write says which names it may ask about:
//
//	stdlib.Names(rt, &stdlib.DNS{
//	    Loop:  loop,
//	    Allow: func(name string) error {
//	        if !strings.HasSuffix(name, ".example.com") {
//	            return fmt.Errorf("%s is not allowed", name)
//	        }
//	        return nil
//	    },
//	})
type DNS struct {
	// Loop is where an answer is delivered: with one, a query runs on a
	// goroutine of its own and the script goes on meanwhile. Without one it
	// is made and waited for before the call returns.
	Loop *Loop
	// Resolver answers the queries. Nil is net.DefaultResolver: the system's
	// configuration and, where Go uses it, the system's own resolver.
	Resolver Resolver
	// Allow is asked about every name before it is looked up, and every
	// address before it is looked up in reverse. Returning an error refuses
	// it: the script's query fails with the code EREFUSED, and the error's
	// cause carries the reason.
	Allow func(name string) error
	// Servers is what getServers answers: the servers the host says the
	// resolver asks, if it wants the script to know. setServers is refused
	// either way -- which servers are asked is the host's to decide.
	Servers []string
	// Timeout bounds each query. Zero means ten seconds.
	Timeout time.Duration
}

// Names installs the "dns" and "dns/promises" modules, as node has them:
// lookup, which asks the system as getaddrinfo does, the resolve family,
// which asks for records of one type, reverse and lookupService, and the
// Resolver class, whose queries cancel together.
//
// What Go's resolver does not offer is refused with ENOTIMP rather than
// imitated: records of the types CAA, NAPTR, SOA, TLSA and ANY, and a
// record's time to live. A TXT record is one string, its pieces joined. A
// service is answered by its port's number rather than its name.
func Names(rt *quickjs.Runtime, cfg *DNS) error {
	if cfg == nil {
		cfg = &DNS{}
	}
	d := &dnsHost{cfg: cfg, resolver: cfg.Resolver}
	if d.resolver == nil {
		d.resolver = net.DefaultResolver
	}
	host := rt.NewObject()
	if err := host.Set("query", d.query); err != nil {
		return err
	}
	servers := slices.Clone(cfg.Servers)
	if servers == nil {
		servers = []string{}
	}
	if err := host.Set("servers", servers); err != nil {
		return err
	}
	api, err := evalWithHost(rt, "<dns>", dnsJS, host)
	if err != nil {
		return err
	}
	for _, mod := range []struct{ name, key string }{{"dns", "dns"}, {"dns/promises", "promises"}} {
		v, err := api.Get(mod.key)
		if err != nil {
			return err
		}
		exports, err := moduleExports(v)
		if err != nil {
			return err
		}
		if err := rt.SetModuleValues(mod.name, exports); err != nil {
			return err
		}
		if err := rt.SetModuleValues("node:"+mod.name, exports); err != nil {
			return err
		}
	}
	return nil
}

type dnsHost struct {
	cfg      *DNS
	resolver Resolver
}

// dnsError is a failed query, as node reports one: "queryA ENOTFOUND
// example.invalid", with the code, the call and the name as properties.
type dnsError struct {
	code, syscall, hostname string
	// cause is why the host refused the query, for one it refused.
	cause error
}

func (e *dnsError) Error() string { return e.syscall + " " + e.code + " " + e.hostname }

// syscalls are the calls node names a query of each kind after.
var syscalls = map[string]string{
	"lookup": "getaddrinfo", "reverse": "getHostByAddr", "lookupService": "getnameinfo",
	"A": "queryA", "AAAA": "queryAaaa", "CNAME": "queryCname", "MX": "queryMx", "NS": "queryNs",
	"PTR": "queryPtr", "SRV": "querySrv", "TXT": "queryTxt", "ANY": "queryAny", "CAA": "queryCaa",
	"NAPTR": "queryNaptr", "SOA": "querySoa", "TLSA": "queryTlsa",
}

// query answers one query of the script's: kind is lookup, lookupService,
// reverse or a record type, and the promise is fulfilled with what node's
// call gives its callback, or rejected with node's error. register is given
// a function that cancels the query, which a Resolver's cancel calls.
func (d *dnsHost) query(r *quickjs.Runtime, kind, name string, family, port int, register quickjs.Value) *quickjs.Promise {
	p := r.NewPromise()
	syscall := syscalls[kind]
	if d.cfg.Allow != nil {
		if err := d.cfg.Allow(name); err != nil {
			p.Reject(dnsErrorValue(r, &dnsError{"EREFUSED", syscall, name, err}))
			return p
		}
	}
	timeout := d.cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(loopContext(d.cfg.Loop), timeout)
	if register.IsFunction() {
		if _, err := register.Call(cancel); err != nil {
			cancel()
			p.RejectError(err)
			return p
		}
	}
	deliver := func(v any, err error) {
		cancel()
		var de *dnsError
		switch {
		case errors.As(err, &de):
			p.Reject(dnsErrorValue(r, de))
		case err != nil:
			p.RejectError(err)
		default:
			if err := p.Resolve(v); err != nil {
				p.RejectError(err)
			}
		}
	}
	if d.cfg.Loop == nil {
		v, err := d.answer(ctx, kind, name, family, port, syscall)
		deliver(v, err)
		return p
	}
	loop := d.cfg.Loop
	loop.Begin()
	go func() {
		defer loop.Done()
		v, err := d.answer(ctx, kind, name, family, port, syscall)
		loop.Post(func() { deliver(v, err) })
	}()
	return p
}

// answer makes the query, off the runtime's goroutine: what it returns is
// plain Go data, which is converted where the promise is settled.
func (d *dnsHost) answer(ctx context.Context, kind, name string, family, port int, syscall string) (any, error) {
	fail := func(err error) error { return &dnsError{code: dnsCode(ctx, err, kind), syscall: syscall, hostname: name} }
	res := d.resolver
	switch kind {
	case "lookup", "A", "AAAA":
		network := "ip"
		switch {
		case kind == "A" || family == 4:
			network = "ip4"
		case kind == "AAAA" || family == 6:
			network = "ip6"
		}
		var addrs []netip.Addr
		if a, err := netip.ParseAddr(name); err == nil && kind == "lookup" {
			// An address is its own answer, as getaddrinfo gives it, if it
			// is of the family asked for.
			if network == "ip" || (network == "ip4") == a.Unmap().Is4() {
				addrs = []netip.Addr{a}
			}
		} else {
			var err error
			if addrs, err = res.LookupNetIP(ctx, network, name); err != nil {
				return nil, fail(err)
			}
		}
		if len(addrs) == 0 {
			return nil, &dnsError{code: notFound(kind), syscall: syscall, hostname: name}
		}
		if kind != "lookup" {
			out := make([]string, len(addrs))
			for i, a := range addrs {
				out[i] = a.Unmap().String()
			}
			return out, nil
		}
		out := make([]dnsAddress, len(addrs))
		for i, a := range addrs {
			a = a.Unmap()
			out[i] = dnsAddress{Address: a.String(), Family: 4}
			if a.Is6() {
				out[i].Family = 6
			}
		}
		return out, nil
	case "reverse", "lookupService", "PTR":
		addr := name
		if kind == "PTR" {
			ip, ok := reverseName(name)
			if !ok {
				return nil, &dnsError{code: "ENOTIMP", syscall: syscall, hostname: name}
			}
			addr = ip.String()
		} else if _, err := netip.ParseAddr(addr); err != nil {
			return nil, &dnsError{code: "EINVAL", syscall: syscall, hostname: name}
		}
		hosts, err := res.LookupAddr(ctx, addr)
		if err != nil {
			return nil, fail(err)
		}
		for i := range hosts {
			hosts[i] = strings.TrimSuffix(hosts[i], ".")
		}
		if kind == "lookupService" {
			if len(hosts) == 0 {
				return nil, &dnsError{code: "ENOTFOUND", syscall: syscall, hostname: name}
			}
			return dnsService{Hostname: hosts[0], Service: fmt.Sprint(port)}, nil
		}
		return hosts, nil
	case "CNAME":
		cname, err := res.LookupCNAME(ctx, name)
		if err != nil {
			return nil, fail(err)
		}
		cname = strings.TrimSuffix(cname, ".")
		// Go answers a name without a CNAME record with the name itself.
		if strings.EqualFold(cname, strings.TrimSuffix(name, ".")) {
			return nil, &dnsError{code: "ENODATA", syscall: syscall, hostname: name}
		}
		return []string{cname}, nil
	case "MX":
		mx, err := res.LookupMX(ctx, name)
		if err != nil {
			return nil, fail(err)
		}
		out := make([]dnsMX, len(mx))
		for i, m := range mx {
			out[i] = dnsMX{Exchange: strings.TrimSuffix(m.Host, "."), Priority: int(m.Pref)}
		}
		return out, nil
	case "NS":
		ns, err := res.LookupNS(ctx, name)
		if err != nil {
			return nil, fail(err)
		}
		out := make([]string, len(ns))
		for i, n := range ns {
			out[i] = strings.TrimSuffix(n.Host, ".")
		}
		return out, nil
	case "SRV":
		_, srv, err := res.LookupSRV(ctx, "", "", name)
		if err != nil {
			return nil, fail(err)
		}
		out := make([]dnsSRV, len(srv))
		for i, s := range srv {
			out[i] = dnsSRV{Name: strings.TrimSuffix(s.Target, "."), Port: int(s.Port),
				Priority: int(s.Priority), Weight: int(s.Weight)}
		}
		return out, nil
	case "TXT":
		txt, err := res.LookupTXT(ctx, name)
		if err != nil {
			return nil, fail(err)
		}
		out := make([][]string, len(txt))
		for i, t := range txt {
			out[i] = []string{t}
		}
		return out, nil
	}
	return nil, &dnsError{code: "ENOTIMP", syscall: syscall, hostname: name}
}

// The records a query is answered with, as node shapes them: a struct
// rather than a map, so that their properties come in node's order.
type (
	dnsAddress struct {
		Address string `json:"address"`
		Family  int    `json:"family"`
	}
	dnsService struct {
		Hostname string `json:"hostname"`
		Service  string `json:"service"`
	}
	dnsMX struct {
		Exchange string `json:"exchange"`
		Priority int    `json:"priority"`
	}
	dnsSRV struct {
		Name     string `json:"name"`
		Port     int    `json:"port"`
		Priority int    `json:"priority"`
		Weight   int    `json:"weight"`
	}
)

// notFound is the code for a name that exists without records of a kind:
// getaddrinfo's has none, and a query's is ENODATA.
func notFound(kind string) string {
	if kind == "lookup" {
		return "ENOTFOUND"
	}
	return "ENODATA"
}

// dnsCode is node's code for a query that failed with err.
func dnsCode(ctx context.Context, err error, kind string) string {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "ECANCELLED"
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsNotFound:
			return "ENOTFOUND"
		case de.IsTimeout:
			return "ETIMEOUT"
		case de.IsTemporary && kind == "lookup":
			return "EAI_AGAIN"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "ETIMEOUT"
	}
	if kind == "lookup" {
		return "ENOTFOUND"
	}
	return "ESERVFAIL"
}

// reverseName is the address a PTR query's name stands for: an
// in-addr.arpa or ip6.arpa name.
func reverseName(name string) (netip.Addr, bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if rest, ok := strings.CutSuffix(name, ".in-addr.arpa"); ok {
		parts := strings.Split(rest, ".")
		if len(parts) != 4 {
			return netip.Addr{}, false
		}
		slices.Reverse(parts)
		a, err := netip.ParseAddr(strings.Join(parts, "."))
		return a, err == nil && a.Is4()
	}
	if rest, ok := strings.CutSuffix(name, ".ip6.arpa"); ok {
		nibbles := strings.Split(rest, ".")
		if len(nibbles) != 32 {
			return netip.Addr{}, false
		}
		slices.Reverse(nibbles)
		var b strings.Builder
		for i, n := range nibbles {
			if len(n) != 1 {
				return netip.Addr{}, false
			}
			if i > 0 && i%4 == 0 {
				b.WriteByte(':')
			}
			b.WriteString(n)
		}
		a, err := netip.ParseAddr(b.String())
		return a, err == nil && a.Is6()
	}
	return netip.Addr{}, false
}

// dnsErrorValue is the Error node rejects a failed query with.
func dnsErrorValue(r *quickjs.Runtime, e *dnsError) quickjs.Value {
	v := r.NewError("Error", e.Error())
	v.Set("code", e.code)
	v.Set("syscall", e.syscall)
	v.Set("hostname", e.hostname)
	if e.cause != nil {
		v.Set("cause", r.NewError("Error", e.cause.Error()))
	}
	return v
}

// dnsJS is the modules' script half: argument checking and the shapes of
// node's API, over the host's query.
const dnsJS = `(host) => {
  "use strict";
  const codes = {
    NODATA: "ENODATA", FORMERR: "EFORMERR", SERVFAIL: "ESERVFAIL", NOTFOUND: "ENOTFOUND",
    NOTIMP: "ENOTIMP", REFUSED: "EREFUSED", BADQUERY: "EBADQUERY", BADNAME: "EBADNAME",
    BADFAMILY: "EBADFAMILY", BADRESP: "EBADRESP", CONNREFUSED: "ECONNREFUSED", TIMEOUT: "ETIMEOUT",
    EOF: "EOF", FILE: "EFILE", NOMEM: "ENOMEM", DESTRUCTION: "EDESTRUCTION", BADSTR: "EBADSTR",
    BADFLAGS: "EBADFLAGS", NONAME: "ENONAME", BADHINTS: "EBADHINTS", NOTINITIALIZED: "ENOTINITIALIZED",
    LOADIPHLPAPI: "ELOADIPHLPAPI", ADDRGETNETWORKPARAMS: "EADDRGETNETWORKPARAMS", CANCELLED: "ECANCELLED",
  };
  const ADDRCONFIG = 1024, V4MAPPED = 2048, ALL = 256;

  function received(v) {
    if (v === undefined || v === null) return "Received " + v;
    if (typeof v === "function") return "Received function " + (v.name || "<anonymous>");
    if (typeof v === "object") return "Received an instance of " + (v.constructor && v.constructor.name || "Object");
    const shown = typeof v === "string" ? "'" + v + "'" : String(v);
    return "Received type " + typeof v + " (" + shown + ")";
  }
  function invalidType(name, type, v) {
    const e = new TypeError('The "' + name + '" argument must be of type ' + type + ". " + received(v));
    e.code = "ERR_INVALID_ARG_TYPE";
    return e;
  }
  function invalidValue(what, v, must) {
    const e = new TypeError(must ? "The " + what + " " + must + ". Received " + (typeof v === "string" ? "'" + v + "'" : String(v))
      : "The argument '" + what + "' is invalid. Received " + (typeof v === "string" ? "'" + v + "'" : String(v)));
    e.code = "ERR_INVALID_ARG_VALUE";
    return e;
  }
  function checkName(v, name) {
    if (typeof v !== "string") throw invalidType(name || "hostname", "string", v);
  }
  function checkCallback(cb) {
    if (typeof cb !== "function") throw invalidType("callback", "function", cb);
    return cb;
  }

  let order = "verbatim";
  function sorted(list, how) {
    if (how === "ipv4first") return list.filter(a => a.family === 4).concat(list.filter(a => a.family === 6));
    if (how === "ipv6first") return list.filter(a => a.family === 6).concat(list.filter(a => a.family === 4));
    return list;
  }

  function makeAPI(cancels) {
    const register = cancels ? c => { cancels.add(c) } : undefined;
    const ask = (kind, name, family, port) => host.query(kind, name, family || 0, port || 0, register);

    async function lookup(hostname, options) {
      checkName(hostname);
      let family = 0, all = false, how = order;
      if (typeof options === "number") family = options;
      else if (options !== undefined && options !== null) {
        if (typeof options !== "object") throw invalidType("options", "object", options);
        family = options.family === undefined ? 0 : options.family;
        all = options.all === true;
        if (options.order !== undefined) how = options.order;
        else if (options.verbatim === false) how = "ipv4first";
      }
      if (family === "IPv4") family = 4;
      else if (family === "IPv6") family = 6;
      if (family !== 0 && family !== 4 && family !== 6) throw invalidValue("property 'options.family'", family, "must be one of: 0, 4, 6");
      if (hostname === "") return all ? [] : { address: null, family: family === 6 ? 6 : 4 };
      const list = sorted(await ask("lookup", hostname, family), how);
      return all ? list : list[0];
    }
    async function lookupService(address, port) {
      if (typeof address !== "string" || !/^[0-9a-fA-F:.]+$/.test(address)) throw invalidValue("address", address);
      if (typeof port !== "number" || !Number.isInteger(port) || port < 0 || port > 65535) {
        const e = new RangeError("Port should be >= 0 and < 65536. Received " + received(port).replace("Received ", ""));
        e.code = "ERR_SOCKET_BAD_PORT";
        throw e;
      }
      return ask("lookupService", address, 0, port);
    }
    const types = ["A", "AAAA", "ANY", "CAA", "CNAME", "MX", "NAPTR", "NS", "PTR", "SOA", "SRV", "TLSA", "TXT"];
    async function resolve(hostname, rrtype) {
      checkName(hostname);
      if (rrtype === undefined) rrtype = "A";
      if (typeof rrtype !== "string") throw invalidType("rrtype", "string", rrtype);
      if (!types.includes(rrtype)) throw invalidValue("rrtype", rrtype);
      return ask(rrtype, hostname);
    }
    function ofType(rrtype) {
      return async function (hostname, options) {
        checkName(hostname);
        if (options && options.ttl) {
          const e = new Error("query" + rrtype[0] + rrtype.slice(1).toLowerCase() + " ENOTIMP " + hostname);
          e.code = "ENOTIMP"; e.syscall = "query" + rrtype[0] + rrtype.slice(1).toLowerCase(); e.hostname = hostname;
          throw e;
        }
        return ask(rrtype, hostname);
      };
    }
    async function reverse(ip) {
      checkName(ip, "ip");
      return ask("reverse", ip);
    }
    const api = {
      lookup, lookupService, resolve, reverse,
      resolve4: ofType("A"), resolve6: ofType("AAAA"), resolveAny: ofType("ANY"), resolveCaa: ofType("CAA"),
      resolveCname: ofType("CNAME"), resolveMx: ofType("MX"), resolveNaptr: ofType("NAPTR"),
      resolveNs: ofType("NS"), resolvePtr: ofType("PTR"), resolveSoa: ofType("SOA"),
      resolveSrv: ofType("SRV"), resolveTlsa: ofType("TLSA"), resolveTxt: ofType("TXT"),
      getServers() { return host.servers.slice() },
      setServers(servers) {
        if (!Array.isArray(servers)) throw invalidType("servers", "an instance of Array", servers);
        const e = new Error("dns.setServers is not allowed: the host decides which servers are asked");
        e.code = "ERR_ACCESS_DENIED";
        throw e;
      },
    };
    return api;
  }

  const promises = makeAPI();
  class PromiseResolver {
    #cancels = new Set();
    constructor(options) { Object.assign(this, makeAPI(this.#cancels)); delete this.lookup; delete this.lookupService; }
    cancel() { for (const c of this.#cancels) c(); this.#cancels.clear(); }
    setLocalAddress() {}
  }
  Object.assign(promises, codes, {
    Resolver: PromiseResolver,
    setDefaultResultOrder, getDefaultResultOrder,
  });

  // The callback forms: the same queries, their results handed to a
  // callback as node hands them -- lookup's as (err, address, family), or
  // (err, addresses) with all, lookupService's as (err, hostname, service).
  const queries = Object.keys(makeAPI()).filter(n => n !== "getServers" && n !== "setServers");
  function callbacks(p) {
    const out = {};
    for (const name of queries) {
      if (!p[name]) continue;
      out[name] = function (...args) {
        const cb = checkCallback(args.pop());
        p[name](...args).then(v => cb(null, v), cb);
      };
    }
    out.getServers = p.getServers;
    out.setServers = p.setServers;
    if (p.lookup) {
      out.lookup = function (hostname, options, cb) {
        if (typeof options === "function") { cb = options; options = undefined; }
        checkCallback(cb);
        p.lookup(hostname, options).then(r => {
          if (options && typeof options === "object" && options.all) cb(null, r);
          else cb(null, r.address, r.family);
        }, cb);
      };
      out.lookupService = function (address, port, cb) {
        checkCallback(cb);
        p.lookupService(address, port).then(r => cb(null, r.hostname, r.service), cb);
      };
    }
    for (const name of queries.filter(n => n.startsWith("resolve") && p[n])) {
      out[name] = function (hostname, options, cb) {
        if (typeof options === "function") { cb = options; options = undefined; }
        checkCallback(cb);
        p[name](hostname, options).then(r => cb(null, r), cb);
      };
    }
    return out;
  }

  function setDefaultResultOrder(value) {
    if (value !== "ipv4first" && value !== "ipv6first" && value !== "verbatim") {
      throw invalidValue("argument 'value'", value, "must be one of: 'verbatim', 'ipv4first', 'ipv6first'");
    }
    order = value;
  }
  function getDefaultResultOrder() { return order; }

  const dns = callbacks(promises);
  class Resolver {
    #p = new PromiseResolver();
    constructor(options) { Object.assign(this, callbacks(this.#p)); }
    cancel() { this.#p.cancel(); }
    setLocalAddress() {}
  }
  Object.assign(dns, codes, {
    ADDRCONFIG, V4MAPPED, ALL, Resolver, promises,
    setDefaultResultOrder, getDefaultResultOrder,
  });
  return { dns, promises };
}`
