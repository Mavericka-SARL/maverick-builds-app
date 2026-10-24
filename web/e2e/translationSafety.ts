// Finds the JSX a page translator breaks, and with --fix keeps that text in
// <Txt> (src/ui/Txt.tsx). A translator (a browser's "translate this page")
// swaps each text node for <font> elements; React still holds the text node
// it made and fails the page — "Failed to execute 'insertBefore' on 'Node'"
// or 'removeChild' — when it
//   (A) inserts a node that comes and goes (a condition, a list) before a
//       text node: {saving && <Spinner/>} Save;
//   (B) removes or adds a text node that comes and goes among siblings:
//       {n} row{n === 1 ? "" : "s"} imported.
// A lone text child is the element's content, which React replaces whole:
// it is safe, and so is the same text inside <Txt>.
//
//   node e2e/translationSafety.ts          lists what it finds
//   node e2e/translationSafety.ts --fix    wraps it in <Txt>
//
// translation-safety.spec.ts fails on anything it finds.
import type { Page } from "@playwright/test";
import ts from "typescript";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

export interface UnsafeText {
  file: string; // relative to web/
  line: number;
  kind: "A" | "B";
  source: string;
  // The node to keep in <Txt>: [start, end) in the file's text.
  wrap: [number, number];
}

const webRoot = join(dirname(fileURLToPath(import.meta.url)), "..");

// What a browser's "translate this page" does (Google's, Yandex's): every
// text node becomes <font><font>its translation</font></font> — here the
// same words, so a test can still find the buttons by name.
export async function translatePage(page: Page): Promise<void> {
  await page.evaluate(() => {
    const walker = document.createTreeWalker(document.getElementById("root")!, NodeFilter.SHOW_TEXT);
    const nodes: Text[] = [];
    while (walker.nextNode()) if (walker.currentNode.nodeValue?.trim()) nodes.push(walker.currentNode as Text);
    for (const t of nodes) {
      const outer = document.createElement("font"), inner = document.createElement("font");
      inner.textContent = t.nodeValue;
      outer.appendChild(inner);
      t.replaceWith(outer);
    }
  });
}

type Branch = { text: boolean; none: boolean; element: boolean };
type Kind = "skip" | "statictext" | "text" | "condtext" | "dynamic" | "element";

export function findUnsafeText(): UnsafeText[] {
  const cfgPath = join(webRoot, "tsconfig.app.json");
  const cfg = ts.getParsedCommandLineOfConfigFile(cfgPath, {}, { ...ts.sys, onUnRecoverableConfigFileDiagnostic: () => {} });
  if (!cfg) throw new Error("tsconfig.app.json could not be read");
  const program = ts.createProgram(cfg.fileNames, cfg.options);
  const checker = program.getTypeChecker();

  const strip = (e: ts.Expression): ts.Expression => {
    while (ts.isParenthesizedExpression(e) || ts.isAsExpression(e) || ts.isNonNullExpression(e)) e = e.expression;
    return e;
  };
  const parts = (t: ts.Type): ts.Type[] => (t.isUnion() ? t.types : [t]);
  // What a type renders as: a text node, nothing (null, undefined, false,
  // ""), an element — or text and element both, a ReactNode its caller fills.
  const kindOfType = (t: ts.Type): Branch => {
    const b = { text: false, none: false, element: false };
    for (const p of parts(t)) {
      if (p.flags & ts.TypeFlags.StringLiteral) {
        if ((p as ts.StringLiteralType).value === "") b.none = true;
        else b.text = true;
      } else if (p.flags & (ts.TypeFlags.String | ts.TypeFlags.TemplateLiteral | ts.TypeFlags.NumberLike | ts.TypeFlags.BigIntLike)) b.text = true;
      else if (p.flags & (ts.TypeFlags.Null | ts.TypeFlags.Undefined | ts.TypeFlags.BooleanLike | ts.TypeFlags.Void | ts.TypeFlags.Never)) b.none = true;
      else b.element = true;
    }
    return b;
  };
  const branches = (expr: ts.Expression): Branch[] => {
    const e = strip(expr);
    if (ts.isConditionalExpression(e)) return [...branches(e.whenTrue), ...branches(e.whenFalse)];
    if (ts.isBinaryExpression(e)) {
      const op = e.operatorToken.kind;
      if (op === ts.SyntaxKind.AmpersandAmpersandToken) {
        // A falsy left side renders itself: nothing, or 0 — a text node,
        // counted for a number alone (a ReactNode is its caller's business).
        const l = kindOfType(checker.getTypeAtLocation(e.left));
        const numeric = parts(checker.getTypeAtLocation(e.left)).some((p) => p.flags & ts.TypeFlags.NumberLike);
        return [{ text: l.text && !l.element && numeric, none: true, element: false }, ...branches(e.right)];
      }
      if (op === ts.SyntaxKind.BarBarToken || op === ts.SyntaxKind.QuestionQuestionToken) {
        // The left side renders only when it is something.
        return [...branches(e.left).map((b) => ({ ...b, none: false })), ...branches(e.right)];
      }
    }
    if (ts.isStringLiteral(e) || ts.isNoSubstitutionTemplateLiteral(e)) return [{ text: e.text !== "", none: e.text === "", element: false }];
    if (ts.isTemplateExpression(e)) return [{ text: true, none: false, element: false }];
    if (ts.isJsxElement(e) || ts.isJsxSelfClosingElement(e) || ts.isJsxFragment(e)) return [{ text: false, none: false, element: true }];
    return [kindOfType(checker.getTypeAtLocation(e))];
  };
  const isList = (expr: ts.Expression): boolean => {
    const e = strip(expr);
    if (ts.isCallExpression(e) && ts.isPropertyAccessExpression(e.expression) && ["map", "flatMap", "filter"].includes(e.expression.name.text)) return true;
    return parts(checker.getTypeAtLocation(e)).some((t) => checker.isArrayType(t));
  };
  const classify = (child: ts.JsxChild): Kind => {
    if (ts.isJsxText(child)) return child.text.trim() === "" ? "skip" : "statictext";
    if (!ts.isJsxExpression(child)) return "element";
    const e = child.expression;
    if (!e) return "skip"; // a comment
    if (isList(e)) return "dynamic";
    const bs = branches(e);
    if (bs.some((b) => b.text && b.element)) return "element";
    const text = bs.some((b) => b.text), none = bs.some((b) => b.none), element = bs.some((b) => b.element);
    if (text && (none || element)) return "condtext";
    if (text) return "text";
    if (none || bs.length > 1) return "dynamic";
    return "element";
  };

  const found: UnsafeText[] = [];
  const seen = new Set<string>();
  const add = (sf: ts.SourceFile, kind: "A" | "B", node: ts.JsxChild, source: string) => {
    const wrap: [number, number] = [node.getStart(sf), node.getEnd()];
    const file = relative(webRoot, sf.fileName);
    if (seen.has(`${file}:${wrap[0]}`)) return;
    seen.add(`${file}:${wrap[0]}`);
    found.push({ file, line: sf.getLineAndCharacterOfPosition(wrap[0]).line + 1, kind, source: source.replace(/\s+/g, " ").slice(0, 120), wrap });
  };
  const visit = (children: ts.NodeArray<ts.JsxChild>, sf: ts.SourceFile) => {
    const list = children.filter((c) => classify(c) !== "skip");
    const kinds = list.map(classify);
    if (list.length < 2) return;
    for (let i = 0; i < list.length; i++) {
      if (kinds[i] === "condtext") add(sf, "B", list[i], list[i].getText(sf));
      const next = list[i + 1];
      if ((kinds[i] === "dynamic" || kinds[i] === "condtext") && next && (kinds[i + 1] === "statictext" || kinds[i + 1] === "text")) {
        add(sf, "A", next, `${list[i].getText(sf).slice(0, 60)} ⟶ ${next.getText(sf)}`);
      }
    }
  };
  for (const sf of program.getSourceFiles()) {
    if (sf.isDeclarationFile || !sf.fileName.startsWith(join(webRoot, "src") + "/") || !sf.fileName.endsWith(".tsx")) continue;
    const walk = (n: ts.Node) => {
      if (ts.isJsxElement(n) || ts.isJsxFragment(n)) visit(n.children, sf);
      ts.forEachChild(n, walk);
    };
    walk(sf);
  }
  return found;
}

// Keeps each found node in <Txt>, a static text's line breaks left outside
// it (as JSX drops them), and imports Txt.
function fix(found: UnsafeText[]): void {
  const byFile = new Map<string, UnsafeText[]>();
  for (const f of found) byFile.set(f.file, [...(byFile.get(f.file) ?? []), f]);
  for (const [file, hits] of byFile) {
    const path = join(webRoot, file);
    let text = readFileSync(path, "utf8");
    const inserts: { at: number; s: string; order: number }[] = [];
    for (const h of hits) {
      let [start, end] = h.wrap;
      const raw = text.slice(start, end);
      if (!raw.startsWith("{")) {
        const lead = raw.length - raw.trimStart().length, trail = raw.length - raw.trimEnd().length;
        if (raw.slice(0, lead).includes("\n")) start += lead;
        if (raw.slice(raw.length - trail).includes("\n")) end -= trail;
      }
      inserts.push({ at: start, s: "<Txt>", order: 1 }, { at: end, s: "</Txt>", order: 0 });
    }
    // From the end, so earlier offsets hold; where one node ends as the next
    // begins, "</Txt>" goes before "<Txt>" (each insert lands before the
    // last one made at that offset).
    inserts.sort((a, b) => b.at - a.at || b.order - a.order);
    for (const ins of inserts) text = text.slice(0, ins.at) + ins.s + text.slice(ins.at);
    if (!/import \{[^}]*\bTxt\b[^}]*\} from/.test(text)) {
      const uiDir = join(webRoot, "src/ui");
      const fromDir = dirname(path);
      const rel = relative(fromDir, uiDir);
      const spec = fromDir === uiDir ? "./Txt" : rel.startsWith(".") ? rel : `./${rel}`;
      const existing = new RegExp(`import \\{([^}]*)\\} from "${spec.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&")}";`);
      if (spec !== "./Txt" && existing.test(text)) {
        // Txt joins the names: last on its own line in a list of lines.
        text = text.replace(existing, (_m, names: string) => {
          const body = names.replace(/\s*$/, "");
          return names.includes("\n")
            ? `import {${body}${body.endsWith(",") ? "" : ","}\n  Txt,\n} from "${spec}";`
            : `import {${body.replace(/,$/, "")}, Txt } from "${spec}";`;
        });
      } else {
        const imports = [...text.matchAll(/^import [^;]+;$/gms)];
        const at = imports.length ? imports[imports.length - 1].index! + imports[imports.length - 1][0].length : 0;
        text = text.slice(0, at) + `\nimport { Txt } from "${spec}";` + text.slice(at);
      }
    }
    writeFileSync(path, text);
  }
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const found = findUnsafeText();
  for (const f of found) console.log(`${f.kind} ${f.file}:${f.line}  ${f.source}`);
  console.error(`${found.length} place(s)`);
  if (process.argv.includes("--fix")) fix(found);
}
