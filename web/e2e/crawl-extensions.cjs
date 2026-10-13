"use strict";
const assert=require("node:assert/strict"),http=require("node:http"),fs=require("node:fs/promises"),path=require("node:path"),{chromium}=require("playwright");
const base=process.env.AUDITOR_BASE_URL||"http://localhost:8099",token=process.env.AUDITOR_ACCESS_TOKEN,artifacts=process.env.AUDITOR_ARTIFACT_DIR||path.join(process.cwd(),"crawl-extension-artifacts");
const address=new URL(base);if(!token||address.protocol!=="http:"||!["localhost","127.0.0.1"].includes(address.hostname)||address.port==="8081")throw Error("Потрібен ізольований стенд, не 8081");
const fixture=http.createServer((req,res)=>{
 if(req.url==="/robots.txt"){res.writeHead(404);res.end();return;}
 if(req.url==="/r"){res.writeHead(301,{Location:"/gone"});res.end();return;}
 if(req.url==="/x"||req.url==="/y"){res.writeHead(301,{Location:req.url==="/x"?"/y":"/x"});res.end();return;}
 if(req.url==="/gone"){res.writeHead(404);res.end();return;}
 res.setHeader("Content-Type","text/html; charset=utf-8");
 res.end('<html><head><title>&lt;img src=x onerror=window.__xss=1&gt;</title></head><body><h1>Сторінка перевірки</h1>'+(req.url==="/"?'<a href="/r">Redirect</a><a href="/x">Cycle</a>':'')+'</body></html>');
});
async function main(){
 await fs.mkdir(artifacts,{recursive:true});await new Promise(resolve=>fixture.listen(0,"127.0.0.1",resolve));
 const browser=await chromium.launch({headless:true,channel:"msedge",args:["--renderer-process-limit=1"]}),context=await browser.newContext({viewport:{width:1440,height:1000}}),page=await context.newPage(),errors=[];
 page.on("pageerror",e=>errors.push(e.message));
 try{
  await page.goto(base+"/#access_token="+encodeURIComponent(token));await page.locator("#audit-form").waitFor();
  await page.locator('[name="mode"][value="site"]').check();assert.equal(await page.locator("#discover-rendered").isDisabled(),true);
  await page.locator("#root-url").fill(`http://127.0.0.1:${fixture.address().port}/`);await page.locator("#use-sitemaps").uncheck();await page.locator("#max-pages").fill("10");await page.locator("#submit").click();await page.waitForURL(/\/audits\/[a-f0-9-]{36}$/i);
  const run=new URL(page.url()).pathname.split("/").at(-1);for(let i=0;i<80;i++){const status=await page.evaluate(async id=>(await(await fetch("/api/audits/"+id)).json()).status,run);if(status!=="running")break;await page.waitForTimeout(250);}
  await page.locator("#tab-routes").click();await page.locator("#routes-rows tr").nth(1).waitFor();assert.match(await page.locator("#routes-rows").innerText(),/Цикл/);assert.match(await page.locator("#routes-rows").innerText(),/HTTP 4xx\/5xx/);await page.screenshot({path:path.join(artifacts,"url-routes-desktop.png"),fullPage:true});
  await page.setViewportSize({width:390,height:844});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false);await page.screenshot({path:path.join(artifacts,"url-routes-mobile.png"),fullPage:true});await page.setViewportSize({width:1440,height:1000});
  await page.locator("#tab-pages").click();await page.locator("#results-table tbody tr").first().click();await page.locator(".field-performance").waitFor();await page.waitForTimeout(500);assert.match(await page.locator(".field-performance").innerText(),/CrUX не налаштовано/);assert.equal(await page.locator(".field-performance button").isDisabled(),true);assert.equal(await page.evaluate(()=>window.__xss),undefined);await page.keyboard.press("Escape");
  let posts=0;
  await page.route("**/api/audits/*/results/*/crux?*",async route=>{const request=route.request();if(request.method()==="POST"){posts++;assert.equal(request.postDataJSON().confirm_public_url,true);}const params=new URL(request.url()).searchParams;const scope=params.get("scope"),device=params.get("device");await route.fulfill({contentType:"application/json",body:JSON.stringify({configured:true,history:request.method()==="POST"?[{fetched_at:new Date().toISOString(),report:{status:"complete",scope,device,requested:scope==="origin"?"https://example.com":"https://example.com/page",identifier:scope==="origin"?"https://example.com":"https://example.com/page",first_date:"2026-09-15",last_date:"2026-10-12",metrics:{largest_contentful_paint:{p75:2600,rating:"needs_improvement",unit:"ms"},interaction_to_next_paint:{p75:150,rating:"good",unit:"ms"},cumulative_layout_shift:{p75:0.06,rating:"good",unit:""}}}}]:[]})});});
  await page.locator("#results-table tbody tr").first().click();const field=page.locator(".field-performance");await field.waitFor();await page.waitForTimeout(300);assert.equal(await field.locator("button").isDisabled(),true);await field.locator('input[type="checkbox"]').check();await field.locator("button").click();await field.getByText("INP",{exact:true}).waitFor();assert.equal(posts,1);await page.screenshot({path:path.join(artifacts,"crux-desktop.png"),fullPage:true});
  await field.locator('select[aria-label="Область CrUX"]').selectOption("origin");await page.waitForTimeout(250);await field.locator('input[type="checkbox"]').check();await field.locator("button").click();await field.getByText(/Доменні дані, не дані цієї сторінки/).waitFor();
  await page.setViewportSize({width:390,height:844});await field.scrollIntoViewIfNeeded();assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false);await page.screenshot({path:path.join(artifacts,"crux-mobile.png"),fullPage:true});assert.deepEqual(errors,[]);
  await fs.writeFile(path.join(artifacts,"verification.json"),JSON.stringify({run,routeCycles:true,routeHTTP:true,xss:false,mobileOverflow:false,cruxUnconfigured:true,cruxConsent:true,cruxFixturesOnly:true,errors},null,2));
  process.stdout.write("PASS: SEO/site crawl, URL routes, XSS, CrUX consent/scope/disabled state, desktop/mobile\n");
 }catch(e){await page.screenshot({path:path.join(artifacts,"failure.png"),fullPage:true});throw e;}finally{await browser.close();await new Promise(resolve=>fixture.close(resolve));}
}
main().catch(e=>{console.error(e);process.exitCode=1;});
