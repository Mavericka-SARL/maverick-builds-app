import { useContext, type ReactNode } from "react";
import { InConsoleLinkContext, isInConsoleHref } from "./RichTextLinks";

/**
 * A small Markdown subset, rendered as React elements.
 *
 * Deliberately not a library and deliberately not HTML: this repository has
 * no `dangerouslySetInnerHTML` anywhere, and text widgets carry whatever a
 * developer typed, so the safe shape is to build elements — a link can only
 * ever become an <a>, never a <script>, whatever the text says.
 *
 * Understood, because it is what explanatory copy needs:
 *   # ## ###   headings
 *   **bold**  *italic*  `code`
 *   [label](https://example.com) and [label](/docs/formulas-manual/manual.html)
 *     — an http(s) link, or any same-origin path, opens in a new tab: the
 *     console has no path routes, so following one in place would unload the
 *     console and lose its state. A bare #fragment stays in place.
 *   [label](dashboard:Model name/Dashboard name) — another dashboard, opened
 *     in place by whoever provides InConsoleLinkContext (RichTextLinks).
 *   [label](page:Developer › Dimensions) — a page of the console's sidebar
 *     (router/pageLinks), followed the same way.
 *     Names may hold spaces, so these two links run to the closing ")".
 *   - bullets, 1. numbers
 *   | a | b |   tables (GitHub's form: a header row, then |---|:---:|---:|
 *     setting each column's alignment, then the rows; \| is a pipe in a cell)
 *   > quote
 *   ---        rule
 *   blank line separates paragraphs; a single newline is a line break.
 * Anything else is shown as written.
 */

// Bold and italic only where the stars hug the words they mark and stand
// outside a word, as Markdown reads them: "a * b * c" and "Price*Qty*12" (a
// formula the AI writes in its reply) stay as written.
const INLINE = /((?<![\w*])\*\*(?=[^\s*])[^*]+?(?<=\S)\*\*(?![\w*])|(?<![\w*])\*(?=[^\s*])[^*]+?(?<=\S)\*(?![\w*])|`[^`]+`|\[[^\]]+\]\((?:dashboard:[^)\n]+|page:[^)\n]+|[^)\s]+)\))/g;

/** The cells of a table row: "| a | b |" → ["a", "b"]; "\|" is a pipe inside a cell. */
function tableCells(line: string): string[] {
  let body = line.trim();
  if (body.startsWith("|")) body = body.slice(1);
  if (body.endsWith("|") && !body.endsWith("\\|")) body = body.slice(0, -1);
  return body.split(/(?<!\\)\|/).map(c => c.trim().replace(/\\\|/g, "|"));
}

const TABLE_DELIMITER = /^\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?$/;

/** Only schemes a browser may follow from a tenant's own text. */
function safeHref(raw: string): string | null {
  const href = raw.trim();
  // "//host" and "/\host" (browsers read "\" as "/") are protocol-relative:
  // they leave the site while looking like a path on it.
  if (/^\/[/\\]/.test(href)) return null;
  if (href.startsWith("/") || href.startsWith("#")) return href;
  if (/^https?:\/\//i.test(href)) return href;
  if (/^mailto:/i.test(href)) return href;
  if (isInConsoleHref(href)) return href;
  return null;
}

const LINK_STYLE: React.CSSProperties = { color: "var(--color-brand-600)", textDecoration: "underline", textUnderlineOffset: 2 };

/** A link that stays in the console: handed to the screen showing the text, never to the browser. */
function InConsoleLink({ href, label }: { href: string; label: string }) {
  const follow = useContext(InConsoleLinkContext);
  return (
    <a
      href="#"
      onClick={e => { e.preventDefault(); follow?.(href); }}
      title={follow ? undefined : "Opens from User › Dashboards"}
      style={{ ...LINK_STYLE, cursor: follow ? "pointer" : "default" }}
    >
      {label}
    </a>
  );
}

/** Links that leave the console open behind them. */
function opensInNewTab(href: string): boolean {
  return /^https?:\/\//i.test(href) || href.startsWith("/");
}

function inline(text: string, keyPrefix: string): ReactNode[] {
  const out: ReactNode[] = [];
  text.split(INLINE).forEach((part, i) => {
    if (!part) return;
    const key = `${keyPrefix}-${i}`;
    // Odd parts are INLINE's matches; the text between is shown as written.
    if (i % 2 === 0) out.push(part);
    // Bold and italic hold links and code of their own: **[Open the plan](page:…)** (2026-11-27).
    else if (part.startsWith("**") && part.endsWith("**")) out.push(<strong key={key}>{inline(part.slice(2, -2), key)}</strong>);
    else if (part.startsWith("`") && part.endsWith("`")) out.push(<code key={key} style={{ fontFamily: "var(--font-mono)", fontSize: "0.92em", background: "var(--color-surface-subtle)", padding: "1px 5px", borderRadius: 4 }}>{part.slice(1, -1)}</code>);
    else if (part.startsWith("*") && part.endsWith("*")) out.push(<em key={key}>{inline(part.slice(1, -1), key)}</em>);
    else if (part.startsWith("[")) {
      const close = part.indexOf("](");
      const label = part.slice(1, close);
      const href = safeHref(part.slice(close + 2, -1));
      out.push(!href
        ? <span key={key}>{label}</span>
        : isInConsoleHref(href)
          ? <InConsoleLink key={key} href={href} label={label} />
          : <a key={key} href={href} target={opensInNewTab(href) ? "_blank" : undefined} rel="noopener noreferrer" style={LINK_STYLE}>{label}</a>);
    } else out.push(part);
  });
  return out;
}

/** One paragraph's text, with single newlines kept as line breaks. */
function lines(text: string, keyPrefix: string): ReactNode[] {
  const out: ReactNode[] = [];
  text.split("\n").forEach((line, i) => {
    if (i > 0) out.push(<br key={`${keyPrefix}-br-${i}`} />);
    out.push(...inline(line, `${keyPrefix}-${i}`));
  });
  return out;
}

export function RichText({ text, style }: { text: string; style?: React.CSSProperties }) {
  const blocks: ReactNode[] = [];
  // Blank lines separate blocks; list items and headings stand alone.
  const src = text.replace(/\r\n/g, "\n").split("\n");
  let para: string[] = [];
  let list: { ordered: boolean; start: number; items: string[] } | null = null;

  const flushPara = () => {
    if (para.length === 0) return;
    const key = `p-${blocks.length}`;
    blocks.push(<p key={key} style={{ margin: "0 0 10px" }}>{lines(para.join("\n"), key)}</p>);
    para = [];
  };
  const flushList = () => {
    if (!list) return;
    const key = `l-${blocks.length}`;
    const items = list.items.map((it, i) => <li key={`${key}-${i}`} style={{ marginBottom: 4 }}>{lines(it, `${key}-${i}`)}</li>);
    // Explicit markers: the global reset (Tailwind preflight) strips them,
    // so a list would otherwise read as indented prose.
    blocks.push(list.ordered
      ? <ol key={key} start={list.start === 1 ? undefined : list.start} style={{ margin: "0 0 10px", paddingLeft: 22, listStyle: "decimal outside" }}>{items}</ol>
      : <ul key={key} style={{ margin: "0 0 10px", paddingLeft: 22, listStyle: "disc outside" }}>{items}</ul>);
    list = null;
  };

  for (let at = 0; at < src.length; at++) {
    const line = src[at].trimEnd();
    // A table: a header row, a delimiter row, then rows while lines start
    // with a pipe. A RACI matrix or a data dictionary in a workbook's notes
    // used to show as its pipes (2026-11-16).
    if (line.trimStart().startsWith("|") && at + 1 < src.length && src[at + 1].includes("|") && TABLE_DELIMITER.test(src[at + 1].trim())) {
      flushPara(); flushList();
      const header = tableCells(line);
      const align = tableCells(src[at + 1]).map(d => d.startsWith(":") && d.endsWith(":") ? "center" : d.endsWith(":") ? "right" : "left") as ("left" | "center" | "right")[];
      const rows: string[][] = [];
      at += 2;
      while (at < src.length && src[at].trimStart().startsWith("|")) rows.push(tableCells(src[at++]));
      at--;
      const key = `t-${blocks.length}`;
      const cell = { padding: "4px 10px", borderBottom: "1px solid var(--color-border)", verticalAlign: "top" as const };
      blocks.push(
        <div key={key} style={{ overflowX: "auto", margin: "0 0 10px" }}>
          <table style={{ borderCollapse: "collapse", fontSize: "0.95em" }}>
            <thead>
              <tr>{header.map((h, i) => <th key={i} style={{ ...cell, textAlign: align[i] ?? "left", fontWeight: 600, background: "var(--color-surface-subtle)" }}>{inline(h, `${key}-h${i}`)}</th>)}</tr>
            </thead>
            <tbody>
              {rows.map((r, ri) => (
                <tr key={ri}>{header.map((_, ci) => <td key={ci} style={{ ...cell, textAlign: align[ci] ?? "left" }}>{inline(r[ci] ?? "", `${key}-${ri}-${ci}`)}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </div>,
      );
      continue;
    }
    const heading = /^(#{1,3})\s+(.*)$/.exec(line);
    const bullet = /^[-*]\s+(.*)$/.exec(line);
    const numbered = /^(\d+)[.)]\s+(.*)$/.exec(line);
    const quote = /^>\s?(.*)$/.exec(line);

    if (line.trim() === "") { flushPara(); flushList(); continue; }
    if (/^(-{3,}|_{3,})$/.test(line.trim())) {
      flushPara(); flushList();
      blocks.push(<hr key={`hr-${blocks.length}`} style={{ border: 0, borderTop: "1px solid var(--color-border)", margin: "12px 0" }} />);
      continue;
    }
    if (heading) {
      flushPara(); flushList();
      const level = heading[1].length;
      const key = `h-${blocks.length}`;
      const sizes = [19, 16, 14];
      const Tag = (["h2", "h3", "h4"] as const)[level - 1];
      blocks.push(<Tag key={key} style={{ fontSize: sizes[level - 1], fontWeight: 700, margin: blocks.length ? "14px 0 6px" : "0 0 6px", lineHeight: 1.3 }}>{inline(heading[2], key)}</Tag>);
      continue;
    }
    if (bullet || numbered) {
      flushPara();
      const ordered = !!numbered;
      // A numbered list keeps its first number: items a blank line apart
      // (as replies write them) read 1, 2, 3 — not 1, 1, 1.
      if (!list || list.ordered !== ordered) { flushList(); list = { ordered, start: numbered ? Number(numbered[1]) : 1, items: [] }; }
      list.items.push(bullet ? bullet[1] : numbered![2]);
      continue;
    }
    if (quote) {
      flushPara(); flushList();
      const key = `q-${blocks.length}`;
      blocks.push(
        <blockquote key={key} style={{ margin: "0 0 10px", paddingLeft: 12, borderLeft: "3px solid var(--color-border)", color: "var(--color-text-muted)" }}>
          {inline(quote[1], key)}
        </blockquote>,
      );
      continue;
    }
    flushList();
    para.push(line);
  }
  flushPara();
  flushList();

  // The trailing block carries the container's own margin, not its own.
  return <div style={{ lineHeight: 1.6, ...style }} className="mvx-rich-text">{blocks}</div>;
}
