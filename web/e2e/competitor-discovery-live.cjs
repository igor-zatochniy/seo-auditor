"use strict";
const { chromium } = require("playwright");
const http = require("node:http");
const dns = require("node:dns/promises");
const zlib = require("node:zlib");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const base = process.env.COMPETITOR_TEST_BASE, token = process.env.COMPETITOR_TEST_TOKEN;
if(!base || !token || new URL(base).hostname !== "127.0.0.1") throw Error("Потрібен ізольований локальний стенд");

(async () => {
  const hosts = ["own","first","second"].map(name=>`${name}.127.0.0.1.nip.io`);
  for(const host of hosts) assert.equal((await dns.lookup(host,{family:4})).address,"127.0.0.1","Тестовий домен має вести лише на loopback");
  const deep = "/solutions/payments/business/stripe-chargeback-protection";
  const hits = new Map();
  const server = http.createServer((req,res) => {
    const origin = "http://"+req.headers.host;
    hits.set(req.headers.host+req.url,(hits.get(req.headers.host+req.url)||0)+1);
    if(req.url === "/robots.txt") {res.setHeader("Content-Type","text/plain");res.end(`User-agent: *\nAllow: /\nSitemap: ${origin}/index.xml\n`);return;}
    if(req.url === "/sitemap.xml") {res.writeHead(404);res.end();return;}
    if(req.url === "/index.xml") {res.setHeader("Content-Type","application/xml");res.end(`<sitemapindex><sitemap><loc>${origin}/pages.xml.gz</loc></sitemap></sitemapindex>`);return;}
    if(req.url === "/pages.xml.gz") {res.setHeader("Content-Type","application/gzip");res.end(zlib.gzipSync(`<urlset>${Array.from({length:55},(_,i)=>`<url><loc>${origin}/news/item-${i}</loc></url>`).join("")}<url><loc>${origin}${deep}</loc></url></urlset>`));return;}
    const title = req.url === deep ? "Stripe chargeback protection software" : "Company news and information";
    const text = req.url === deep ? "Stripe chargeback protection software prevents disputes and protects payment transactions with evidence. " : "Company information and updates about business operations. ";
    res.setHeader("Content-Type","text/html; charset=utf-8");
    res.end(`<html lang="en"><head><title>${title}</title><meta name="description" content="${text}"><link rel="canonical" href="${origin}${req.url}"></head><body><main><h1>${title}</h1><p>${text.repeat(20)}</p></main>${req.url==="/"?'<a href="/news/item-0">News</a>':""}</body></html>`);
  });
  await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve));
  const port = server.address().port;
  let browser;
  try {
    browser = await chromium.launch({channel:process.env.QUALITY_BROWSER_CHANNEL || "msedge",headless:true});
    const context = await browser.newContext({extraHTTPHeaders:{Authorization:"Bearer "+token},viewport:{width:1440,height:1000}});
    const page = await context.newPage(), errors=[];
    page.on("pageerror",error=>errors.push(error.message));
    await page.goto(base);
    await page.locator("#urls").fill(`http://${hosts[0]}:${port}${deep}`);
    await page.locator("#submit").click();
    await page.waitForURL(/\/audits\/[a-f0-9-]{36}$/);
    await page.waitForFunction(()=>document.getElementById("run-status").textContent==="Завершено",{},{timeout:120000});
    const ownRun = page.url().split("/").at(-1);
    const beforeResponse = await context.request.get(`${base}/api/audits/${ownRun}/results`), before = await beforeResponse.json();
    await page.goto(base+"/geo");
    await page.locator("#source-run").fill(ownRun);
    await page.locator("#domain").fill(hosts[0]);
    await page.locator("#brand").fill("Власний тестовий сайт");
    await page.locator("#queries").fill("best Stripe chargeback protection");
    await page.locator("#analyze").click();
    await page.waitForURL(/\/geo\/[a-f0-9-]{36}$/,{timeout:120000});
    const report = page.url().split("/").at(-1);
    await page.locator("#geo-tab-competitors").click();
    await page.locator("#competitor-create-open").click();
    await page.locator("#competitor-input").fill(`http://${hosts[1]}:${port}/ | Перший конкурент\nhttp://${hosts[2]}:${port}/ | Другий конкурент`);
    await page.locator("#competitor-max-pages").fill("3");
    await page.locator("#competitor-submit").click();
    await page.waitForFunction(()=>!document.getElementById("geo-competitor-create").open,{},{timeout:30000});
    let data, set;
    const deadline = Date.now()+150000;
    while(Date.now()<deadline) {
      const history = await (await context.request.get(`${base}/api/geo/reports/${report}/competitors`)).json();
      assert.equal(history.length,1);
      set = history[0].id;
      data = await (await context.request.get(`${base}/api/geo/competitors/${set}`)).json();
      if(data.competitors.every(c=>c.status==="ready")) break;
      if(data.competitors.some(c=>c.status==="failed")) throw Error(JSON.stringify(data.competitors.map(c=>({status:c.status,error:c.error}))));
      await new Promise(resolve=>setTimeout(resolve,500));
    }
    assert(data.competitors.every(c=>c.status==="ready"),"Конкуренти не завершили аналіз");
    for(const c of data.competitors) {
      assert.equal(c.discovery.candidate_urls,56);
      assert.equal(c.discovery.selected_urls,2);
      assert.equal(c.discovery.matching_queries,1);
      assert(c.rows[0].result.target_url.endsWith(deep),"GEO не вибрав глибоку комерційну сторінку");
      const results = await (await context.request.get(`${base}/api/audits/${c.run_id}/results`)).json();
      assert.equal(results.rows.length,3,"Порушено ліміт конкурентного сканування");
    }
    await page.locator("#competitor-refresh").click();
    await page.waitForFunction(()=>document.querySelector("#competitor-status").textContent.includes("56 URL-кандидатів"));
    for(const viewport of [{width:1440,height:1000},{width:390,height:844}]) {
      await page.setViewportSize(viewport);
      assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
      if(process.env.QUALITY_ARTIFACT_DIR) {await fs.mkdir(process.env.QUALITY_ARTIFACT_DIR,{recursive:true});await page.screenshot({path:path.join(process.env.QUALITY_ARTIFACT_DIR,`competitor-live-${viewport.width}.png`),fullPage:true});}
    }
    assert.deepEqual(await (await context.request.get(`${base}/api/audits/${ownRun}/results`)).json(),before,"Вихідний SEO-аудит змінено");
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({own_run:ownRun,report,set,competitors:2,deep_targets:true,sitemap_candidates:56,page_limit:3,desktop:true,mobile:true,seo_unchanged:true,js_errors:errors}));
    await context.close();
  } finally {if(browser) await browser.close();await new Promise(resolve=>server.close(resolve));}
})().catch(error=>{console.error(error);process.exitCode=1;});
