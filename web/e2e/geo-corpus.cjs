"use strict";
const {chromium} = require("playwright");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const [base, fixture, token, sourceRun] = process.argv.slice(2);
if (!base || !fixture || !token || !sourceRun || new URL(base).hostname !== "127.0.0.1" || new URL(fixture).hostname !== "127.0.0.1") throw Error("Потрібен ізольований локальний стенд");

(async () => {
  const browser = await chromium.launch({headless:true,...(process.env.QUALITY_BROWSER_CHANNEL?{channel:process.env.QUALITY_BROWSER_CHANNEL}:{})});
  try {
    const context = await browser.newContext({extraHTTPHeaders:{Authorization:"Bearer "+token},viewport:{width:1440,height:1000}});
    const page = await context.newPage(), errors=[];
    page.on("pageerror",error=>errors.push(error.message));
    await page.goto(base);
    await page.locator("#urls").fill(fixture+"/page?token=browser-private-value");
    await page.locator("#submit").click();
    await page.waitForURL(/\/audits\/[a-f0-9-]{36}$/);
    await page.waitForFunction(()=>document.getElementById("run-status").textContent==="Завершено",{},{timeout:60000});
    const ordinaryRun=page.url().split("/").at(-1);
    const ordinaryBefore=await (await context.request.get(`${base}/api/audits/${ordinaryRun}/results`)).json();
    assert.equal(ordinaryBefore.rows.length,1);
    assert.equal(ordinaryBefore.rows[0].status_code,200);
    assert(!JSON.stringify(ordinaryBefore).includes("browser-private-value"));
    const sourceBefore=await (await context.request.get(`${base}/api/audits/${sourceRun}/results`)).json();
    await page.goto(base+"/geo");
    await page.locator("#source-run").fill(sourceRun);
    await page.locator("#domain").fill("example.com");
    await page.locator("#brand").fill('<img src=x onerror="window.geoCorpusXss=1">');
    await page.locator("#queries").fill("rare stripe chargeback protection");
    await page.locator("#analyze").click();
    await page.waitForURL(/\/geo\/[a-f0-9-]{36}$/,{timeout:120000});
    const report=page.url().split("/").at(-1);
    await page.waitForFunction(()=>document.getElementById("geo-corpus-status").textContent.includes("10001"));
    assert.match(await page.locator("#geo-corpus-status").textContent(),/10002/);
    assert.match(await page.locator("#geo-corpus-status").textContent(),/повноту покриття сайту не підтверджено/);
    await page.waitForFunction(()=>document.getElementById("opportunity-counts").textContent.includes("10002"));
    assert.equal(await page.evaluate(()=>window.geoCorpusXss),undefined);
    await page.locator("#target-edit-open").click();
    await page.locator("#geo-target-edit").waitFor({state:"visible"});
    assert.match(await page.locator("#target-edit-page-label").textContent(),/У вибірці: 50/);
    await page.locator("#target-edit-next").click();
    await page.waitForFunction(()=>document.getElementById("target-edit-page-label").textContent.includes("Сторінка 2"));
    assert(await page.locator('#target-edit-page option[value="51"]').count());
    await page.locator("#target-edit-search").fill("catalog/10001");
    await page.waitForFunction(()=>document.querySelector('#target-edit-page option[value="10001"]')!=null);
    await page.locator("#target-edit-page").selectOption("10001");
    await page.locator("#target-edit-reason").fill("Перевірено останню ціль повного корпусу");
    await page.locator("#target-edit-save").click();
    await page.waitForFunction(()=>!document.getElementById("geo-target-edit").open);
    await page.waitForFunction(()=>document.querySelector("#geo-table tbody").textContent.includes("Ручний вибір"));
    await page.locator("#target-edit-open").click();
    await page.waitForFunction(()=>document.getElementById("geo-target-edit").open);
    await page.locator("#target-edit-search").fill("catalog/1");
    await page.waitForFunction(()=>document.getElementById("target-edit-page-label").textContent.includes("У вибірці: 50"));
    assert.equal(await page.locator("#target-edit-page").inputValue(),"10001");
    for(const viewport of [{width:1440,height:1000},{width:390,height:844}]) {
      await page.setViewportSize(viewport);
      assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),"Переповнення сторінки");
      assert.equal(await page.evaluate(()=>window.geoCorpusXss),undefined);
      if(process.env.QUALITY_ARTIFACT_DIR) {
        await fs.mkdir(process.env.QUALITY_ARTIFACT_DIR,{recursive:true});
        await page.screenshot({path:path.join(process.env.QUALITY_ARTIFACT_DIR,`geo-corpus-${viewport.width}.png`),fullPage:true});
      }
    }
    await page.locator('[data-close="geo-target-edit"]').click();
    assert.deepEqual(await (await context.request.get(`${base}/api/audits/${sourceRun}/results`)).json(),sourceBefore,"GEO змінив вихідний SEO-аудит");
    assert.deepEqual(await (await context.request.get(`${base}/api/audits/${ordinaryRun}/results`)).json(),ordinaryBefore,"GEO змінив звичайний SEO-аудит");
    const csv=await context.request.get(`${base}/api/geo/reports/${report}/tasks.csv`);
    assert.equal(csv.status(),200);
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({source_run:sourceRun,ordinary_run:ordinaryRun,report,domain_pages:10001,best_target:10001,pagination:true,manual_mapping:true,desktop:true,mobile:true,escaped:true,seo_unchanged:true,js_errors:errors}));
    await context.close();
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
