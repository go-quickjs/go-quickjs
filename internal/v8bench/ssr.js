// The ssr suite's page: a shop's listing, rendered on the server by React 18
// (ReactDOMServer.renderToString), as a host behind a web server renders a
// page for each request. React's UMD builds, fetched into -dir, define React
// and ReactDOMServer; this file is the suite's own, part of the repository.
//
// __ssrRender(n) renders a page of n products and returns its markup; the
// runners check its length (__ssrWant), which React's output for this page
// fixes, so that a wrong render fails the run rather than timing nothing.
(function () {
	"use strict";
	var h = React.createElement;

	function Price(props) {
		var whole = Math.floor(props.cents / 100);
		var part = String(props.cents % 100).padStart(2, "0");
		return h("span", { className: "price" }, props.currency, whole, ".", part);
	}

	function Tags(props) {
		return h("ul", { className: "tags" }, props.tags.map(function (t) {
			return h("li", { key: t, className: "tag tag-" + t }, t);
		}));
	}

	function Rating(props) {
		var items = [];
		for (var i = 0; i < 5; i++) items.push(h("i", { key: i, className: i < props.stars ? "star on" : "star" }));
		return h("div", { className: "rating", title: props.stars + " of 5" }, items);
	}

	function Reviews(props) {
		return h("section", { className: "reviews" }, props.reviews.map(function (r, i) {
			return h("blockquote", { key: i }, h("p", null, r.text), h("cite", null, r.author));
		}));
	}

	function Product(props) {
		var p = props.p;
		return h("article", { className: props.featured ? "product featured" : "product", "data-id": p.id },
			h("header", null,
				h("h2", null, h("a", { href: "/p/" + p.slug }, p.name)),
				p.sale ? h("span", { className: "badge" }, "Sale") : null),
			h("p", { className: "desc" }, p.description),
			h(Price, { cents: p.cents, currency: "$" }),
			h(Rating, { stars: p.stars }),
			h(Tags, { tags: p.tags }),
			p.reviews.length > 0 && h(Reviews, { reviews: p.reviews }));
	}

	function Nav(props) {
		return h("nav", null, props.sections.map(function (s) {
			return h("a", { key: s, href: "/" + s.toLowerCase(), className: s === props.current ? "active" : undefined }, s);
		}));
	}

	function Page(props) {
		var data = props.data;
		return h("html", null,
			h("head", null, h("title", null, data.title)),
			h("body", null,
				h(Nav, { sections: data.sections, current: "Shop" }),
				h("main", null, data.products.map(function (p, i) {
					return h(Product, { key: p.id, p: p, featured: i % 7 === 0 });
				})),
				h("footer", null, "© " + data.year + " " + data.title)));
	}

	var words = "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa".split(" ");

	// makeData is the page's data, made anew for each render, as a request's
	// handler makes it.
	function makeData(n) {
		var products = [];
		for (var i = 0; i < n; i++) {
			var w = function (k) { return words[(i * 7 + k * 3) % words.length]; };
			products.push({
				id: i,
				slug: w(0) + "-" + i,
				name: w(1) + " " + w(2) + " " + i,
				description: [w(3), w(4), w(5), w(6), w(7), w(8)].join(" ") + ".",
				cents: 999 + i * 37,
				stars: i % 6,
				sale: i % 5 === 0,
				tags: [w(9), w(10), w(11)].slice(0, 1 + (i % 3)),
				reviews: i % 3 === 0 ? [{ text: w(12) + " " + w(13), author: w(14) }, { text: w(15) + " " + w(0), author: w(1) }] : []
			});
		}
		return { title: "Shop", year: 2026, sections: ["Home", "Shop", "About", "Contact"], products: products };
	}

	globalThis.__ssrRender = function (n) {
		return ReactDOMServer.renderToString(h(Page, { data: makeData(n) }));
	};

	// __ssrRenderData renders a page of data made elsewhere: the fresh suite's
	// requests.
	globalThis.__ssrRenderData = function (data) {
		return ReactDOMServer.renderToString(h(Page, { data: data }));
	};
})();
