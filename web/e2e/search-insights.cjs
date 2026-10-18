"use strict";
const assert = require("node:assert/strict"), http = require("node:http"), fs = require("node:fs/promises"), path = require("node:path"), { chromium } = require("playwright");
const base = process.env.AUDITOR_BASE_URL || "http://127.0.0.1:8107", token = process.env.AUDITOR_ACCESS_TOKEN;
const out = process.env.AUDITOR_ARTIFACT_DIR || path.join(process.cwd(), "search-insights-artifacts");
if (!token || !["127.0.0.1", "localhost"].includes(new URL(base).hostname) || new URL(base).port === "8081") throw Error("Потрібен ізольований стенд, не 8081");
const fixture = http.createServer((req, res) => {
  if (req.url === "/robots.txt") { res.writeHead(404); res.end(); return; }
  res.setHeader("Content-Type", "text/html; charset=utf-8");
  res.end(`<html><head><title>Сторінка продукту</title>${req.url.includes("/products/b") ? '<meta name="robots" content="noindex">' : ""}</head><body><main><h1>Продукт</h1><p>Основний текст продукту для звичайного SEO-аудиту.</p></main></body></html>`);
});
(async () => {
  await fs.mkdir(out, { recursive: true }); await new Promise(resolve => fixture.listen(0, "127.0.0.1", resolve));
  const origin = "http://127.0.0.1:" + fixture.address().port;
  const browser = await chromium.launch({ channel: "msedge", headless: true, args: ["--renderer-process-limit=1"] });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } }), page = await context.newPage(), errors = [];
  page.on("pageerror", e => errors.push(e.message));
  const api = async (endpoint, body) => {
    const options = { headers: { Authorization: "Bearer " + token, Origin: base, "X-SEO-Auditor-Request": "1" } };
    const r = body === undefined ? await context.request.get(base + endpoint, options) : await context.request.post(base + endpoint, { ...options, data: body });
    const data = await r.json(); if (!r.ok()) throw Error(r.status() + " " + JSON.stringify(data)); return data;
  };
  try {
    await page.goto(base + "/#access_token=" + encodeURIComponent(token)); await page.locator("#home").waitFor({ state: "visible" });
    const run = await api("/api/audits", { urls: origin + "/\n" + origin + "/products/a\n" + origin + "/products/b?token=fixture-private-search" });
    for (let n = 0; ; n++) { const p = await api(`/api/audits/${run.id}/progress`); if (p.run.status !== "running") { assert.equal(p.run.status, "completed"); break; } if (n > 100) throw Error("SEO timeout"); await new Promise(r => setTimeout(r, 250)); }
    await page.goto(base + "/audits/" + run.id);
    await page.locator("#tab-landing").click(); await page.waitForFunction(() => document.querySelector("#search-origin").options.length > 0);
    const query = "chargeback <img src=x onerror=window.__searchXSS=1>";
    const csv = `Date,Query,Page,Country,Device,Impressions,Clicks,Position\n2026-10-01,${query},${origin}/products/a,USA,desktop,100,10,2\n2026-10-01,${query},${origin}/products/b?token=fixture-private-search,USA,desktop,20,1,4\n2026-10-02,${query},${origin}/products/a,USA,desktop,10,1,5\n2026-10-02,${query},${origin}/products/b?token=fixture-private-search,USA,desktop,110,5,3\n`;
    await page.locator("#search-file").setInputFiles({ name: "gsc-history.csv", mimeType: "text/csv", buffer: Buffer.from(csv) });
    await page.locator('#search-import-form button[type="submit"]').click();
    await page.waitForFunction(() => document.querySelector("#landing-rows").textContent.includes("Змінилася посадкова сторінка"));
    assert.equal(await page.locator("#landing-rows img").count(), 0); assert.equal(await page.evaluate(() => Boolean(window.__searchXSS)), false);
    assert(!(await page.locator("#landing").innerText()).includes("fixture-private-search"));
    await page.screenshot({ path: path.join(out, "landing-desktop.png"), fullPage: true });
    await page.locator("#landing-rows button").click(); await page.locator("#landing-history-rows tr").nth(3).waitFor();
    assert(!(await page.locator("#landing-history-dialog").innerText()).includes("fixture-private-search"));
    await page.screenshot({ path: path.join(out, "history-desktop.png"), fullPage: true });
    await page.locator('[data-close="landing-history-dialog"]').click();
    await page.locator("#tab-structure").click(); await page.waitForFunction(() => document.querySelector("#structure-rows").textContent.includes("/products/"));
    const data = await api(`/api/audits/${run.id}/structure`), products = data.items.find(v => v.path === "/products/");
    assert.equal(products.pages, 2); assert.equal(products.noindex, 1); assert.equal(products.clicks, 6); assert.equal(products.previous_clicks, 11); assert.equal(products.traffic_comparable, true);
    await page.screenshot({ path: path.join(out, "structure-desktop.png"), fullPage: true });
    await page.getByRole("button", { name: "/products/", exact: true }).click(); await page.waitForFunction(() => document.querySelector("#structure-path").textContent === "/products/");
    await page.locator("#structure-up").click();
    await page.setViewportSize({ width: 390, height: 844 }); await page.screenshot({ path: path.join(out, "structure-mobile.png"), fullPage: true });
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.locator("#tab-landing").click(); await page.waitForFunction(() => document.querySelector("#landing-rows").textContent.includes("Змінилася посадкова сторінка"));
    await page.screenshot({ path: path.join(out, "landing-mobile.png"), fullPage: true }); assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    const results = await api(`/api/audits/${run.id}/results?limit=50`); assert.equal(results.rows.length, 3); assert(results.rows.every(v => v.status_code === 200));
    assert.deepEqual(errors, []); await fs.writeFile(path.join(out, "verification.json"), JSON.stringify({ run: run.id, errors, checks: ["real SEO", "CSV import", "leader change", "history", "URL redaction", "XSS", "structure counts", "drilldown", "desktop/mobile"] }, null, 2));
    console.log("PASS: search landing history, site structure, real SEO, privacy, XSS, desktop/mobile");
  } finally { await browser.close(); await new Promise(resolve => fixture.close(resolve)); }
})().catch(e => { console.error(e); process.exitCode = 1; });
