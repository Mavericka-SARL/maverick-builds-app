import type { ReactNode } from "react";

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
 *   [label](https://example.com) and [label](/console/path)
 *   - bullets, 1. numbers
 *   > quote
 *   ---        rule
 *   blank line separates paragraphs; a single newline is a line break.
 * Anything else is shown as written.
 */

const INLINE = /(\*\*[^*]+\*\*|\*[^*]+\*|`[^`]+`|\[[^\]]+\]\([^)\s]+\))/g;

/** Only schemes a browser may follow from a tenant's own text. */
function safeHref(raw: string): string | null {
  const href = raw.trim();
  if (href.startsWith("/") || href.startsWith("#")) return href;
  if (/^https?:\/\//i.test(href)) return href;
  if (/^mailto:/i.test(href)) return href;
  return null;
}

function inline(text: string, keyPrefix: string): ReactNode[] {
  const out: ReactNode[] = [];
  text.split(INLINE).forEach((part, i) => {
    if (!part) return;
    const key = `${keyPrefix}-${i}`;
    if (part.startsWith("**") && part.endsWith("**")) out.push(<strong key={key}>{part.slice(2, -2)}</strong>);
    else if (part.startsWith("`") && part.endsWith("`")) out.push(<code key={key} style={{ fontFamily: "var(--font-mono)", fontSize: "0.92em", background: "var(--color-surface-subtle)", padding: "1px 5px", borderRadius: 4 }}>{part.slice(1, -1)}</code>);
    else if (part.startsWith("*") && part.endsWith("*")) out.push(<em key={key}>{part.slice(1, -1)}</em>);
    else if (part.startsWith("[")) {
      const close = part.indexOf("](");
      const label = part.slice(1, close);
      const href = safeHref(part.slice(close + 2, -1));
      out.push(href
        ? <a key={key} href={href} target={href.startsWith("http") ? "_blank" : undefined} rel="noopener noreferrer" style={{ color: "var(--color-primary)" }}>{label}</a>
        : <span key={key}>{label}</span>);
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
  let list: { ordered: boolean; items: string[] } | null = null;

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
      ? <ol key={key} style={{ margin: "0 0 10px", paddingLeft: 22, listStyle: "decimal outside" }}>{items}</ol>
      : <ul key={key} style={{ margin: "0 0 10px", paddingLeft: 22, listStyle: "disc outside" }}>{items}</ul>);
    list = null;
  };

  for (const raw of src) {
    const line = raw.trimEnd();
    const heading = /^(#{1,3})\s+(.*)$/.exec(line);
    const bullet = /^[-*]\s+(.*)$/.exec(line);
    const numbered = /^\d+[.)]\s+(.*)$/.exec(line);
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
      if (!list || list.ordered !== ordered) { flushList(); list = { ordered, items: [] }; }
      list.items.push((bullet ?? numbered)![1]);
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
