package stdlib_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// fakeResolver answers for example.test and its kin, and fails for the
// names the tests ask about failing.
type fakeResolver struct{}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	switch host {
	case "example.test":
		var out []netip.Addr
		if network != "ip6" {
			out = append(out, netip.MustParseAddr("192.0.2.1"))
		}
		if network != "ip4" {
			out = append(out, netip.MustParseAddr("2001:db8::1"))
		}
		return out, nil
	case "slow.test":
		<-ctx.Done()
		return nil, ctx.Err()
	case "flaky.test":
		return nil, &net.DNSError{Err: "server misbehaving", Name: host, IsTemporary: true}
	}
	return nil, notFound(host)
}

func (fakeResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	if addr == "192.0.2.1" {
		return []string{"example.test."}, nil
	}
	return nil, notFound(addr)
}

func (fakeResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	switch host {
	case "www.example.test":
		return "example.test.", nil
	case "example.test":
		return "example.test.", nil
	}
	return "", notFound(host)
}

func (fakeResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return []*net.MX{{Host: "mail.example.test.", Pref: 10}}, nil
}

func (fakeResolver) LookupNS(ctx context.Context, name string) ([]*net.NS, error) {
	return []*net.NS{{Host: "ns1.example.test."}, {Host: "ns2.example.test."}}, nil
}

func (fakeResolver) LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
	return name, []*net.SRV{{Target: "sip.example.test.", Port: 5060, Priority: 1, Weight: 2}}, nil
}

func (fakeResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return []string{"v=spf1 -all"}, nil
}

// TestDNS covers the dns modules over a resolver of the test's: lookup in
// its forms, the resolve family, reverse and lookupService, the callback
// forms, errors as node reports them, a name Allow refuses, a Resolver
// whose cancel stops its queries, and what is not offered.
func TestDNS(t *testing.T) {
	cfg := stdlib.Config{DNS: &stdlib.DNS{
		Resolver: fakeResolver{},
		Servers:  []string{"192.0.2.53"},
		Allow: func(name string) error {
			if strings.HasPrefix(name, "secret.") {
				return errors.New("no")
			}
			return nil
		},
	}}
	out, errOut := run(t, cfg, `
		const show = v => JSON.stringify(v);
		const fail = e => [e.constructor.name, e.code, e.syscall, e.hostname, e.message].join(" | ");
		(async () => {
			const dns = (await import("dns")).default;
			const dnsp = await import("node:dns/promises");
			const p = dns.promises;
			console.log(p === dnsp.default, typeof dnsp.lookup, dns.NOTFOUND, dns.ADDRCONFIG);

			console.log(show(await p.lookup("example.test")));
			console.log(show(await p.lookup("example.test", 6)));
			console.log(show(await p.lookup("example.test", { family: "IPv4" })));
			console.log(show(await p.lookup("example.test", { all: true })));
			console.log(show(await p.lookup("192.0.2.1")).length > 0);
			console.log(show(await p.resolve4("example.test")), show(await p.resolve6("example.test")));
			console.log(show(await p.resolve("example.test", "MX")), show(await p.resolveNs("x.test")));
			console.log(show(await p.resolveSrv("_sip._tcp.example.test")), show(await p.resolveTxt("example.test")));
			console.log(show(await p.resolveCname("www.example.test")), show(await p.reverse("192.0.2.1")));
			console.log(show(await p.resolvePtr("1.2.0.192.in-addr.arpa")), show(await p.lookupService("192.0.2.1", 443)));
			console.log(show(p.getServers()));

			for (const f of [
				() => p.lookup("missing.test"),
				() => p.resolve4("missing.test"),
				() => p.resolveCname("example.test"),
				() => p.lookup("flaky.test"),
				() => p.resolve4("secret.example.test"),
				() => p.reverse("not-an-ip"),
				() => p.resolveSoa("example.test"),
				() => p.resolve4("example.test", { ttl: true }),
			]) {
				try { await f(); console.log("no error") } catch (e) { console.log(fail(e)) }
			}
			for (const f of [
				() => p.lookup(42),
				() => p.lookup("example.test", { family: 5 }),
				() => p.resolve("example.test", "BOGUS"),
				() => p.lookupService("192.0.2.1", 70000),
				() => p.setServers(["8.8.8.8"]),
				() => dns.lookup("example.test"),
			]) {
				try { await f(); console.log("no error") } catch (e) { console.log(e.constructor.name, e.code) }
			}

			const r = new p.Resolver();
			const pending = r.resolve4("slow.test").catch(fail);
			r.cancel();
			console.log(await pending);

			await new Promise(done => {
				dns.lookup("example.test", (err, address, family) => {
					console.log("cb", err, address, family);
					dns.lookup("example.test", { all: true }, (err, list) => {
						console.log("cb all", list.length);
						dns.resolveMx("example.test", (err, mx) => {
							console.log("cb mx", show(mx));
							dns.lookupService("192.0.2.1", 22, (err, hostname, service) => {
								console.log("cb service", hostname, service);
								dns.resolve4("missing.test", err => {
									console.log("cb err", err.code);
									const cr = new dns.Resolver();
									cr.reverse("192.0.2.1", (err, names) => { console.log("cb resolver", names[0]); done() });
								});
							});
						});
					});
				});
			});
			dns.setDefaultResultOrder("ipv6first");
			console.log(dns.getDefaultResultOrder(), show(await p.lookup("example.test")));
		})().catch(e => console.log("uncaught", e));
	`)
	want := strings.Join([]string{
		`true function ENOTFOUND 1024`,
		`{"address":"192.0.2.1","family":4}`,
		`{"address":"2001:db8::1","family":6}`,
		`{"address":"192.0.2.1","family":4}`,
		`[{"address":"192.0.2.1","family":4},{"address":"2001:db8::1","family":6}]`,
		`true`,
		`["192.0.2.1"] ["2001:db8::1"]`,
		`[{"exchange":"mail.example.test","priority":10}] ["ns1.example.test","ns2.example.test"]`,
		`[{"name":"sip.example.test","port":5060,"priority":1,"weight":2}] [["v=spf1 -all"]]`,
		`["example.test"] ["example.test"]`,
		`["example.test"] {"hostname":"example.test","service":"443"}`,
		`["192.0.2.53"]`,
		`Error | ENOTFOUND | getaddrinfo | missing.test | getaddrinfo ENOTFOUND missing.test`,
		`Error | ENOTFOUND | queryA | missing.test | queryA ENOTFOUND missing.test`,
		`Error | ENODATA | queryCname | example.test | queryCname ENODATA example.test`,
		`Error | EAI_AGAIN | getaddrinfo | flaky.test | getaddrinfo EAI_AGAIN flaky.test`,
		`Error | EREFUSED | queryA | secret.example.test | queryA EREFUSED secret.example.test`,
		`Error | EINVAL | getHostByAddr | not-an-ip | getHostByAddr EINVAL not-an-ip`,
		`Error | ENOTIMP | querySoa | example.test | querySoa ENOTIMP example.test`,
		`Error | ENOTIMP | queryA | example.test | queryA ENOTIMP example.test`,
		`TypeError ERR_INVALID_ARG_TYPE`,
		`TypeError ERR_INVALID_ARG_VALUE`,
		`TypeError ERR_INVALID_ARG_VALUE`,
		`RangeError ERR_SOCKET_BAD_PORT`,
		`Error ERR_ACCESS_DENIED`,
		`TypeError ERR_INVALID_ARG_TYPE`,
		`Error | ECANCELLED | queryA | slow.test | queryA ECANCELLED slow.test`,
		`cb null 192.0.2.1 4`,
		`cb all 2`,
		`cb mx [{"exchange":"mail.example.test","priority":10}]`,
		`cb service example.test 22`,
		`cb err ENOTFOUND`,
		`cb resolver example.test`,
		`ipv6first {"address":"2001:db8::1","family":6}`,
	}, "\n")
	if out != want {
		t.Errorf("got\n%s\nwant\n%s\nstderr: %s", out, want, errOut)
	}
}

// TestDNSIsACapability pins that the dns modules are there only when the
// host gives them, and that without a loop a query is answered before the
// call returns.
func TestDNSIsACapability(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := stdlib.Install(rt, stdlib.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.EvalModule("main.js", `import "dns"`); err == nil {
		t.Error("importing dns without the capability should have failed")
	}

	rt2 := quickjs.New()
	defer rt2.Close()
	if err := stdlib.Names(rt2, &stdlib.DNS{Resolver: fakeResolver{}, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	v, err := rt2.EvalModule("main.js", `import {promises} from "dns"; export default await promises.resolve4("example.test")`)
	if err != nil {
		t.Fatal(err)
	}
	def, err := v.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if got := def.String(); got != "192.0.2.1" {
		t.Errorf("without a loop: got %q", got)
	}
}
