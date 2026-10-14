const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

class Element {
  constructor(tag = "div", text = "") { this.tag = tag; this.text = text; this.children = []; this.dataset = {}; this.listeners = {}; this.disabled = false; this.className = ""; }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  setAttribute() {}
  addEventListener(name, fn) { this.listeners[name] = fn; }
  querySelector() { if (!this.body) { this.body = new Element("tbody"); this.children.push(this.body); } return this.body; }
  get textContent() { return this.text + this.children.map(child => child.textContent).join(" "); }
  set textContent(value) { this.text = String(value); this.children = []; }
}

test("AI discoverability renders safe evidence, desired mismatch, and paginates", async () => {
  const nodes = new Map(), requests = [];
  const $ = id => nodes.get(id) || (nodes.set(id, new Element(id)), nodes.get(id));
  const malicious = "<img src=x onerror=alert(1)>";
  const spec = {token:"Google-Extended",provider:"Google",purpose:"training_grounding",purpose_label:"Керування використанням контенту",has_http_user_agent:false,documentation_url:"https://developers.google.com/crawling/docs/crawlers-fetchers/google-common-crawlers"};
  let page = 0;
  const api = async url => {
    requests.push(url);
    if (url === "/api/geo/ai-policy-specs") return [spec];
    const after = new URLSearchParams(url.split("?")[1]).get("after");
    page = after ? 1 : 0;
    return {
      domain:"example.com",desired_configured:true,desired_policy:{"google-extended":"blocked"},
      baseline_run_id:"run-old",pages_total:2,pages_with_matrix:2,
      summary:[{token:"Google-Extended",allowed:1,blocked:1,unknown:0,mismatches:1,conflicts:0,drift:1}],
      items:[{target_id:page?2:1,url:page?"https://example.com/two":malicious,scan_status:"completed",http_status:200,mismatches:1,conflicts:0,policy_changes:1,
        controls:[{spec,evidence:{token:"Google-Extended",provider:"Google",purpose:"training_grounding",status:"allowed",directive:malicious,robots_url:"https://example.com/robots.txt",checked_at:"2026-10-14T00:00:00Z",robots_http_status:200,confidence:"robots_parsed",explanation:malicious},desired:"blocked",mismatch:true,previous_status:"blocked",drift:"changed"}]}],
      next_after:page?null:2,limit:50};
  };
  const window = {};
  const document = {getElementById:$,createElement:(tag)=>new Element(tag)};
  const source = fs.readFileSync(path.join(__dirname,"../static/geo-ai-policy.js"),"utf8");
  vm.runInNewContext(source,{window,document,URLSearchParams});
  await window.seoAIPolicy.init({api,el:(tag,text)=>new Element(tag,text==null?"":String(text)),reportID:"report-1"});

  assert.equal($("ai-policy-pages").querySelector().children.length,1);
  assert($("ai-policy-pages").textContent.includes(malicious));
  assert($("ai-policy-meta").textContent.includes("не підтверджує доступність"));
  assert.equal($("ai-policy-next").disabled,false);
  await $("ai-policy-next").listeners.click();
  assert(requests.some(url=>url.includes("after=2")));
  assert($("ai-policy-pages").textContent.includes("https://example.com/two"));
  await $("ai-policy-prev").listeners.click();
  assert(requests.filter(url=>url.endsWith("limit=50")).length >= 2);
});
