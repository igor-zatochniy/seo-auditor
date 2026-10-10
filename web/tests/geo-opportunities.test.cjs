const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../static/geo-opportunities.js"), "utf8");

class Element {
  constructor(tag = "div", text = "") { this.tag = tag; this.text = text; this.children = []; this.value = ""; }
  append(...items) { this.children.push(...items); if (this.tag === "select" && this.children.length === 1) this.value = String(items[0].value); }
  replaceChildren(...items) { this.children = items; }
  get options() { return this.children; }
  get textContent() { return this.text + this.children.map(child => child.textContent).join(" "); }
  set textContent(value) { this.text = String(value); this.children = []; }
  querySelector() { return this.body ||= new Element("tbody"); }
  close() { this.open = false; }
  showModal() { this.open = true; }
  getContext() { return {clearRect() {}, fillText() {}, fillRect() {}}; }
}

async function fixture() {
  const nodes = new Map(), events = new Map(), requests = [], notices = [];
  const $ = id => { if (!nodes.has(id)) nodes.set(id, new Element(id.includes("query") || id.includes("baseline") ? "select" : "div")); return nodes.get(id); };
  for (const [id, value] of Object.entries({"opportunity-priority":"all", "impact-source":"ai_observed", "impact-engine":"chatgpt", "impact-country":"US", "impact-device":"all", "impact-scope":"url"})) $(id).value = value;
  const row = {ordinal:1, mapping_mode:"automatic", result:{query:"chargeback protection", target_id:1, target_url:"https://example.com/one"}};
  const data = {report:{domain:"example.com", created_at:"2026-10-02"}, matching_model:"test", generated_at:"2026-10-02", plan:{model:"test", tasks:[], links:[], warnings:[]}, rows:[row], targets:[{target_id:1,http_status:200,scan_status:"completed",url:row.result.target_url}]};
  let pendingSeries, failRefresh = false;
  const seriesResult = {query:row.result.query,scope:"url",points:[{period_start:"2026-09-01",period_end:"2026-09-30",impressions:42,clicks:2,matched_rows:1}]};
  const api = async (url, payload) => {
    requests.push({url,payload});
    if (url.includes("visibility-series")) { const pending = pendingSeries; pendingSeries = null; return pending || structuredClone(seriesResult); }
    if (url.endsWith("opportunities")) return structuredClone(data);
    if (url === "/api/geo/reports") return [{id:"baseline",domain:"example.com",created_at:"2026-10-01",model:"test"}];
    if (url.endsWith("target-history")) return [];
    if (url.endsWith("target")) return {revision:1};
    if (url.includes("/impact?")) return {baseline:{model:"test"},current:{model:"test"},same_model:true,items:[]};
    throw new Error(url);
  };
  const window = {
    addEventListener(name, fn) { events.set(name, fn); },
    dispatchEvent(event) { events.get(event.type)?.(event); }
  };
  vm.runInNewContext(source, {window, document:{getElementById:$}, URLSearchParams, crypto:{randomUUID:()=>"test-decision"}, Event:class {constructor(type) {this.type=type;}}});
  await window.seoOpportunities.init({api,el:(tag,text)=>new Element(tag,text == null ? "" : String(text)),reportID:"current",rows:[row],notice:message=>notices.push(message),refreshRows:async()=>{if(failRefresh)throw new Error("Refresh failed");}});
  return {$, requests, notices, events, seriesResult, window, setPendingSeries(promise){pendingSeries=promise;}, setFailRefresh(){failRefresh=true;}};
}

test("Зміна області прибирає старі AI-показники та відкидає запізнілу відповідь", async () => {
  const f = await fixture();
  assert.match(f.$("impact-series-table").querySelector().textContent, /42/);
  let release;
  f.setPendingSeries(new Promise(resolve => { release = resolve; }));
  const pending = f.$("impact-series-refresh").onclick();
  f.$("impact-country").value = "UA";
  f.$("impact-country").oninput?.();
  release(f.seriesResult);
  await pending;
  assert.doesNotMatch(f.$("impact-series-table").querySelector().textContent, /42/);
});

test("Збережена ручна ціль не залишає старий графік навіть за помилки оновлення таблиці", async () => {
  const f = await fixture();
  f.setFailRefresh();
  await f.$("target-edit-open").onclick();
  f.$("target-edit-page").value = "1";
  f.$("target-edit-reason").value = "Перевірено редактором";
  await f.$("target-edit-form").onsubmit({preventDefault() {}});
  assert.doesNotMatch(f.$("impact-series-table").querySelector().textContent, /42/);
  assert.equal(f.$("target-edit-status").textContent, "Refresh failed");
});

test("Зміни імпортів оновлюють графік без перезавантаження сторінки", async () => {
  const f = await fixture();
  const handler = f.events.get("geo:visibility-changed");
  assert.equal(typeof handler, "function");
  const before = f.requests.length;
  await handler();
  assert(f.requests.slice(before).some(request => request.url.includes("visibility-series")));
});

test("Збереження й видалення CSV сповіщають графік тільки після успішного API-запиту", async () => {
  const nodes = new Map(), changes = [];
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, new Element(id === "visibility-import-select" ? "select" : "div"));
    return nodes.get(id);
  };
  for (const [id, value] of Object.entries({"visibility-source":"ai_observed", "visibility-engine":"chatgpt", "visibility-start":"2026-09-01", "visibility-end":"2026-09-30", "visibility-country":"US", "visibility-device":"all"})) $(id).value = value;
  $("visibility-file").files = [{size:10,text:async()=>"Query,Impressions\nchargeback protection,42\n"}];
  const item = {id:"fixture",source:"ai_observed",engine:"chatgpt",period_start:"2026-09-01",period_end:"2026-09-30",country:"US",device:"all",row_count:1};
  let exists = false, fail = false;
  const api = async (url, payload) => {
    if (payload && fail) throw new Error("Import failed");
    if (url.endsWith("/delete")) {exists=false;return {};}
    if (payload) {exists=true;return item;}
    if (url.endsWith("/imports")) return exists ? [item] : [];
    return {import:item,rows:[],next:0};
  };
  const window = {confirm:()=>true,dispatchEvent:event=>changes.push(event.type)};
  const visibilitySource = fs.readFileSync(path.join(__dirname, "../static/geo-visibility.js"), "utf8");
  vm.runInNewContext(visibilitySource, {window,document:{getElementById:$},crypto:{randomUUID:()=>"fixture"},Event:class {constructor(type){this.type=type;}}});
  await window.seoVisibility.init({api,el:(tag,text)=>new Element(tag,text == null ? "" : String(text)),reportID:"current",rows:[],notice() {}});
  await $("visibility-import-form").onsubmit({preventDefault() {}});
  assert.deepEqual(changes, ["geo:visibility-changed"]);
  await $("visibility-delete").onclick();
  assert.deepEqual(changes, ["geo:visibility-changed","geo:visibility-changed"]);
  fail=true;
  await $("visibility-import-form").onsubmit({preventDefault() {}});
  assert.equal(changes.length, 2);
});
