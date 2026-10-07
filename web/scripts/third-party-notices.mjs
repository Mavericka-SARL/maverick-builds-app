// Writes the licence notices the console bundle must carry: the licence and
// NOTICE texts of every production npm package (package-lock.json entries
// not marked dev), plus build tools whose own code is emitted into the
// bundle. Refuses a package whose licence is missing or not on the reviewed
// list, so a dependency with new obligations fails the image build.
//
//   node scripts/third-party-notices.mjs [--out dist/licenses/THIRD_PARTY_NOTICES.txt] [--report]
//
// web/Dockerfile runs it after `vite build`; the Go side is
// cmd/third-party-notices. See docs/LICENSING.md, "What ships with a release".
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

// Same reviewed list as cmd/third-party-notices.
const ALLOWED = new Set(["MIT", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "MPL-2.0", "Zlib", "0BSD", "Unlicense", "CC0-1.0", "BlueOak-1.0.0"]);
// Dev dependencies whose code ends up in the bundle: Tailwind emits its
// preflight stylesheet into the CSS.
const ALSO_BUNDLED = ["node_modules/tailwindcss"];
// Packages accepted after review although their licence field does not
// parse; each names why. Empty today.
const EXCEPTIONS = {};

function licenceOf(meta) {
  if (typeof meta.license === "string") return meta.license;
  if (meta.license && typeof meta.license.type === "string") return meta.license.type;
  if (Array.isArray(meta.licenses)) return meta.licenses.map((l) => l.type ?? l).join(" OR ");
  return "MISSING";
}

// An SPDX expression is acceptable when every alternative of an OR, or
// every term of an AND, is: "(MIT OR Apache-2.0)" passes on either.
function acceptable(expr) {
  const clean = expr.replace(/[()]/g, " ").trim();
  if (/\sOR\s/.test(clean)) return clean.split(/\s+OR\s+/).some((t) => acceptable(t));
  return clean.split(/\s+AND\s+/).every((t) => ALLOWED.has(t.trim()));
}

// Licence texts at the package root, plus those of code it vendors in
// subdirectories (victory-vendor carries d3's that way), nested
// node_modules excluded: those are packages of their own.
function licenceFiles(dir, prefix = "", depth = 0) {
  const files = {};
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.isDirectory()) {
      if (depth < 3 && entry.name !== "node_modules") Object.assign(files, licenceFiles(join(dir, entry.name), `${prefix}${entry.name}/`, depth + 1));
    } else if (/^(licen[cs]e|copying|notice|unlicense)/i.test(entry.name)) {
      files[prefix + entry.name] = readFileSync(join(dir, entry.name), "utf8");
    }
  }
  return files;
}

const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8"));
const paths = Object.entries(lock.packages)
  .filter(([path, entry]) => path.startsWith("node_modules/") && ((!entry.dev && !entry.devOptional) || ALSO_BUNDLED.includes(path)))
  .map(([path]) => path);

const notices = [];
const bad = [];
for (const path of paths) {
  const dir = join(root, path);
  if (!existsSync(join(dir, "package.json"))) continue; // optional, not installed on this platform
  const meta = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
  const licence = licenceOf(meta);
  const files = licenceFiles(dir);
  if (!(meta.name in EXCEPTIONS) && !acceptable(licence)) bad.push(`${meta.name} ${meta.version}: ${licence}`);
  notices.push({ name: meta.name, version: meta.version, licence, files });
}
notices.sort((a, b) => a.name.localeCompare(b.name) || a.version.localeCompare(b.version));

if (bad.length) {
  console.error("these packages need a licence review before they can ship (see web/scripts/third-party-notices.mjs):");
  for (const b of bad) console.error("  " + b);
  process.exit(1);
}

const args = process.argv.slice(2);
if (args.includes("--report")) {
  for (const n of notices) console.log(`${n.name}\t${n.version}\t${n.licence}`);
  process.exit(0);
}

const rule = "=".repeat(78);
let text =
  "THIRD-PARTY NOTICES — maverickbuilds.app console\n\n" +
  "The console bundle includes the third-party components listed below, each under\n" +
  "its own licence, reproduced here. Mavericka's own licences (LICENSE.txt,\n" +
  "ee-LICENSE.txt) sit beside this file and do not replace or restrict any of these.\n\n";
for (const n of notices) {
  text += `${rule}\n${n.name} ${n.version} — ${n.licence}\n${rule}\n`;
  const names = Object.keys(n.files).sort();
  if (names.length === 0) text += `\n(the package ships no licence file; its package.json declares ${n.licence})\n`;
  for (const name of names) text += `\n--- ${name} ---\n\n${n.files[name].trim()}\n`;
  text += "\n";
}

const outIdx = args.indexOf("--out");
if (outIdx >= 0) {
  const out = resolve(args[outIdx + 1]);
  mkdirSync(dirname(out), { recursive: true });
  writeFileSync(out, text);
  console.log(`third-party notices for ${notices.length} packages written to ${args[outIdx + 1]}`);
} else {
  process.stdout.write(text);
}
