"use strict";
const { chromium } = require("playwright");
const { randomUUID } = require("node:crypto");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const [base, fixture, token] = process.argv.slice(2);
if (!base || !fixture || !token || new URL(base).hostname !== "127.0.0.1") throw Error("Потрібен ізольований локальний стенд");
(async () => {
  const browser = await chromium.launch(process.env.QUALITY_BROWSER_CHANNEL ? {channel:process.env.QUALITY_BROWSER_CHANNEL} : {});
  try {
    const context = await browser.newContext({extraHTTPHeaders:{Authorization:"Bearer "+token},viewport:{width:1440,height:1000}});
    const page = await context.newPage(), errors = [];
    page.on("pageerror", e => errors.push(e.message));
    await page.goto(base);
    await page.locator("#urls").fill(fixture+"/en\n"+fixture+"/es?token=quality-browser-private\n"+fixture+"/plain");
    await page.locator("#submit").click();
    await page.waitForURL(/\/audits\/[a-f0-9-]{36}$/);
    await page.waitForFunction(() => document.getElementById("run-status").textContent === "Завершено",{},{timeout:60000});
    const run = page.url().split("/").at(-1);
    const before = await context.request.get(`${base}/api/audits/${run}/results`);
    const snapshot = await before.json();
    assert.equal(snapshot.rows.length,3);
    assert(!JSON.stringify(snapshot).includes("quality-browser-private"));
    await page.locator("#tab-international").click();
    await page.waitForFunction(() => document.querySelectorAll("#international-rows tr").length === 3);
    assert((await page.locator("#international-rows").textContent()).includes("Hreflang не оголошено"));
    assert(!(await page.locator("#international").textContent()).includes("quality-browser-private"));
    assert.equal(await page.evaluate(() => window.qualityXss),undefined);
    for (const viewport of [{width:1440,height:1000},{width:390,height:844}]) {
      await page.setViewportSize(viewport);
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth+1),"Переповнення сторінки");
      if (process.env.QUALITY_ARTIFACT_DIR) {
        await fs.mkdir(process.env.QUALITY_ARTIFACT_DIR,{recursive:true});
        await page.screenshot({path:path.join(process.env.QUALITY_ARTIFACT_DIR,`international-${viewport.width}.png`),fullPage:true});
      }
    }
    const geoResponse = await context.request.post(`${base}/api/geo/reports`,{headers:{Origin:base,"X-SEO-Auditor-Request":"1"},data:{id:randomUUID(),source_run_id:run,domain:"quality-own.test",brand:'<img src=x onerror="window.qualityXss=1">',queries:"chargeback protection\nStripe prevention"}});
    assert.equal(geoResponse.status(),201,await geoResponse.text());
    const geo=await geoResponse.json();
    await page.goto(`${base}/geo/${geo.id}`);
    await page.locator("#geo-table tbody tr").first().waitFor();
    assert.equal(await page.evaluate(() => window.qualityXss),undefined);
    const after = await context.request.get(`${base}/api/audits/${run}/results`);
    assert.deepEqual(await after.json(),snapshot,"GEO змінив збережені SEO-результати");
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({seo:true,hreflang:true,geo:true,escaped:true,isolated:true,desktop:true,mobile:true,js_errors:errors}));
    await context.close();
  } finally { await browser.close(); }
})().catch(e=>{console.error(e.message);process.exitCode=1;});
