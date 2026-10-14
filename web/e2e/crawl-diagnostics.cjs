"use strict";
const assert=require("node:assert/strict"),http=require("node:http"),fs=require("node:fs/promises"),path=require("node:path"),{chromium}=require("playwright");
const base=process.env.AUDITOR_BASE_URL||"http://localhost:8100",token=process.env.AUDITOR_ACCESS_TOKEN,artifacts=process.env.AUDITOR_ARTIFACT_DIR||path.join(process.cwd(),"crawl-diagnostics-artifacts");
const address=new URL(base);if(!token||address.protocol!=="http:"||!["localhost","127.0.0.1"].includes(address.hostname)||address.port==="8081")throw Error("Потрібен окремий стенд, не 8081");
let fixtureStatus=200;
const fixture=http.createServer((req,res)=>{
 const uri=new URL(req.url,"http://"+req.headers.host);
 if(uri.pathname==="/robots.txt"){res.writeHead(404);res.end();return;}
 if(uri.pathname==="/sitemap.xml"){res.setHeader("Content-Type","application/xml");res.end(`<urlset><url><loc>http://${req.headers.host}/a?page=1</loc></url><url><loc>http://${req.headers.host}/a?page=2</loc></url><url><loc>http://${req.headers.host}/a?page=1500</loc></url></urlset>`);return;}
 res.setHeader("Content-Type","text/html; charset=utf-8");res.writeHead(uri.pathname==="/a"?fixtureStatus:200);
 res.end('<html><head><title>&lt;img src=x onerror=window.__diagnosticXSS=1&gt;</title></head><body><h1>Сторінка фікстури</h1><p>Звичайний SEO залишається незмінним.</p>'+(uri.pathname==="/"?'<a href="/deep?token=fixture-private-token">Глибока</a><a rel="nofollow" href="/nofollow">Nofollow</a>':'')+'</body></html>');
});
async function main(){
 await fs.mkdir(artifacts,{recursive:true});await new Promise(resolve=>fixture.listen(0,"127.0.0.1",resolve));const root="http://127.0.0.1:"+fixture.address().port;
 const browser=await chromium.launch({channel:"msedge",headless:true,args:["--renderer-process-limit=1"]}),context=await browser.newContext({viewport:{width:1440,height:1000}}),page=await context.newPage(),errors=[];
 page.on("pageerror",e=>errors.push(e.message));
 const api=async(endpoint,body)=>{const response=body===undefined?await context.request.get(base+endpoint):await context.request.post(base+endpoint,{data:body,headers:{"X-SEO-Auditor-Request":"1",Origin:base}});const data=await response.json();if(!response.ok())throw Error(`${response.status()} ${JSON.stringify(data)}`);return data;};
 const pause=ms=>new Promise(resolve=>setTimeout(resolve,ms));
 async function waitRules(id){for(let n=0;n<180;n++){const data=await api(`/api/audits/${id}/rules`);if(data.job?.status==="done")return data;await pause(500);}throw Error("Rule job timed out");}
 let scheduleID="";
 try{
  await page.goto(base+"/#access_token="+encodeURIComponent(token));await page.locator("#audit-form").waitFor();
  await page.locator('[name="mode"][value="site"]').check();await page.locator("#root-url").fill(root);await page.locator("#max-pages").fill("2");await page.locator("#max-depth").fill("0");
  const navigation=page.waitForURL(url=>/^\/audits\/[0-9a-f-]+$/.test(url.pathname));await page.locator("#submit").click();await navigation;const run=new URL(page.url()).pathname.split("/").pop();
  await page.waitForFunction(()=>document.querySelector("#run-status")?.textContent.includes("Завершено"),null,{timeout:60000});
  await page.locator("#tab-coverage").click();await page.locator("#coverage-rows tr").first().waitFor();await page.waitForFunction(()=>document.querySelector("#coverage-summary")?.textContent.includes("Відомі URL"));
  const coverage=await api(`/api/audits/${run}/coverage`);assert.equal(coverage.summary.known,6);assert.equal(coverage.meta.partial,true);assert(!JSON.stringify(coverage).includes("fixture-private-token"));
  await page.locator("#coverage-import").click();await page.locator("#coverage-file").setInputFiles({name:"gsc.csv",mimeType:"text/csv",buffer:Buffer.from(`Page,Clicks\n${root}/a?page=98765&token=fixture-csv-private,1`)});await page.locator("#coverage-important").check();await page.locator('#coverage-import-form button[type="submit"]').click();await page.locator("#coverage-import-dialog").waitFor({state:"hidden"});
  const imported=await api(`/api/audits/${run}/coverage`);assert.equal(imported.summary.gsc,1);assert(!JSON.stringify(imported).includes("fixture-csv-private"));
  await page.screenshot({path:path.join(artifacts,"coverage-desktop.png"),fullPage:true});
  await page.locator("#tab-facets").click();await page.waitForFunction(()=>document.querySelector("#facets-rows")?.textContent.includes("/a"));
  const families=await api(`/api/audits/${run}/facets`);assert.equal(families.items[0].variants,4);assert.equal(families.items[0].max_pagination,1000);assert.equal(families.items[0].audited,1);assert(!JSON.stringify(families).includes("98765"));
  await page.screenshot({path:path.join(artifacts,"facets-desktop.png"),fullPage:true});
  await page.setViewportSize({width:390,height:844});await page.screenshot({path:path.join(artifacts,"facets-mobile.png"),fullPage:true});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await page.setViewportSize({width:1440,height:1000});await page.goto(base+"/monitoring");await page.locator("#new-schedule").click();await page.locator("#schedule-name").fill("Правила E2E <img src=x onerror=window.__diagnosticXSS=1>");await page.locator("#schedule-urls").fill(root+"/a");
  const created=page.waitForResponse(r=>new URL(r.url()).pathname==="/api/schedules"&&r.request().method()==="POST");await page.locator("#save-schedule").click();scheduleID=(await(await created).json()).id;
  const scheduleRow=page.locator("#schedules tbody tr").filter({hasText:"Правила E2E"});await scheduleRow.getByRole("button",{name:"Власні правила аудиту",exact:true}).click();await page.locator("#rule-name").fill("HTTP <script>window.__diagnosticXSS=1</script>");await page.locator("#rule-pattern").fill("/a");await page.locator("#rule-severity").selectOption("critical");
  await page.locator('#rule-form button[type="submit"]').click();await page.waitForFunction(()=>document.querySelector("#rule-count")?.textContent.startsWith("1 /"));
  await page.screenshot({path:path.join(artifacts,"rules-editor-desktop.png"),fullPage:true});await page.setViewportSize({width:390,height:844});await page.screenshot({path:path.join(artifacts,"rules-editor-mobile.png"),fullPage:true});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await page.setViewportSize({width:1440,height:1000});await page.locator('[data-close="audit-rules-dialog"]').click();
  const runs=[];for(const status of [200,404,200]){fixtureStatus=status;const result=await api(`/api/schedules/${scheduleID}/run`,{});runs.push(result.id);const report=await waitRules(result.id);assert.equal(report.items[0].checks[0].state,status===200?"pass":"fail");}
  const alerts=await api(`/api/changes?run_id=${runs[2]}`);assert(alerts.items.some(v=>v.kind.startsWith("rule_")&&v.after_value.includes("Виправлено")));
  await page.goto(base+"/audits/"+runs[2]);await page.locator("#tab-rules").click();await page.waitForFunction(()=>document.querySelector("#rules-status")?.textContent.includes("Перевірено"));assert(await page.evaluate(()=>!window.__diagnosticXSS));
  await page.screenshot({path:path.join(artifacts,"rules-report-desktop.png"),fullPage:true});await page.setViewportSize({width:390,height:844});await page.screenshot({path:path.join(artifacts,"rules-report-mobile.png"),fullPage:true});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));assert.deepEqual(errors,[]);
  await fs.writeFile(path.join(artifacts,"verification.json"),JSON.stringify({run,scheduleID,runs,checks:["SEO preserved","coverage","GSC privacy","pagination buckets","facets counts","rule editor","new/resolved alerts","XSS","desktop/mobile"],errors},null,2));
  console.log("PASS: crawl coverage, GSC privacy, facets, custom rules, scheduled alerts, XSS, desktop/mobile");
 }finally{
  if(scheduleID){try{const schedules=await api("/api/schedules"),item=schedules.find(v=>v.id===scheduleID);if(item)await api(`/api/schedules/${scheduleID}/update`,{name:item.name,enabled:false,interval_hours:item.interval_hours,next_run_at:new Date(Date.now()+86400000).toISOString()});}catch(e){console.error("Failed to pause own fixture schedule:",e.message);}}
  await browser.close();await new Promise(resolve=>fixture.close(resolve));
 }
}
main().catch(e=>{console.error(e);process.exitCode=1;});
