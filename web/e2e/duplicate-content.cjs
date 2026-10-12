"use strict";
const { chromium } = require("playwright");
const http = require("node:http");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const base = process.env.DUPLICATE_TEST_BASE;
const token = process.env.DUPLICATE_TEST_TOKEN;
if (!base || !token || new URL(base).hostname !== "127.0.0.1") throw Error("Потрібен ізольований локальний стенд");
(async () => {
  let fixture;
  const server = http.createServer((req,res) => {
    if (req.url.startsWith("/robots.txt")) { res.writeHead(404); res.end(); return; }
    const product = req.url.startsWith("/product");
    const text = product ? "Mountain bicycles include aluminum wheels hydraulic brakes outdoor equipment reliable suspension. " : "Merchant payment protection prevents fraud with transaction evidence and dispute monitoring. ";
    res.setHeader("Content-Type","text/html; charset=utf-8");
    res.end(`<html lang="en"><head><title>&lt;img src=x onerror=window.duplicateXss=1&gt;</title><link rel="canonical" href="${fixture}/${product?"product":"page"}/1">${product?'<script type="application/ld+json">{"@type":"Product"}</script>':""}</head><body><nav>${req.url}</nav><main><h1>${product?"Mountain bicycles":"Merchant payment protection"}</h1><p>${text.repeat(40)}</p></main></body></html>`);
  });
  await new Promise(resolve => server.listen(0,"127.0.0.1",resolve));
  fixture = `http://127.0.0.1:${server.address().port}`;
  const browser = await chromium.launch({channel:process.env.QUALITY_BROWSER_CHANNEL || "msedge",headless:true});
  try {
    const context = await browser.newContext({extraHTTPHeaders:{Authorization:"Bearer "+token},viewport:{width:1440,height:1000}});
    const page = await context.newPage(), errors=[];
    page.on("pageerror",error=>errors.push(error.message));
    await page.goto(base);
    const urls = Array.from({length:31},(_,i)=>`${fixture}/page/${i+1}?token=duplicate-private-${i}`);
    urls.push(`${fixture}/product/1`,`${fixture}/product/2`);
    await page.locator("#urls").fill(urls.join("\n"));
    await page.locator("#submit").click();
    await page.waitForURL(/\/audits\/[a-f0-9-]{36}$/);
    await page.waitForFunction(()=>document.getElementById("run-status").textContent==="Завершено",{},{timeout:120000});
    const run=page.url().split("/").at(-1);
    const before = await context.request.get(`${base}/api/audits/${run}/results`), snapshot=await before.json();
    assert.equal(snapshot.rows.length,33);
    await page.locator("#tab-duplicates").click();
    await page.waitForFunction(()=>document.querySelectorAll("#duplicates-rows tr").length===2);
    assert((await page.locator("#duplicates-summary").textContent()).includes("33"));
    assert((await page.locator("#duplicates-rows").textContent()).includes("Товарний варіант"));
    assert(!(await page.locator("#duplicates").textContent()).includes("duplicate-private"));
    assert.equal(await page.evaluate(()=>window.duplicateXss),undefined);
    for (const viewport of [{width:1440,height:1000},{width:390,height:844}]) {
      await page.setViewportSize(viewport);
      assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),"Переповнення сторінки");
      if(process.env.QUALITY_ARTIFACT_DIR) {
        await fs.mkdir(process.env.QUALITY_ARTIFACT_DIR,{recursive:true});
        await page.screenshot({path:path.join(process.env.QUALITY_ARTIFACT_DIR,`duplicates-${viewport.width}.png`),fullPage:true});
      }
    }
    await page.locator("#duplicates-rows button").first().click();
    await page.waitForFunction(()=>document.querySelectorAll("#duplicates-member-rows tr").length===31);
    assert((await page.locator("#duplicates-member-rows").textContent()).includes("<img src=x onerror=window.duplicateXss=1>"));
    assert.equal(await page.locator("#duplicates-member-rows img").count(),0);
    assert.equal(await page.evaluate(()=>window.duplicateXss),undefined);
    await page.locator('[data-close="duplicates-dialog"]').click();
    await page.locator("#tab-pages").click();
    assert.equal(await page.locator("#results-table tbody tr").count(),33);
    const after=await context.request.get(`${base}/api/audits/${run}/results`);
    assert.deepEqual(await after.json(),snapshot,"Аналіз дублів змінив SEO-результати");
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({run,results:33,exact_clusters:2,product_context:true,redaction:true,escaped:true,desktop:true,mobile:true,seo_unchanged:true,js_errors:errors}));
    await context.close();
  } finally { await browser.close(); await new Promise(resolve=>server.close(resolve)); }
})().catch(error=>{console.error(error);process.exitCode=1;});
