import { createServer } from "node:http";
import { mkdir, writeFile } from "node:fs/promises";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, isAbsolute } from "node:path";
import { chromium } from "playwright";

const stateDir = process.argv[2];
if (!stateDir) {
  console.error("usage: node browser.mjs <state-dir>");
  process.exit(1);
}

const meta = Object.fromEntries(
  readFileSync(join(stateDir, "meta.env"), "utf8")
    .split("\n")
    .filter((line) => line && !line.startsWith("#"))
    .map((line) => {
      const index = line.indexOf("=");
      return [line.slice(0, index), line.slice(index + 1)];
    }),
);

const evidenceDir = meta.EVIDENCE_DIR;
const origin = meta.FRONTEND_ORIGIN;

const browser = await chromium.launch({
  headless: true,
  args: ["--no-sandbox", "--disable-dev-shm-usage"],
});
const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
const page = await context.newPage();
page.setDefaultTimeout(20000);
page.on("pageerror", (error) => {
  console.error(`pageerror ${error.message}`);
});

function target(body) {
  if (body.selector) return page.locator(body.selector).first();
  if (body.role) {
    const options = {};
    if (body.name !== undefined) options.name = body.name;
    if (body.exact) options.exact = true;
    return page.getByRole(body.role, options).first();
  }
  if (body.text !== undefined) {
    return page.getByText(body.text, { exact: Boolean(body.exact) }).first();
  }
  throw new Error("missing target");
}

function counted(body) {
  if (body.selector) return page.locator(body.selector);
  if (body.role) {
    const options = {};
    if (body.name !== undefined) options.name = body.name;
    if (body.exact) options.exact = true;
    return page.getByRole(body.role, options);
  }
  if (body.text !== undefined) {
    return page.getByText(body.text, { exact: Boolean(body.exact) });
  }
  throw new Error("missing target");
}

function evidencePath(path) {
  const full = isAbsolute(path) ? path : join(evidenceDir, path);
  return full;
}

async function handle(body) {
  const timeout = Number(body.timeout || 20000);
  if (body.op === "goto") {
    const path = body.path.startsWith("http") ? body.path : `${origin}${body.path}`;
    const response = await page.goto(path, { waitUntil: "domcontentloaded", timeout });
    await page.waitForFunction(
      () => Object.keys(document.body).some((key) => key.startsWith("__reactFiber")),
      { timeout },
    );
    return { url: page.url(), status: response ? response.status() : 0 };
  }
  if (body.op === "click") {
    await target(body).click({ timeout });
    return { url: page.url() };
  }
  if (body.op === "click-selector") {
    await page.locator(body.selector).first().click({ timeout });
    return { url: page.url() };
  }
  if (body.op === "hover") {
    await page.locator(body.selector).first().hover({ timeout });
    return {};
  }
  if (body.op === "fill") {
    await target(body).fill(body.value, { timeout });
    return {};
  }
  if (body.op === "type") {
    const locator = target(body);
    await locator.click({ timeout });
    await locator.pressSequentially(body.value, { delay: 20, timeout });
    return {};
  }
  if (body.op === "otp") {
    const otp = page.locator("[data-slot='input-otp']").first();
    await otp.click({ timeout });
    await page.keyboard.type(body.value, { delay: 40 });
    return { url: page.url() };
  }
  if (body.op === "expect") {
    await target(body).waitFor({ state: "visible", timeout });
    return {};
  }
  if (body.op === "expect-text") {
    await page.getByText(body.text, { exact: Boolean(body.exact) }).first().waitFor({
      state: "visible",
      timeout,
    });
    return {};
  }
  if (body.op === "expect-count") {
    const locator = counted(body);
    const wanted = Number(body.count);
    const started = Date.now();
    let count = await locator.count();
    while (count !== wanted && Date.now() - started < timeout) {
      await page.waitForTimeout(200);
      count = await locator.count();
    }
    if (count !== wanted) {
      throw new Error(`count ${count} wanted ${wanted} for ${JSON.stringify(body)}`);
    }
    return { count };
  }
  if (body.op === "expect-url") {
    await page.waitForFunction(
      ({ includes, pattern }) => {
        const url = location.href;
        if (includes && !url.includes(includes)) return false;
        if (pattern && !new RegExp(pattern).test(url)) return false;
        return true;
      },
      { includes: body.includes || "", pattern: body.pattern || "" },
      { timeout },
    );
    return { url: page.url() };
  }
  if (body.op === "screenshot") {
    const path = evidencePath(body.path);
    await mkdir(dirname(path), { recursive: true });
    if (body.selector) await page.locator(body.selector).first().scrollIntoViewIfNeeded();
    await page.screenshot({ path, fullPage: false });
    return { path };
  }
  if (body.op === "snapshot") {
    const path = evidencePath(body.path);
    await mkdir(dirname(path), { recursive: true });
    const text = await page.locator("body").ariaSnapshot();
    await writeFile(path, text);
    return { path };
  }
  if (body.op === "viewport") {
    await page.setViewportSize({ width: Number(body.width), height: Number(body.height) });
    return {};
  }
  if (body.op === "storage") {
    const data = await page.evaluate(() => ({
      token: localStorage.getItem("multica_token"),
      workspaceId: localStorage.getItem("multica_workspace_id"),
    }));
    if (!data.token || !data.workspaceId) {
      throw new Error(`missing session storage ${JSON.stringify(data)}`);
    }
    return data;
  }
  if (body.op === "url") return { url: page.url() };
  if (body.op === "quit") {
    server.close();
    await browser.close();
    return { bye: true };
  }
  throw new Error(`unknown op ${body.op}`);
}

const server = createServer(async (request, response) => {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  let body = {};
  try {
    body = JSON.parse(Buffer.concat(chunks).toString() || "{}");
    const result = await handle(body);
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({ ok: true, ...result }));
    if (body.op === "quit") process.exit(0);
  } catch (error) {
    const failure = join(evidenceDir, "failure.png");
    try {
      if (existsSync(evidenceDir)) await page.screenshot({ path: failure, fullPage: false });
    } catch {
    }
    response.writeHead(400, { "content-type": "application/json" });
    response.end(JSON.stringify({ ok: false, error: String(error && error.stack ? error.stack : error) }));
  }
});

await mkdir(evidenceDir, { recursive: true });
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const { port } = server.address();
await writeFile(join(stateDir, "browser.port"), String(port));
console.log(`browser ready ${port}`);
