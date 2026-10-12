"use strict";
const { chromium } = require("playwright");
const http = require("node:http");
const fs = require("node:fs/promises");
const path = require("node:path");
const assert = require("node:assert/strict");
const reportID = "11111111-1111-4111-8111-111111111111";
const setID = "22222222-2222-4222-8222-222222222222";

(async () => {
  const root = path.resolve(__dirname,"..");
  const html = (await fs.readFile(path.join(root,"templates","geo.html"),"utf8"))
    .replace('<script src="/static/geo.js" defer></script>','<script src="/fixture-init.js" defer></script>');
  const row = {ordinal:1,citations:[],result:{query:"best Stripe chargeback protection",target_id:1,target_url:"https://own.example/stripe",coverage:90,level:"strong",gaps:[]}};
  const fixture = {
    id:setID,report_id:reportID,canceled:false,own:[row],
    competitors:[{domain:"first.example",brand:"Перший конкурент",status:"ready",reused:false,run_id:reportID,report_id:reportID,rows:[row],gaps:{},discovery:{version:"sitemap-query-url-v1",state:"partial",candidate_urls:5000,selected_urls:25,matching_queries:1,query_count:2,sitemap_files:16,selection_limited:true,discovery_limited:true,warning:"<img src=x onerror=window.discoveryXss=1>"}},{domain:"second.example",brand:"Другий конкурент",status:"ready",reused:true,run_id:reportID,report_id:reportID,rows:[row],gaps:{}}]
  };
  const init = `"use strict";document.getElementById("geo-home").hidden=true;document.getElementById("geo-report").hidden=false;
    const el=(tag,value,cls)=>{const n=document.createElement(tag);if(value!=null)n.textContent=String(value);if(cls)n.className=cls;return n;};
    const api=async path=>path.includes("/competitors/${setID}")?${JSON.stringify(fixture)}:path.endsWith("/competitors")?[{id:"${setID}",at:"2026-10-11T10:00:00Z",ready:2,total:2,canceled:false}]:[];
    window.seoStrategy.init({api,el,reportID:"${reportID}",rows:[${JSON.stringify(row)}],notice:message=>{document.getElementById("notice").textContent=message;document.getElementById("notice").hidden=!message;}}).then(()=>window.fixtureReady=true);`;
  const server = http.createServer(async (req,res) => {
    try {
      if(req.url === "/fixture-init.js") { res.setHeader("Content-Type","text/javascript");res.end(init);return; }
      if(req.url.startsWith("/static/")) {
        const name = req.url.slice(8);
        if(!/^[a-zA-Z0-9.-]+$/.test(name)) {res.writeHead(404);res.end();return;}
        res.setHeader("Content-Type",name.endsWith(".css")?"text/css":"text/javascript");
        res.end(await fs.readFile(path.join(root,"static",name)));return;
      }
      if(req.url.startsWith("/geo/")) {res.setHeader("Content-Type","text/html; charset=utf-8");res.end(html);return;}
      res.writeHead(404);res.end();
    } catch(error) {res.writeHead(500);res.end(String(error));}
  });
  await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve));
  let browser;
  try {
    browser = await chromium.launch({channel:process.env.QUALITY_BROWSER_CHANNEL || "msedge",headless:true});
    const page = await browser.newPage(), errors=[];
    page.on("pageerror",error=>errors.push(error.message));
    await page.goto(`http://127.0.0.1:${server.address().port}/geo/${reportID}`);
    await page.waitForFunction(()=>window.fixtureReady);
    await page.locator("#geo-tab-competitors").click();
    await page.waitForFunction(()=>document.querySelector("#competitor-status").textContent.includes("5000"));
    const status = await page.locator("#competitor-status").textContent();
    assert(status.includes("відібрано 25"));assert(status.includes("discovery неповне"));assert(status.includes("повторно використано"));
    assert.equal(await page.locator("#competitor-status img").count(),0);
    assert.equal(await page.evaluate(()=>window.discoveryXss),undefined);
    assert.equal(await page.locator("#competitor-table tbody tr").count(),1);
    for(const viewport of [{width:1440,height:1000},{width:390,height:844}]) {
      await page.setViewportSize(viewport);
      assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),"Переповнення сторінки");
      if(process.env.QUALITY_ARTIFACT_DIR) {
        await fs.mkdir(process.env.QUALITY_ARTIFACT_DIR,{recursive:true});
        await page.screenshot({path:path.join(process.env.QUALITY_ARTIFACT_DIR,`competitor-discovery-${viewport.width}.png`),fullPage:true});
      }
    }
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({fixture_ui:true,discovery_summary:true,partial:true,reuse:true,escaped:true,desktop:true,mobile:true,js_errors:errors}));
  } finally {if(browser) await browser.close();await new Promise(resolve=>server.close(resolve));}
})().catch(error=>{console.error(error);process.exitCode=1;});
