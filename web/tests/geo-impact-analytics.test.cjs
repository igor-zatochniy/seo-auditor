const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

class Element {
  constructor(tag = "div", text = "") { this.tag = tag; this.text = text; this.children = []; this.value = ""; this.disabled = false; this.title = ""; }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  get textContent() { return this.text + this.children.map(child => child.textContent).join(" "); }
  set textContent(value) { this.text = String(value); this.children = []; }
  querySelector() { return this.body ||= new Element("tbody"); }
}

test("Impact Analytics keeps sources separate, escapes text, and paginates by Target ID", async () => {
  const nodes = new Map(), listeners = new Map(), requests = [], notices = [];
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, new Element(id.startsWith("impact-gsc-") || id === "impact-ga4" ? "select" : "div"));
    return nodes.get(id);
  };
  const datasets = [
    {id:"web-1",source:"gsc_web",period_start:"2026-09-01",period_end:"2026-09-30",country:"US",device:"all",row_count:2},
    {id:"ai-1",source:"gsc_ai",period_start:"2026-09-01",period_end:"2026-09-30",country:"US",device:"all",row_count:1},
    {id:"ga-1",source:"ga4",period_start:"2026-09-01",period_end:"2026-09-30",country:"US",device:"all",row_count:2}
  ];
  const selected = {gsc_web:datasets[0],gsc_ai:datasets[1],ga4:datasets[2]};
  const malicious = "<img src=x onerror=alert(1)>";
  const makeResponse = (after) => ({
    datasets, selected, baseline:{period_start:"2026-08-01",period_end:"2026-08-31"}, bot_import_id:"bot-1",
    items:[{target_id:after?2:1,url:after?"https://example.com/two":malicious,title:malicious,http_status:200,scan_status:"completed",url_match:"exact",gsc_impressions:1000,gsc_clicks:10,gsc_ctr:.01,gsc_position:4,opportunity_clicks_estimate:40,opportunity_confidence:"low",gsc_ai_impressions:700,ai_referral_sessions:12,verified_bot_visits:3}],
    next:after?0:2,
    warnings:["AI referral sessions are not citations"]
  });
  const api = async url => { requests.push(url);const query=new URLSearchParams(url.split("?")[1]);return makeResponse(Number(query.get("after"))); };
  const window = {addEventListener(name,fn){listeners.set(name,fn);}};
  const source=fs.readFileSync(path.join(__dirname,"../static/geo-impact-analytics.js"),"utf8");
  vm.runInNewContext(source,{window,document:{getElementById:$},URLSearchParams});
  await window.seoImpactAnalytics.init({api,el:(tag,text)=>new Element(tag,text==null?"":String(text)),reportID:"report-1",notice:message=>notices.push(message)});

  assert.equal($("impact-analytics-table").querySelector().children.length,1);
  const row=$("impact-analytics-table").querySelector().children[0];
  assert(row.textContent.includes(malicious));
  assert(row.textContent.includes("12"));
  assert($("impact-analytics-warnings").textContent.includes("not citations"));
  assert.equal($("impact-gsc-web").value,"web-1");
  assert.equal($("impact-gsc-ai").value,"ai-1");
  assert.equal($("impact-ga4").value,"ga-1");

  await $("impact-analytics-next").onclick();
  assert(requests.some(url=>url.includes("after=2")));
  assert.equal($("impact-analytics-table").querySelector().children[0].textContent.includes("/two"),true);
  await $("impact-analytics-prev").onclick();
  assert(requests.filter(url=>url.includes("after=0")).length>=2);
  assert.equal(notices.filter(Boolean).length,0);
});
