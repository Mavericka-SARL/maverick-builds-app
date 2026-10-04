// A calculated member's formula — Variance = {RF} - {LY} — evaluated in the
// browser, for the totals the planning grid works out itself (a FY Variance %
// from the FY RF and LY). It implements exactly the subset the server lets a
// member formula use (internal/metricformula ValidateMemberFormula) with the
// server's semantics (internal/rollup EvalCalculated): + - * / and
// comparisons, member codes as names (bare or in braces, case-insensitive),
// IF, ABS, MIN, MAX, ROUND, AND, OR, NOT and METRICFORMAT(). A member with no
// value reads 0; when none of the members read has a value there is none; an
// error (a division by zero) is no value.

type Val = number | string | boolean;
type Node =
  | { k: "num"; v: number }
  | { k: "str"; v: string }
  | { k: "bool"; v: boolean }
  | { k: "ref"; name: string }
  | { k: "neg"; e: Node }
  | { k: "bin"; op: string; l: Node; r: Node }
  | { k: "call"; name: string; args: Node[] };

class FormulaError extends Error {}

function tokenize(src: string): string[] {
  const out: string[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (/\s/.test(c)) { i++; continue; }
    if (c === "{") {
      const j = src.indexOf("}", i);
      if (j < 0) throw new FormulaError("no closing brace");
      out.push("{" + src.slice(i + 1, j));
      i = j + 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1, s = "";
      while (j < src.length) {
        if (src[j] === '"') {
          if (src[j + 1] === '"') { s += '"'; j += 2; continue; }
          break;
        }
        s += src[j++];
      }
      out.push('"' + s);
      i = j + 1;
      continue;
    }
    const two = src.slice(i, i + 2);
    if (two === "<>" || two === "<=" || two === ">=") { out.push(two); i += 2; continue; }
    if ("+-*/(),=<>".includes(c)) { out.push(c); i++; continue; }
    const m = /^(\d+(\.\d+)?([eE][+-]?\d+)?|\.\d+|[A-Za-z_][A-Za-z0-9_.]*)/.exec(src.slice(i));
    if (!m) throw new FormulaError(`unexpected ${c}`);
    out.push(m[0]);
    i += m[0].length;
  }
  return out;
}

function parse(src: string): Node {
  const t = tokenize(src.trim().replace(/^=/, ""));
  let p = 0;
  const peek = () => t[p];
  const take = () => t[p++];
  const expect = (s: string) => { if (take() !== s) throw new FormulaError(`expected ${s}`); };
  const comparison = (): Node => {
    let l = addSub();
    while (["=", "<>", "<", "<=", ">", ">="].includes(peek())) { const op = take(); l = { k: "bin", op, l, r: addSub() }; }
    return l;
  };
  const addSub = (): Node => {
    let l = mulDiv();
    while (peek() === "+" || peek() === "-") { const op = take(); l = { k: "bin", op, l, r: mulDiv() }; }
    return l;
  };
  const mulDiv = (): Node => {
    let l = unary();
    while (peek() === "*" || peek() === "/") { const op = take(); l = { k: "bin", op, l, r: unary() }; }
    return l;
  };
  const unary = (): Node => {
    if (peek() === "-") { take(); return { k: "neg", e: unary() }; }
    if (peek() === "+") { take(); return unary(); }
    return primary();
  };
  const primary = (): Node => {
    const tok = take();
    if (tok === undefined) throw new FormulaError("unexpected end");
    if (tok === "(") { const e = comparison(); expect(")"); return e; }
    if (tok[0] === '"') return { k: "str", v: tok.slice(1) };
    if (tok[0] === "{") return { k: "ref", name: tok.slice(1) };
    if (/^[\d.]/.test(tok)) return { k: "num", v: Number(tok) };
    const up = tok.toUpperCase();
    if (up === "TRUE" || up === "FALSE") return { k: "bool", v: up === "TRUE" };
    if (peek() === "(") {
      take();
      const args: Node[] = [];
      if (peek() !== ")") {
        args.push(comparison());
        while (peek() === ",") { take(); args.push(comparison()); }
      }
      expect(")");
      return { k: "call", name: up, args };
    }
    return { k: "ref", name: tok };
  };
  const node = comparison();
  if (p !== t.length) throw new FormulaError("unexpected " + t[p]);
  return node;
}

function refsOf(n: Node, out: Set<string>): Set<string> {
  switch (n.k) {
    case "ref": out.add(n.name.toUpperCase()); break;
    case "neg": refsOf(n.e, out); break;
    case "bin": refsOf(n.l, out); refsOf(n.r, out); break;
    case "call": n.args.forEach(a => refsOf(a, out)); break;
  }
  return out;
}

const num = (v: Val): number => {
  if (typeof v === "number") return v;
  if (typeof v === "boolean") return v ? 1 : 0;
  const n = Number(v);
  if (v.trim() === "" || Number.isNaN(n)) throw new FormulaError("not a number");
  return n;
};
const truthy = (v: Val): boolean => (typeof v === "string" ? v.toUpperCase() === "TRUE" || (v !== "" && v !== "0" && !Number.isNaN(Number(v)) && Number(v) !== 0) : num(v) !== 0);

function compare(a: Val, b: Val): number {
  if (typeof a !== "string" && typeof b !== "string") return num(a) - num(b);
  const as = String(a).toUpperCase(), bs = String(b).toUpperCase();
  return as < bs ? -1 : as > bs ? 1 : 0;
}

function roundHalfAway(n: number, places: number): number {
  const f = Math.pow(10, Math.trunc(places));
  return (Math.sign(n) * Math.round(Math.abs(n) * f + 1e-9)) / f;
}

function evalNode(n: Node, vars: Map<string, number>, metricFormat: string): Val {
  switch (n.k) {
    case "num": case "str": case "bool": return n.v;
    case "ref": return vars.get(n.name.toUpperCase()) ?? 0;
    case "neg": return -num(evalNode(n.e, vars, metricFormat));
    case "bin": {
      const l = evalNode(n.l, vars, metricFormat), r = evalNode(n.r, vars, metricFormat);
      switch (n.op) {
        case "+": return num(l) + num(r);
        case "-": return num(l) - num(r);
        case "*": return num(l) * num(r);
        case "/": { const d = num(r); if (d === 0) throw new FormulaError("#DIV/0!"); return num(l) / d; }
        case "=": return compare(l, r) === 0;
        case "<>": return compare(l, r) !== 0;
        case "<": return compare(l, r) < 0;
        case "<=": return compare(l, r) <= 0;
        case ">": return compare(l, r) > 0;
        case ">=": return compare(l, r) >= 0;
      }
      throw new FormulaError(n.op);
    }
    case "call": {
      const a = (i: number) => evalNode(n.args[i], vars, metricFormat);
      switch (n.name) {
        case "IF": return truthy(a(0)) ? a(1) : (n.args.length > 2 ? a(2) : false);
        case "ABS": return Math.abs(num(a(0)));
        case "MIN": return Math.min(...n.args.map((_, i) => num(a(i))));
        case "MAX": return Math.max(...n.args.map((_, i) => num(a(i))));
        case "ROUND": return roundHalfAway(num(a(0)), num(a(1)));
        case "AND": return n.args.every((_, i) => truthy(a(i)));
        case "OR": return n.args.some((_, i) => truthy(a(i)));
        case "NOT": return !truthy(a(0));
        case "METRICFORMAT": return metricFormat;
      }
      throw new FormulaError(n.name);
    }
  }
}

const parsed = new Map<string, Node | null>();

/** The member codes a member formula reads, upper-cased. */
export function memberFormulaRefs(text: string): string[] {
  let node = parsed.get(text);
  if (node === undefined) {
    try { node = parse(text); } catch { node = null; }
    parsed.set(text, node);
  }
  return node ? [...refsOf(node, new Set())] : [];
}

/**
 * Evaluates a calculated member's formula for one metric at one coordinate.
 * value(code) is the metric at the same coordinate with the dimension at that
 * member: a number, undefined (no value) or null (withheld — the result is
 * withheld too, so nothing partial is shown).
 */
export function evalMemberFormula(
  text: string,
  metricFormat: string,
  value: (code: string) => number | null | undefined,
): number | null | undefined {
  let node = parsed.get(text);
  if (node === undefined) {
    try { node = parse(text); } catch { node = null; }
    parsed.set(text, node);
  }
  if (!node) return undefined;
  const refs = [...refsOf(node, new Set())];
  const vars = new Map<string, number>();
  let any = false;
  for (const ref of refs) {
    const v = value(ref);
    if (v === null) return null;
    if (v !== undefined) { any = true; vars.set(ref, v); }
  }
  if (refs.length > 0 && !any) return undefined;
  try {
    const r = evalNode(node, vars, metricFormat);
    return typeof r === "number" && Number.isFinite(r) ? r : typeof r === "boolean" ? (r ? 1 : 0) : undefined;
  } catch {
    return undefined;
  }
}
