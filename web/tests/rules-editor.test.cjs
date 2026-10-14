const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") {
    this.tag = tag;
    this.children = [];
    this.value = "";
    this.dataset = {};
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
  }
  set innerHTML(value) {
    this.html = value;
    for (const match of value.matchAll(/id="([^"]+)"/g)) nodes.set(match[1], new Element(match[1]));
    const select = nodes.get("rule-kind");
    for (const match of value.matchAll(/<option value="([^"]+)">([^<]*)<\/option>/g)) {
      const option = new Element("option");
      option.value = match[1];
      option.textContent = match[2];
      select.append(option);
    }
  }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  setAttribute() {}
  showModal() { this.open = true; }
  close() { this.open = false; }
  querySelector() { return new Element("button"); }
  get textContent() { return this.text || ""; }
  set textContent(value) { this.text = String(value); }
}

const nodes = new Map();

test("custom rules editor sends bounded conditions and renders names as text", async () => {
  nodes.clear();
  const listeners = new Map();
  const calls = [];
  let saved;
  let exceptions = [{fingerprint:"a".repeat(64),safe_url:"https://example.com/product",reason:"Fixture",expires_at:"2026-10-15T00:00:00Z"}];
  const response = (value, ok = true) => ({ok, json: async () => value});
  const document = {
    getElementById: id => nodes.get(id) || (nodes.set(id, new Element(id)), nodes.get(id)),
    createElement: tag => new Element(tag),
    addEventListener: (name, callback) => listeners.set(name, callback),
    body: {append() {}}
  };
  const fetch = async (url, options = {}) => {
    calls.push({url, options});
    if (options.method === "DELETE") {
      exceptions = [];
      return response({ok:true});
    }
    if (options.method === "POST") {
      saved = {...JSON.parse(options.body), revision: 1};
      return response(saved);
    }
    if (url.endsWith("/exceptions")) return response(exceptions);
    if (saved) return response([{...saved, last_notified_at: null}]);
    return response([]);
  };
  const source = fs.readFileSync(path.join(__dirname, "../static/rules-editor.js"), "utf8");
  vm.runInNewContext(source, {document, fetch, crypto: {randomUUID: () => "rule-id"}, URLSearchParams});
  await listeners.get("seo:rules-edit")({detail: {id: "schedule-id", name: "Project"}});

  assert(nodes.get("rule-kind").children.some(option => option.value === "oai_search_allowed"));
  nodes.get("rule-name").value = "<img src=x onerror=alert(1)>";
  nodes.get("rule-pattern").value = "/products/*";
  nodes.get("rule-kind").value = "content_drop";
  nodes.get("rule-threshold").value = "40";
  nodes.get("rule-consecutive").value = "2";
  await nodes.get("rule-form").onsubmit({preventDefault() {}, target: nodes.get("rule-form")});

  assert.equal(saved.operator, "decrease_percent");
  assert.equal(saved.threshold, 40);
  assert.equal(saved.consecutive_runs, 2);
  assert.equal(saved.kind, "content_drop");
  assert(calls.some(call => call.options.method === "POST"));
  assert(nodes.get("rule-list").children[0].textContent.includes("<img src=x onerror=alert(1)>"));
  await new Promise(resolve => setImmediate(resolve));
  await nodes.get("exception-list").children[0].children[2].onclick();
  const deletion = calls.find(call => call.options.method === "DELETE");
  assert.equal(deletion.options.headers["Content-Type"],"application/json");
  assert.equal(deletion.options.headers["X-SEO-Auditor-Request"],"1");
  assert.equal(nodes.get("exception-list").children.length,0);
});

test("late exception response cannot replace the selected rule's exceptions", async () => {
  nodes.clear();
  const listeners = new Map();
  const response = value => ({ok:true,json:async()=>value});
  const rules = ["a","b"].map(id => ({id,name:"Rule "+id,pattern:"/*",kind:"http_200",enabled:true}));
  let finishFirst;
  const first = new Promise(resolve => { finishFirst = resolve; });
  const document = {
    getElementById:id=>nodes.get(id)||(nodes.set(id,new Element(id)),nodes.get(id)),
    createElement:tag=>new Element(tag),addEventListener:(name,callback)=>listeners.set(name,callback),body:{append(){}}
  };
  const fetch = async url => {
    if (url.endsWith("/rules/a/exceptions")) return first;
    if (url.endsWith("/rules/b/exceptions")) return response([{fingerprint:"b".repeat(64),safe_url:"https://example.com/b",reason:"B",expires_at:"2026-10-15T00:00:00Z"}]);
    return response(rules);
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname,"../static/rules-editor.js"),"utf8"),{document,fetch,crypto:{randomUUID:()=>"new-rule"}});
  await listeners.get("seo:rules-edit")({detail:{id:"schedule-id",name:"Project"}});
  nodes.get("rule-list").children[0].onclick();
  nodes.get("rule-list").children[1].onclick();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(nodes.get("exception-list").children[0].children[0].textContent,"https://example.com/b");
  finishFirst(response([{fingerprint:"a".repeat(64),safe_url:"https://example.com/a",reason:"A",expires_at:"2026-10-15T00:00:00Z"}]));
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(nodes.get("exception-list").children[0].children[0].textContent,"https://example.com/b");
});
