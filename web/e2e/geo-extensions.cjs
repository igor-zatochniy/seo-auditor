"use strict";
const {chromium}=require("playwright");
const assert=require("node:assert/strict");
const fs=require("node:fs/promises");
const path=require("node:path");
const [base,fixture,token,source]=process.argv.slice(2);
if(!base||new URL(base).hostname!=="127.0.0.1")throw Error("Потрібен ізольований локальний стенд");
(async()=>{
 const browser=await chromium.launch({headless:true,channel:process.env.QUALITY_BROWSER_CHANNEL||"msedge"});
 try{
  const context=await browser.newContext({extraHTTPHeaders:{Authorization:"Bearer "+token},viewport:{width:1440,height:1000}}),page=await context.newPage(),errors=[];
  page.on("pageerror",e=>errors.push(e.message));
  const sourceBefore=await(await context.request.get(`${base}/api/audits/${source}/results`)).json();
  await page.goto(base);assert.equal(await page.locator("#max-pages").getAttribute("max"),"20000");
  await page.locator("#urls").fill(fixture+"/page");await page.locator("#submit").click();await page.waitForURL(/\/audits\/[a-f0-9-]{36}$/);await page.waitForFunction(()=>document.getElementById("run-status").textContent==="Завершено",{},{timeout:60000});
  const ordinary=page.url().split("/").at(-1),ordinaryBefore=await(await context.request.get(`${base}/api/audits/${ordinary}/results`)).json();
  await page.goto(base+"/geo");await page.locator("#source-run").fill(source);await page.locator("#domain").fill("example.com");await page.locator("#brand").fill("Merchanto");await page.locator("#queries").fill("Stripe protección pagos");await page.locator("#semantic-matching").check();await page.locator("#analyze").click();await page.waitForURL(/\/geo\/[a-f0-9-]{36}$/);
  const report=page.url().split("/").at(-1);await page.locator("#geo-table tbody tr").first().click();await page.locator("#geo-details").waitFor({state:"visible"});
  assert.match(await page.locator("#detail-analysis").textContent(),/Багатомовне словникове/);
  await page.locator("#answer-text").fill('Merchanto helps. <img src=x onerror="window.answerXss=1"> https://example.com/report?token=browser-answer-secret');await page.locator("#answer-sources").fill("https://other.example/article");await page.locator("#answer-preview").click();await page.locator("#answer-review").waitFor({state:"visible"});assert(await page.locator("#answer-save").isDisabled());
  assert(!((await page.locator("#answer-candidates").textContent()).includes("browser-answer-secret")));
  const candidates=page.locator("#answer-candidates label");for(let i=0;i<await candidates.count();i++){const row=candidates.nth(i);if((await row.textContent()).includes("Мій сайт"))await row.locator("input").check();}
  await page.locator("#answer-confirm").check();await page.locator("#answer-save").click();await page.waitForFunction(()=>document.getElementById("answer-status").textContent.includes("збережено"));await page.waitForFunction(()=>document.querySelectorAll("#answer-history details").length===1);
  await page.locator("#answer-history details").first().evaluate(n=>n.open=true);assert.equal(await page.evaluate(()=>window.answerXss),undefined);
  for(const v of [{width:1440,height:1000},{width:390,height:844}]){await page.setViewportSize(v);await page.locator("#answer-history").scrollIntoViewIfNeeded();assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));assert(await page.locator("#geo-details").evaluate(n=>n.scrollWidth<=n.clientWidth+1));if(process.env.QUALITY_ARTIFACT_DIR){await fs.mkdir(process.env.QUALITY_ARTIFACT_DIR,{recursive:true});await page.screenshot({path:path.join(process.env.QUALITY_ARTIFACT_DIR,`geo-extensions-${v.width}.png`)});}}
  const stored=await(await context.request.get(`${base}/api/geo/reports/${report}/queries/1/answers`)).json();assert.equal(stored.items.length,1);assert(!JSON.stringify(stored).includes("browser-answer-secret"));
  assert.deepEqual(await(await context.request.get(`${base}/api/audits/${source}/results`)).json(),sourceBefore);assert.deepEqual(await(await context.request.get(`${base}/api/audits/${ordinary}/results`)).json(),ordinaryBefore);assert.deepEqual(errors,[]);
  console.log(JSON.stringify({report,ordinary,semantic:true,confirmation:true,escaped:true,seo_unchanged:true,desktop:true,mobile:true,js_errors:errors}));
 }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
