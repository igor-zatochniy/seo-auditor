const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

class Element {
  constructor(tag = "div") { this.tag = tag; this.children = []; this.hidden = false; this.disabled = false; this.value = ""; this.classList = {add() {}}; }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return [this.text || "", ...this.children.map(child => child.textContent)].join(" "); }
  set innerHTML(_) { throw new Error("Untrusted content must not be assigned as HTML"); }
}

test("AI fetch diagnostics separates simulations from logs and escapes response text", async () => {
	const nodes = new Map(), calls = [];
	let failNext = false;
  const get = id => nodes.get(id) || (nodes.set(id, new Element(id)), nodes.get(id));
  const malicious = '<img src=x onerror="alert(1)">';
  const observation = {status:"html_received",robots_status:"allowed",http_status:200,content_type:"text/html",html_content_type:true,visible_words:12,body_bytes:120,explanation:malicious,redirects:[]};
  const fetch = async (url, options = {}) => {
    calls.push({url, options});
    let data;
    if (url === "/api/bots/networks" || url === "/api/bots/imports") data = [];
    else if (url === "/api/bots/fetch-diagnostics" && failNext) return {ok:false,json:async()=>({error:{message:"Мережа недоступна"}})};
    else if (url === "/api/bots/fetch-diagnostics") data = {
      bot:"OAI-SearchBot",url:"https://example.com/",auditor_simulation:observation,
      bot_simulation:{...observation,status:"challenge_suspected",challenge_suspected:true,challenge_signals:["captcha_required"]},
      real_log_observations:[{status:"observed",bot:"OAI-SearchBot",verification:"matched_range",visits:2,failures:2,http_statuses:[403,429],explanation:malicious,repeated_429_503:true}],
      discrepancy:true,disclaimer:"User-Agent is only a simulation"
    };
    else throw new Error(url);
    return {ok:true,json:async()=>data};
  };
  const document = {getElementById:get,createElement:tag=>new Element(tag)};
  const source = fs.readFileSync(path.join(__dirname,"../static/bots.js"),"utf8");
  vm.runInNewContext(source,{document,fetch,location:{hash:"",pathname:"/bots"},URLSearchParams});
  await new Promise(resolve=>setImmediate(resolve));

  get("ai-fetch-url").value = "https://example.com/";
  get("ai-fetch-bot").value = "oai-searchbot";
  await get("ai-fetch-form").onsubmit({preventDefault() {}});

  const report = get("ai-fetch-report");
  assert.equal(report.hidden,false);
  assert(report.textContent.includes(malicious));
  assert(report.textContent.includes("Симуляція User-Agent бота"));
  assert(report.textContent.includes("Фактичні події з імпортованих журналів"));
  assert(report.textContent.includes("429"));
  assert.equal(get("notice").textContent.trim(),"");
  assert.equal(get("ai-fetch-submit").disabled,false);
  assert.deepEqual(JSON.parse(calls.find(call=>call.url==="/api/bots/fetch-diagnostics").options.body),{url:"https://example.com/",bot:"oai-searchbot"});

	get("ai-fetch-url").value = "https://other.example/";
	failNext = true;
	await get("ai-fetch-form").onsubmit({preventDefault() {}});
	assert.equal(report.hidden,true,"failed checks must not leave the previous URL's report visible");
	assert.equal(report.textContent.trim(),"");
	assert.equal(get("ai-fetch-status").textContent,"Перевірку не завершено");
	assert.equal(get("notice").textContent,"Мережа недоступна");
	assert.equal(get("ai-fetch-submit").disabled,false);
});
