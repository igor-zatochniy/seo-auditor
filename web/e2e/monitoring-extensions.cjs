"use strict";
const assert = require("node:assert/strict");
const http = require("node:http");
const fs = require("node:fs/promises");
const path = require("node:path");
const {chromium} = require("playwright");

const base = process.env.AUDITOR_BASE_URL || "http://localhost:8097";
const address=new URL(base);
if(address.protocol!=="http:"||!["localhost","127.0.0.1"].includes(address.hostname)||address.port==="8081")throw Error("Потрібен ізольований localhost стенд, не 8081");
const token = process.env.AUDITOR_ACCESS_TOKEN;
if (!token) throw Error("AUDITOR_ACCESS_TOKEN is required for an isolated preview");
const artifacts = process.env.AUDITOR_ARTIFACT_DIR || path.join(process.cwd(), "monitoring-extension-artifacts");
let phase = 0;
const fixture = http.createServer((req,res)=>{
  if(req.url.startsWith("/robots.txt")){res.writeHead(404);res.end();return;}
  if(phase===1){res.setHeader("X-Robots-Tag","googlebot: noindex");}
  res.setHeader("ETag",'"fixture-v1"');
  if(req.headers["if-none-match"]==='"fixture-v1"'){res.writeHead(304);res.end();return;}
  res.setHeader("Content-Type","text/html; charset=utf-8");
  res.end('<html><head><title>&lt;img src=x onerror=window.__xss=1&gt;</title><meta name="description" content="Fixture description"><script type="application/ld+json">{"@type":"Product","name":"&amp;quot;Fixture&amp;quot;"}</script></head><body><main><h1>Тестова сторінка</h1><p>Основний текст для перевірки звичайного аудиту.</p></main></body></html>');
});

async function main(){
  await fs.mkdir(artifacts,{recursive:true});
  await new Promise(resolve=>fixture.listen(0,"127.0.0.1",resolve));
  const fixtureURL=`http://127.0.0.1:${fixture.address().port}/page`;
  const browser=await chromium.launch({headless:true,channel:"msedge",args:["--disable-dev-shm-usage","--renderer-process-limit=2"]});
  const context=await browser.newContext({viewport:{width:1440,height:1000}});
  const page=await context.newPage();const errors=[];
  page.on("pageerror",error=>errors.push(error.message));
  async function api(route,body){return page.evaluate(async({route,body})=>{const headers={"X-SEO-Auditor-Request":"1"};if(body!==undefined)headers["Content-Type"]="application/json";const response=await fetch(route,{method:body===undefined?"GET":"POST",headers,credentials:"same-origin",body:body===undefined?undefined:JSON.stringify(body)});const data=await response.json();if(!response.ok)throw Error(`${response.status}: ${data.error?.message}`);return data;},{route,body});}
  async function waitRun(id){for(let i=0;i<80;i++){const result=await api(`/api/audits/${id}`);if(result.status!=="running")return result;await page.waitForTimeout(250);}throw Error("run did not finish");}
  try{
    await page.goto(base+"/#access_token="+encodeURIComponent(token));
    await page.locator("#audit-form").waitFor({state:"visible"});
    await page.locator("#urls").fill(fixtureURL);
    await page.locator("#submit").click();
    await page.waitForURL(/\/audits\/[a-f0-9-]{36}$/i);
    const firstID=new URL(page.url()).pathname.split("/").at(-1);
    assert.equal((await waitRun(firstID)).status,"completed");
    await page.locator("#tab-pages").click();
    await page.waitForTimeout(1500);
    await page.locator("#results-table tbody tr").first().click();
    await page.locator(".structured-report").waitFor();
    assert.match(await page.locator(".structured-report").innerText(),/Product snippets|повторне HTML-екранування/);
    assert.equal(await page.evaluate(()=>window.__xss),undefined);
    await page.screenshot({path:path.join(artifacts,"seo-jsonld-desktop.png"),fullPage:true});
    await page.keyboard.press("Escape");

    await page.goto(base+"/bots");
    await page.locator("#bot-domain").fill("example.com");
    await page.locator("#bot-log").fill(JSON.stringify({time:"2026-10-12T09:00:00Z",remote_addr:"66.249.66.1",request:"GET /file?token=private-browser-fixture HTTP/1.1",user_agent:"Googlebot/2.1",status:404}));
    await page.locator("#bot-import").click();
    await page.locator("#bot-report").waitFor({state:"visible"});
    const reportText=await page.locator("#bot-report").innerText();
    assert.match(reportText,/404/);assert.doesNotMatch(reportText,/private-browser-fixture|66\.249\.66\.1/);
    await page.screenshot({path:path.join(artifacts,"bot-analytics-desktop.png"),fullPage:true});
    await page.setViewportSize({width:390,height:844});
    await page.screenshot({path:path.join(artifacts,"bot-analytics-mobile.png"),fullPage:true});
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>window.innerWidth+1),false,"mobile page overflows");
    await page.setViewportSize({width:1440,height:1000});

    await page.goto(base+"/monitoring");
    await page.locator("#new-schedule").click();
    await page.locator("#schedule-name").fill("Тест адаптивного моніторингу");
    await page.locator("#schedule-adaptive").check();
    assert.equal(await page.locator("#schedule-site-mode").isDisabled(),true);
    await page.locator("#schedule-urls").fill(fixtureURL);
    await page.locator("#interval-hours").fill("1");
    await page.locator("#adaptive-max-hours").fill("8");
    await page.locator("#save-schedule").click();
    await page.locator("#schedule-dialog").waitFor({state:"hidden"});
    const schedules=await api("/api/schedules");
    const schedule=schedules.find(item=>item.name==="Тест адаптивного моніторингу");
    assert.ok(schedule?.adaptive);
    const first=await api(`/api/schedules/${schedule.id}/run`,{});await waitRun(first.id);
    for(let i=0;i<80;i++){const runs=await api(`/api/schedules/${schedule.id}/runs`);if(runs[0]?.comparison_status!=="pending")break;await page.waitForTimeout(300);}
    const second=await api(`/api/schedules/${schedule.id}/run`,{});await waitRun(second.id);
    const results=await api(`/api/audits/${second.id}/results`);
    assert.equal(results.rows[0].http_response_status,304);assert.equal(results.rows[0].reused_from_run_id,first.id);
    for(let i=0;i<80;i++){const runs=await api(`/api/schedules/${schedule.id}/runs`);if(runs[0]?.comparison_status!=="pending")break;await page.waitForTimeout(300);}
    await api(`/api/schedules/${schedule.id}/update`,{name:schedule.name,enabled:false,interval_hours:1,next_run_at:new Date(Date.now()+3600000).toISOString()});
    await page.locator("#refresh").click();
    await page.screenshot({path:path.join(artifacts,"adaptive-monitoring-desktop.png"),fullPage:true});
    await page.setViewportSize({width:390,height:844});
    await page.screenshot({path:path.join(artifacts,"adaptive-monitoring-mobile.png"),fullPage:true});
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>window.innerWidth+1),false,"monitoring page overflows");
    assert.deepEqual(errors,[],"JavaScript errors occurred");
    await fs.writeFile(path.join(artifacts,"verification.json"),JSON.stringify({ordinarySEO:firstID,adaptive:[first.id,second.id],botImportRedaction:true,jsonldEscaping:true,mobileOverflow:false,errors},null,2));
    console.log("PASS: ordinary SEO, JSON-LD, bot import/redaction, adaptive 200→304, desktop/mobile, JavaScript errors");
  }catch(error){await page.screenshot({path:path.join(artifacts,"failure.png"),fullPage:true});throw error;}
  finally{await context.close();await browser.close();await new Promise(resolve=>fixture.close(resolve));}
}
main().catch(error=>{console.error(error);fixture.close();process.exitCode=1;});
