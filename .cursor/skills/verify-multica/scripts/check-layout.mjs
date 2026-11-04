import { existsSync, lstatSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const repo = resolve(scriptDir, "../../../..");
const errors = [];

function read(path) {
  return readFileSync(path, "utf8");
}

function frontmatter(text) {
  const match = text.match(/^---\n([\s\S]*?)\n---/);
  if (!match) return null;
  const data = {};
  for (const line of match[1].split("\n")) {
    const index = line.indexOf(":");
    if (index === -1) continue;
    data[line.slice(0, index).trim()] = line.slice(index + 1).trim().replace(/^["']|["']$/g, "");
  }
  return data;
}

function must(condition, message) {
  if (!condition) errors.push(message);
}

const stometa = join(repo, ".cursor/skills/stometa-mode/SKILL.md");
const verify = join(repo, ".cursor/skills/verify-multica/SKILL.md");
must(existsSync(stometa), "missing stometa-mode/SKILL.md");
must(existsSync(verify), "missing verify-multica/SKILL.md");

if (existsSync(stometa)) {
  const meta = frontmatter(read(stometa));
  must(meta && meta.name === "stometa-mode", "stometa-mode name");
  must(meta && meta.description, "stometa-mode description");
  must(existsSync(join(repo, ".cursor/skills/stometa-mode/references/provenance.md")), "provenance");
}

if (existsSync(verify)) {
  const text = read(verify);
  const meta = frontmatter(text);
  must(meta && meta.name === "verify-multica", "verify-multica name");
  must(meta && /Multica/.test(meta.description || "") && /web/i.test(meta.description || ""), "verify description");
  for (const heading of ["## Launch", "## Doctor", "## Drive", "## Evidence", "## Cleanup", "## Helpers"]) {
    must(text.includes(heading), `missing ${heading}`);
  }
}

const features = ["sign-in.md", "create-issue.md", "inbox.md", "agents.md", "settings.md"];
const headings = [
  "## Sub-features",
  "## How to get to it (user POV)",
  "## Driving it with verify-multica",
  "## Gotchas",
];
for (const feature of features) {
  const path = join(repo, ".cursor/skills/verify-multica/features", feature);
  must(existsSync(path), `missing ${feature}`);
  if (!existsSync(path)) continue;
  const text = read(path);
  let offset = 0;
  for (const heading of headings) {
    const index = text.indexOf(heading, offset);
    must(index !== -1, `${feature} missing ${heading}`);
    if (index === -1) break;
    offset = index + heading.length;
  }
}

for (const name of ["stometa-mode", "verify-multica"]) {
  const link = join(repo, ".claude/skills", name);
  must(existsSync(link), `missing .claude/skills/${name}`);
  if (!existsSync(link)) continue;
  const stat = lstatSync(link);
  must(stat.isSymbolicLink(), `.claude/skills/${name} is not a symlink`);
  must(existsSync(join(link, "SKILL.md")), `symlink ${name} does not resolve to SKILL.md`);
}

const shell = join(repo, ".cursor/skills/verify-multica/scripts/verify-multica.sh");
must(existsSync(shell) && (lstatSync(shell).mode & 0o111) !== 0, "verify-multica.sh is not executable");
must(existsSync(join(repo, ".cursor/skills/verify-multica/scripts/browser.mjs")), "browser.mjs");

if (errors.length) {
  for (const error of errors) console.error(error);
  process.exit(1);
}
console.log("layout ok");
