package stdlib

import (
	quickjs "github.com/go-quickjs/go-quickjs"
)

// Locales installs Intl, and points the toLocale methods at it.
//
// What is here is what can be had without a copy of the Unicode locale data,
// which is tens of megabytes and the reason engines that have Intl are large:
// the formats are English, and what varies by language is the part that is
// small and matters most -- how a number is grouped and where its decimal
// point goes, which is wrong often enough to be worth getting right.
//
//	new Intl.NumberFormat("de-DE").format(1234.5)          // 1.234,5
//	new Intl.NumberFormat("en", {style: "currency", currency: "USD"}).format(9.5)
//	new Intl.DateTimeFormat("en", {dateStyle: "long", timeZone: "UTC"}).format(d)
//
// A host that needs real locale data should install its own Intl before this
// one; nothing here replaces an Intl that is already there. A time zone other
// than UTC or the machine's own is refused rather than guessed at, since a
// wrong time is worse than a missing one.
func Locales(rt *quickjs.Runtime) error {
	// A host that has its own is left alone.
	existing, err := rt.Eval(`typeof Intl`)
	if err != nil {
		return err
	}
	if existing.String() != "undefined" {
		return nil
	}
	api, err := evalWithHost(rt, "<intl>", intlJS, quickjs.Value{})
	if err != nil {
		return err
	}
	intl, err := api.Get("Intl")
	if err != nil {
		return err
	}
	return rt.Set("Intl", intl)
}

// intlJS is the whole of it: the four formats that are asked for most, and the
// three smaller ones that are easy once the plural rules are there.
const intlJS = `(function () {
  "use strict";

  // --- locales ---------------------------------------------------------------

  // What a language does to a number. These are the conventions, not the
  // language: the names of months and days are English below, whatever locale
  // is asked for, and resolvedOptions says so.
  const conventions = {
    root: {group: ",", decimal: "."},
    de: {group: ".", decimal: ","},
    es: {group: ".", decimal: ","},
    it: {group: ".", decimal: ","},
    nl: {group: ".", decimal: ","},
    pt: {group: ".", decimal: ","},
    id: {group: ".", decimal: ","},
    tr: {group: ".", decimal: ","},
    da: {group: ".", decimal: ","},
    el: {group: ".", decimal: ","},
    ro: {group: ".", decimal: ","},
    vi: {group: ".", decimal: ","},
    fr: {group: " ", decimal: ","},
    ru: {group: " ", decimal: ","},
    pl: {group: " ", decimal: ","},
    cs: {group: " ", decimal: ","},
    sk: {group: " ", decimal: ","},
    uk: {group: " ", decimal: ","},
    sv: {group: " ", decimal: ","},
    nb: {group: " ", decimal: ","},
    fi: {group: " ", decimal: ","},
    hu: {group: " ", decimal: ","},
    bg: {group: " ", decimal: ","},
    lv: {group: " ", decimal: ","},
    lt: {group: " ", decimal: ","},
    "de-CH": {group: "’", decimal: "."},
    "en-IN": {group: ",", decimal: ".", indian: true},
    "hi-IN": {group: ",", decimal: ".", indian: true},
  };

  // canonical is as much of a language tag as this understands: the language,
  // and the region where it changes the answer.
  function canonical(locales) {
    const first = Array.isArray(locales) ? locales[0] : locales;
    if (first === undefined) return "en-US";
    const tag = String(first.constructor === Object ? first.baseName || "en-US" : first);
    if (!/^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$/.test(tag)) {
      throw new RangeError("that is not a language tag: " + tag);
    }
    const parts = tag.split("-");
    const language = parts[0].toLowerCase();
    const region = parts.slice(1).find(p => /^([A-Za-z]{2}|\d{3})$/.test(p));
    return region ? language + "-" + region.toUpperCase() : language;
  }

  function conventionFor(locale) {
    return conventions[locale] || conventions[locale.split("-")[0]] || conventions.root;
  }

  // --- NumberFormat ----------------------------------------------------------

  const currencySymbols = {
    USD: "$", EUR: "€", GBP: "£", JPY: "¥", CNY: "CN¥", KRW: "₩", INR: "₹",
    RUB: "₽", BRL: "R$", CAD: "CA$", AUD: "A$", NZD: "NZ$", MXN: "MX$",
    CHF: "CHF", SEK: "SEK", NOK: "NOK", DKK: "DKK", PLN: "PLN", TRY: "₺",
    ILS: "₪", THB: "THB", VND: "₫", NGN: "₦", ZAR: "ZAR", PHP: "₱", UAH: "₴",
  };
  // The currencies that are not written with two decimals.
  const currencyDigits = {JPY: 0, KRW: 0, VND: 0, CLP: 0, ISK: 0, HUF: 0,
                          BHD: 3, JOD: 3, KWD: 3, OMR: 3, TND: 3};

  class NumberFormat {
    constructor(locales, options = {}) {
      if (!new.target) return new NumberFormat(locales, options);
      const locale = canonical(locales);
      const style = options.style || "decimal";
      if (style === "currency" && !options.currency) {
        throw new TypeError("a currency style needs a currency");
      }
      const currency = options.currency ? String(options.currency).toUpperCase() : undefined;
      const digits = currency !== undefined && currency in currencyDigits
        ? currencyDigits[currency] : 2;
      const resolved = {
        locale, style, currency,
        currencyDisplay: options.currencyDisplay || "symbol",
        unit: options.unit,
        notation: options.notation || "standard",
        signDisplay: options.signDisplay || "auto",
        useGrouping: options.useGrouping === undefined ? true : options.useGrouping,
        minimumIntegerDigits: options.minimumIntegerDigits === undefined
          ? 1 : Number(options.minimumIntegerDigits),
        minimumFractionDigits: options.minimumFractionDigits === undefined
          ? (style === "currency" ? digits : 0) : Number(options.minimumFractionDigits),
        maximumFractionDigits: undefined,
        numberingSystem: "latn",
      };
      if (options.maximumFractionDigits !== undefined) {
        resolved.maximumFractionDigits = Number(options.maximumFractionDigits);
      } else if (style === "currency") {
        resolved.maximumFractionDigits = Math.max(resolved.minimumFractionDigits, digits);
      } else if (style === "percent") {
        resolved.maximumFractionDigits = Math.max(resolved.minimumFractionDigits, 0);
      } else {
        resolved.maximumFractionDigits = Math.max(resolved.minimumFractionDigits, 3);
      }
      if (resolved.maximumFractionDigits < resolved.minimumFractionDigits) {
        throw new RangeError("the fraction digits are the wrong way round");
      }
      Object.defineProperty(this, "_o", {value: resolved});
      Object.defineProperty(this, "_c", {value: conventionFor(locale)});
    }

    resolvedOptions() { return {...this._o}; }

    format(value) {
      return this.formatToParts(value).map(p => p.value).join("");
    }

    formatToParts(value) {
      const o = this._o;
      let n = Number(value);
      if (Number.isNaN(n)) return [{type: "nan", value: "NaN"}];
      if (!Number.isFinite(n)) {
        const parts = [];
        if (n < 0) parts.push({type: "minusSign", value: "-"});
        parts.push({type: "infinity", value: "∞"});
        return parts;
      }
      if (o.style === "percent") n *= 100;

      const negative = n < 0 || Object.is(n, -0);
      n = Math.abs(n);

      let suffix = "";
      if (o.notation === "compact") {
        // The short forms English uses, which is what compact notation is
        // nearly always asked for.
        const steps = [[1e12, "T"], [1e9, "B"], [1e6, "M"], [1e3, "K"]];
        for (const [size, letter] of steps) {
          if (n >= size) {
            n = n / size;
            suffix = letter;
            break;
          }
        }
      }

      const min = suffix ? 0 : o.minimumFractionDigits;
      const max = suffix ? (n < 10 ? 1 : 0) : o.maximumFractionDigits;
      let text = n.toFixed(max);
      if (max > min) {
        // Trailing zeros beyond the minimum are not written.
        text = text.replace(/(\.\d*?)0+$/, "$1").replace(/\.$/, "");
        const dot = text.indexOf(".");
        const have = dot < 0 ? 0 : text.length - dot - 1;
        if (have < min) text = n.toFixed(min);
      }

      let [whole, fraction] = text.split(".");
      while (whole.length < o.minimumIntegerDigits) whole = "0" + whole;

      const parts = [];
      const sign = negative ? "-" : "+";
      const wantsSign = o.signDisplay === "always" ||
        (o.signDisplay === "exceptZero" && n !== 0) ||
        (negative && o.signDisplay !== "never");
      if (wantsSign && o.signDisplay !== "never") {
        parts.push({type: negative ? "minusSign" : "plusSign", value: sign});
      }
      if (o.style === "currency" && o.currencyDisplay !== "none") {
        parts.push({type: "currency", value: currencyText(o)});
      }
      for (const piece of groupWhole(whole, this._c, o.useGrouping && !suffix)) {
        parts.push(piece);
      }
      if (fraction !== undefined && fraction !== "") {
        parts.push({type: "decimal", value: this._c.decimal});
        parts.push({type: "fraction", value: fraction});
      }
      if (suffix) parts.push({type: "compact", value: suffix});
      if (o.style === "percent") parts.push({type: "percentSign", value: "%"});
      if (o.style === "unit" && o.unit) {
        parts.push({type: "literal", value: " "});
        parts.push({type: "unit", value: String(o.unit)});
      }
      return parts;
    }
  }

  function currencyText(o) {
    if (o.currencyDisplay === "code") return o.currency;
    if (o.currencyDisplay === "name") return o.currency;
    return currencySymbols[o.currency] || o.currency;
  }

  // groupWhole splits the integer part where the separators go: in threes,
  // except where a language counts differently.
  function groupWhole(whole, convention, grouping) {
    if (!grouping || whole.length < 4) return [{type: "integer", value: whole}];
    const parts = [];
    let pieces;
    if (convention.indian) {
      // The last three digits, then twos: 1,23,45,678.
      const tail = whole.slice(-3);
      const head = whole.slice(0, -3);
      pieces = [];
      for (let i = head.length; i > 0; i -= 2) {
        pieces.unshift(head.slice(Math.max(0, i - 2), i));
      }
      pieces.push(tail);
    } else {
      pieces = [];
      for (let i = whole.length; i > 0; i -= 3) {
        pieces.unshift(whole.slice(Math.max(0, i - 3), i));
      }
    }
    pieces.forEach((piece, i) => {
      if (i > 0) parts.push({type: "group", value: convention.group});
      parts.push({type: "integer", value: piece});
    });
    return parts;
  }

  // --- DateTimeFormat --------------------------------------------------------

  const months = ["January", "February", "March", "April", "May", "June", "July",
                  "August", "September", "October", "November", "December"];
  const days = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday",
                "Saturday"];

  const localZone = (() => {
    // The machine's own zone has no name here, only an offset, and the offset
    // is what a format needs.
    try { return new Date().getTimezoneOffset(); } catch (e) { return 0; }
  })();

  class DateTimeFormat {
    constructor(locales, options = {}) {
      if (!new.target) return new DateTimeFormat(locales, options);
      const locale = canonical(locales);
      const zone = options.timeZone === undefined ? undefined : String(options.timeZone);
      if (zone !== undefined && !/^(UTC|GMT|Etc\/(UTC|GMT|Greenwich))$/i.test(zone)) {
        throw new RangeError(
          "this runtime knows only UTC and the machine's own zone, not " + zone);
      }
      const o = {
        locale,
        calendar: "gregory",
        numberingSystem: "latn",
        timeZone: zone === undefined ? "system" : "UTC",
        hour12: options.hour12 === undefined ? undefined : !!options.hour12,
      };
      // dateStyle and timeStyle are shorthands for a set of fields.
      if (options.dateStyle || options.timeStyle) {
        o.dateStyle = options.dateStyle;
        o.timeStyle = options.timeStyle;
        if (options.dateStyle) {
          o.weekday = options.dateStyle === "full" ? "long" : undefined;
          o.year = "numeric";
          o.month = {full: "long", long: "long", medium: "short", short: "numeric"}[
            options.dateStyle];
          o.day = "numeric";
        }
        if (options.timeStyle) {
          o.hour = "numeric";
          o.minute = "2-digit";
          if (options.timeStyle !== "short") o.second = "2-digit";
          if (options.timeStyle === "full" || options.timeStyle === "long") {
            o.timeZoneName = "short";
          }
        }
      } else {
        for (const field of ["weekday", "era", "year", "month", "day", "hour",
                             "minute", "second", "timeZoneName"]) {
          if (options[field] !== undefined) o[field] = options[field];
        }
        // With nothing asked for, a date is a date.
        if (o.year === undefined && o.month === undefined && o.day === undefined &&
            o.hour === undefined && o.minute === undefined && o.second === undefined &&
            o.weekday === undefined) {
          o.year = "numeric";
          o.month = "numeric";
          o.day = "numeric";
        }
      }
      if (o.hour12 === undefined) o.hour12 = o.hour !== undefined;
      Object.defineProperty(this, "_o", {value: o});
    }

    resolvedOptions() {
      const out = {...this._o};
      if (out.timeZone === "system") out.timeZone = "system";
      return out;
    }

    format(value) { return this.formatToParts(value).map(p => p.value).join(""); }

    formatToParts(value) {
      const o = this._o;
      const date = value === undefined ? new Date() : new Date(value);
      if (Number.isNaN(date.getTime())) throw new RangeError("that is not a date");
      const utc = o.timeZone === "UTC";
      const get = {
        year: utc ? date.getUTCFullYear() : date.getFullYear(),
        month: (utc ? date.getUTCMonth() : date.getMonth()),
        day: utc ? date.getUTCDate() : date.getDate(),
        weekday: utc ? date.getUTCDay() : date.getDay(),
        hour: utc ? date.getUTCHours() : date.getHours(),
        minute: utc ? date.getUTCMinutes() : date.getMinutes(),
        second: utc ? date.getUTCSeconds() : date.getSeconds(),
      };

      const parts = [];
      const pad = (n) => String(n).padStart(2, "0");
      const push = (type, text) => parts.push({type, value: text});
      const literal = (text) => parts.push({type: "literal", value: text});

      const hasDate = o.weekday || o.year || o.month || o.day;
      if (o.weekday) {
        const name = days[get.weekday];
        push("weekday", o.weekday === "short" ? name.slice(0, 3)
          : o.weekday === "narrow" ? name.slice(0, 1) : name);
        if (o.year || o.month || o.day) literal(", ");
      }
      const wordMonth = o.month === "long" || o.month === "short" || o.month === "narrow";
      if (wordMonth) {
        // The order English puts them in: January 2, 2020.
        const name = months[get.month];
        push("month", o.month === "short" ? name.slice(0, 3)
          : o.month === "narrow" ? name.slice(0, 1) : name);
        if (o.day) { literal(" "); push("day", String(get.day)); }
        if (o.year) { literal(", "); push("year", yearText(o, get.year)); }
      } else if (o.month || o.day || o.year) {
        // And the order it puts numbers in: 1/2/2020.
        const bits = [];
        if (o.month) bits.push(["month", o.month === "2-digit" ? pad(get.month + 1)
          : String(get.month + 1)]);
        if (o.day) bits.push(["day", o.day === "2-digit" ? pad(get.day) : String(get.day)]);
        if (o.year) bits.push(["year", yearText(o, get.year)]);
        bits.forEach(([type, text], i) => {
          if (i > 0) literal("/");
          push(type, text);
        });
      }

      if (o.hour !== undefined || o.minute !== undefined || o.second !== undefined) {
        if (hasDate) literal(", ");
        let hour = get.hour;
        let suffix = "";
        if (o.hour12) {
          suffix = hour < 12 ? " AM" : " PM";
          hour = hour % 12;
          if (hour === 0) hour = 12;
        }
        if (o.hour !== undefined) {
          push("hour", o.hour === "2-digit" ? pad(hour) : String(hour));
        }
        if (o.minute !== undefined) {
          if (o.hour !== undefined) literal(":");
          push("minute", pad(get.minute));
        }
        if (o.second !== undefined) {
          if (o.hour !== undefined || o.minute !== undefined) literal(":");
          push("second", pad(get.second));
        }
        if (suffix) {
          literal(" ");
          push("dayPeriod", suffix.trim());
        }
      }

      if (o.timeZoneName) {
        literal(" ");
        push("timeZoneName", utc ? "UTC" : offsetName(localZone));
      }
      return parts;
    }
  }

  function yearText(o, year) {
    const text = String(Math.abs(year));
    return o.year === "2-digit" ? text.slice(-2).padStart(2, "0") : text;
  }

  // offsetName is what a zone without a name can be called: GMT+2.
  function offsetName(minutesBehind) {
    const minutes = -minutesBehind;
    const sign = minutes < 0 ? "-" : "+";
    const abs = Math.abs(minutes);
    const hours = Math.floor(abs / 60);
    const rest = abs % 60;
    return "GMT" + sign + hours + (rest ? ":" + String(rest).padStart(2, "0") : "");
  }

  // --- Collator --------------------------------------------------------------

  class Collator {
    constructor(locales, options = {}) {
      if (!new.target) return new Collator(locales, options);
      Object.defineProperty(this, "_o", {value: {
        locale: canonical(locales),
        usage: options.usage || "sort",
        sensitivity: options.sensitivity || "variant",
        numeric: !!options.numeric,
        caseFirst: options.caseFirst || "false",
        ignorePunctuation: !!options.ignorePunctuation,
        collation: "default",
      }});
      const o = this._o;
      // compare is bound, since it is nearly always passed to sort.
      this.compare = (a, b) => compareWith(o, String(a), String(b));
    }
    resolvedOptions() { return {...this._o}; }
  }

  function fold(text, o) {
    let out = text;
    if (o.ignorePunctuation) out = out.replace(/[\s\p{P}\p{S}]/gu, "");
    if (o.sensitivity === "base" || o.sensitivity === "accent") out = out.toLowerCase();
    if (o.sensitivity === "base" || o.sensitivity === "case") {
      // Diacritics are taken off by decomposing and dropping the marks, which
      // is as close to a base letter as this gets.
      out = out.normalize("NFD").replace(/[̀-ͯ]/g, "");
    }
    return out;
  }

  function compareWith(o, a, b) {
    const x = fold(a, o), y = fold(b, o);
    if (o.numeric) {
      // Digits compare as numbers, which is what "file10" after "file9" means.
      const split = /(\d+)/;
      const xs = x.split(split), ys = y.split(split);
      for (let i = 0; i < Math.max(xs.length, ys.length); i++) {
        const p = xs[i] === undefined ? "" : xs[i];
        const q = ys[i] === undefined ? "" : ys[i];
        if (/^\d/.test(p) && /^\d/.test(q)) {
          const n = Number(p), m = Number(q);
          if (n !== m) return n < m ? -1 : 1;
        } else if (p !== q) {
          return p < q ? -1 : 1;
        }
      }
      return 0;
    }
    // Equal once folded means the difference is only in what this sensitivity
    // was told to ignore, which is what makes it equal.
    if (x === y) return 0;
    return x < y ? -1 : 1;
  }

  // --- PluralRules -----------------------------------------------------------

  class PluralRules {
    constructor(locales, options = {}) {
      if (!new.target) return new PluralRules(locales, options);
      Object.defineProperty(this, "_o", {value: {
        locale: canonical(locales),
        type: options.type || "cardinal",
        pluralCategories: options.type === "ordinal"
          ? ["one", "two", "few", "other"] : ["one", "other"],
      }});
    }
    resolvedOptions() { return {...this._o}; }
    select(value) {
      const n = Number(value);
      if (this._o.type === "ordinal") {
        // English: 1st, 2nd, 3rd, 4th, and the teens are all th.
        const tens = Math.abs(n) % 100, ones = Math.abs(n) % 10;
        if (tens >= 11 && tens <= 13) return "other";
        if (ones === 1) return "one";
        if (ones === 2) return "two";
        if (ones === 3) return "few";
        return "other";
      }
      return n === 1 ? "one" : "other";
    }
  }

  // --- ListFormat and RelativeTimeFormat --------------------------------------

  class ListFormat {
    constructor(locales, options = {}) {
      if (!new.target) return new ListFormat(locales, options);
      Object.defineProperty(this, "_o", {value: {
        locale: canonical(locales),
        type: options.type || "conjunction",
        style: options.style || "long",
      }});
    }
    resolvedOptions() { return {...this._o}; }
    format(list) {
      const items = [...list].map(String);
      const word = this._o.type === "disjunction" ? "or"
        : this._o.style === "narrow" ? "" : "and";
      if (items.length === 0) return "";
      if (items.length === 1) return items[0];
      if (items.length === 2) return word ? items.join(" " + word + " ") : items.join(", ");
      const last = items[items.length - 1];
      const head = items.slice(0, -1).join(", ");
      return word ? head + ", " + word + " " + last : head + ", " + last;
    }
    formatToParts(list) {
      return [{type: "literal", value: this.format(list)}];
    }
  }

  const relativeUnits = ["second", "minute", "hour", "day", "week", "month",
                         "quarter", "year"];

  class RelativeTimeFormat {
    constructor(locales, options = {}) {
      if (!new.target) return new RelativeTimeFormat(locales, options);
      Object.defineProperty(this, "_o", {value: {
        locale: canonical(locales),
        numeric: options.numeric || "always",
        style: options.style || "long",
      }});
    }
    resolvedOptions() { return {...this._o}; }
    format(value, unit) {
      const n = Number(value);
      const one = String(unit).replace(/s$/, "");
      if (!relativeUnits.includes(one)) {
        throw new RangeError("that is not a unit of time: " + unit);
      }
      if (this._o.numeric === "auto") {
        const named = {
          "0 day": "today", "1 day": "tomorrow", "-1 day": "yesterday",
          "0 second": "now", "1 week": "next week", "-1 week": "last week",
          "1 month": "next month", "-1 month": "last month",
          "1 year": "next year", "-1 year": "last year",
        }[n + " " + one];
        if (named) return named;
      }
      const plural = Math.abs(n) === 1 ? one : one + "s";
      return n < 0 ? Math.abs(n) + " " + plural + " ago"
                   : "in " + n + " " + plural;
    }
    formatToParts(value, unit) {
      return [{type: "literal", value: this.format(value, unit)}];
    }
  }

  // --- Intl ------------------------------------------------------------------

  const Intl = {
    NumberFormat, DateTimeFormat, Collator, PluralRules, ListFormat,
    RelativeTimeFormat,
    getCanonicalLocales(locales) {
      const list = locales === undefined ? []
        : (Array.isArray(locales) ? locales : [locales]);
      return list.map(canonical);
    },
    supportedValuesOf(key) {
      switch (key) {
        case "currency": return Object.keys(currencySymbols).sort();
        case "timeZone": return ["UTC"];
        case "calendar": return ["gregory"];
        case "numberingSystem": return ["latn"];
        case "collation": return ["default"];
        case "unit": return [];
      }
      throw new RangeError("there is no such key: " + key);
    },
  };

  // Each format also answers supportedLocalesOf, and the answer is honest:
  // the conventions this knows are the ones it can claim to support.
  for (const ctor of [NumberFormat, DateTimeFormat, Collator, PluralRules,
                      ListFormat, RelativeTimeFormat]) {
    ctor.supportedLocalesOf = (locales) => {
      const list = locales === undefined ? []
        : (Array.isArray(locales) ? locales : [locales]);
      return list.filter((tag) => {
        const one = canonical(tag);
        return one.startsWith("en") || conventions[one] ||
               conventions[one.split("-")[0]] !== undefined;
      });
    };
  }

  // The toLocale methods are pointed at this, so that a number formats the way
  // the locale asks rather than the way the engine happens to print it.
  Number.prototype.toLocaleString = function (locales, options) {
    return new NumberFormat(locales, options).format(this.valueOf());
  };
  BigInt.prototype.toLocaleString = function (locales, options) {
    return new NumberFormat(locales, options).format(Number(this.valueOf()));
  };
  Date.prototype.toLocaleString = function (locales, options = {}) {
    return new DateTimeFormat(locales, {
      year: "numeric", month: "numeric", day: "numeric",
      hour: "numeric", minute: "2-digit", second: "2-digit", ...options,
    }).format(this);
  };
  Date.prototype.toLocaleDateString = function (locales, options = {}) {
    return new DateTimeFormat(locales, {
      year: "numeric", month: "numeric", day: "numeric", ...options,
    }).format(this);
  };
  Date.prototype.toLocaleTimeString = function (locales, options = {}) {
    return new DateTimeFormat(locales, {
      hour: "numeric", minute: "2-digit", second: "2-digit", ...options,
    }).format(this);
  };
  String.prototype.localeCompare = function (that, locales, options) {
    return new Collator(locales, options).compare(String(this), String(that));
  };

  return {Intl};
})`
