"use strict";
(() => {
  const $ = id => document.getElementById(id);
  const labels = {running:"Виконується",pending:"Очікує",completed:"Завершено",completed_with_errors:"З помилками",failed:"Помилка",canceled:"Скасовано",abandoned:"Перервано",redirect:"Редирект",blocked_by_robots:"Robots blocked"};
  const defaults = ["safe_url","status_code","scan_status","title","title_width_px","title_status","description","description_width_px","description_status","description_mobile_status","h1","canonical_url","meta_robots","links_count","images_missing_alt","word_count","duration_ms"];
  const filters = {all:"Усі сторінки",errors:"Помилки", "4xx":"HTTP 4xx", "5xx":"HTTP 5xx",redirects:"Редиректи",robots_blocked:"Robots blocked",title_missing:"Title відсутній",title_recommended:"Title: Recommended",title_borderline:"Title: Borderline",title_high_risk:"Title: High truncation risk",description_missing:"Description відсутній",description_mobile_risk:"Description: Mobile risk",description_desktop_risk:"Description: Desktop risk",h1_missing:"H1 відсутній",h1_multiple:"Декілька H1",canonical_missing:"Canonical відсутній",canonical_other:"Non-self canonical",noindex:"Noindex / none",images_alt:"Зображення без alt",json_ld_absent:"JSON-LD відсутній",viewport_absent:"Viewport відсутній",truncated:"Обрізані метадані"};
  const runID = location.pathname.match(/^\/audits\/([a-f0-9-]{36})$/i)?.[1];
  let fields = [], columns = defaults.slice(), rows = [], cursors = [""], next = "", historyNext = "", historyCursor = "", pollTimer, refreshCount = 0, lastStatus = "", resultRequest = 0;
  const notice = message => { $("notice").textContent = message; $("notice").hidden = !message; };
  const text = value => value == null || value === "" ? "—" : typeof value === "boolean" ? (value ? "Так" : "Ні") : String(value);
  const el = (tag, value, cls) => { const node = document.createElement(tag); if(value != null) node.textContent = value; if(cls) node.className = cls; return node; };
  const statusBadge = status => el("span", labels[status] || status, "badge " + (status === "completed" ? "success" : status === "failed" ? "error" : ["redirect","blocked_by_robots","completed_with_errors","abandoned"].includes(status) ? "warning" : ""));
  const metricStatusKeys = new Set(["title_status", "description_status", "description_mobile_status"]);
  const metricBadge = value => el("span", text(value), "badge " + (["Recommended","Safe"].includes(value) ? "success" : ["Borderline","May truncate"].includes(value) ? "warning" : ["High truncation risk","Likely truncate","Missing"].includes(value) ? "error" : ""));
  async function api(path, body, token) {
    const headers = {"X-SEO-Auditor-Request":"1"};
    if(body !== undefined) headers["Content-Type"] = "application/json";
    if(token) headers.Authorization = "Bearer " + token;
    const response = await fetch(path, {method: body === undefined ? "GET" : "POST", headers, credentials:"same-origin",body:body === undefined ? undefined : JSON.stringify(body)});
    const data = await response.json();
    if(!response.ok) throw new Error(data.error?.message || "Запит не виконано");
    return data;
  }
  function formatDate(value) { return value ? new Date(value).toLocaleString("uk-UA") : "—"; }
  function elapsed(start, finish) { const s = Math.max(0,Math.round(((finish ? new Date(finish) : new Date()) - new Date(start))/1000)); return s < 60 ? s+" с" : Math.floor(s/60)+" хв "+s%60+" с"; }
  async function loadHistory() {
    const data = await api("/api/audits?cursor="+encodeURIComponent(historyCursor));
    const list = $("history-list"); list.replaceChildren(); historyNext = data.next;
    for(const run of data.runs) {
      const link = el("a",null,"history-row"); link.href = "/audits/"+run.id;
      for(const [label,value] of [["Початок",formatDate(run.started_at)],["Аудит",run.id.slice(0,8)],["Статус",run.status],["URL",run.total],["Успішно",run.successful],["Помилки",run.failed]]) {
        const cell = el("span"); cell.append(el("small",label)); cell.append(label === "Статус" ? statusBadge(value) : el("span",value));
        if(label === "Початок") cell.append(el("small",elapsed(run.started_at,run.finished_at)));
        link.append(cell);
      }
      link.title = "Тривалість: "+elapsed(run.started_at,run.finished_at); link.append(el("span","→")); list.append(link);
    }
    if(!data.runs.length) list.append(el("div","Аудитів ще немає","empty"));
    $("history-next").hidden = !historyNext;
  }
  function countURLs() {
    const lines = $("urls").value.split(/\r?\n/).map(s=>s.trim()).filter(Boolean); const set = new Set(); let duplicates=0,invalid=0;
    for(const line of lines) { try { const u = new URL(line); u.hash=""; if(!["http:","https:"].includes(u.protocol)||u.username||u.password) throw Error(); if(set.has(u.href)) duplicates++; else set.add(u.href); } catch { invalid++; } }
    $("url-count").textContent=set.size; $("duplicate-count").textContent=duplicates; $("invalid-count").textContent=invalid;
  }
  async function submit(event) {
    event.preventDefault(); notice(""); $("submit").disabled=true;
    try { const run = await api("/api/audits",{urls:$("urls").value}); $("urls").value=""; location.assign("/audits/"+run.id); }
    catch(error) { notice(error.message); $("submit").disabled=false; }
  }
  function renderSummary(p, analytics) {
    const run = p.run; const summary = $("summary"); summary.replaceChildren();
    const outcomes = analytics?.groups.find(g=>g.label==="Результати")?.buckets || [];
    for(const [name,value] of [["Усього URL",run.total],["Оброблено",p.counts.completed+p.counts.failed],["Успішно",p.counts.completed],["Помилки",p.counts.failed],["Редиректи",outcomes[1]?.value ?? "—"],["Robots blocked",outcomes[2]?.value ?? "—"]]) {
      const item=el("div",null,"summary-item"); item.append(el("span",name,"summary-label"),el("strong",value,"summary-value")); summary.append(item);
    }
  }
  let analyticsData;
  async function analytics() {
    analyticsData = await api(`/api/audits/${runID}/analytics`);
    $("parsed-count").textContent = `${analyticsData.parsed} HTML сторінок · ${analyticsData.total} результатів`;
    const charts=$("analytics"); charts.replaceChildren();
    for(const group of analyticsData.groups) {
      const chart=el("section",null,"chart"); chart.append(el("h3",group.label)); const max=Math.max(1,...group.buckets.map(b=>b.value));
      for(const bucket of group.buckets) {
        const row=el("div",null,"chart-row"), label=el("div",null,"chart-label"), bar=el("progress");
        label.append(el("span",bucket.label),el("strong",Number(bucket.value.toFixed(1)).toLocaleString("uk-UA")));
        bar.max=max; bar.value=bucket.value; bar.setAttribute("aria-label",bucket.label); row.append(label,bar); chart.append(row);
      }
      if(!group.buckets.length) chart.append(el("span","Немає","muted")); charts.append(chart);
    }
  }
  async function progress() {
    clearTimeout(pollTimer);
    try {
      const p=await api(`/api/audits/${runID}/progress`); const run=p.run;
      const badge=statusBadge(run.status); badge.id="run-status"; $("run-status").replaceWith(badge);
      $("run-reference").textContent="АУДИТ / "+run.id;
      $("run-time").textContent=`${formatDate(run.started_at)} · ${elapsed(run.started_at,run.finished_at)}`;
      $("run-progress").value=p.percentage; $("percentage").textContent=Math.round(p.percentage)+"%";
      $("target-states").replaceChildren();
      for(const [status,n] of Object.entries(p.counts)){ const span=el("span",labels[status]+" "); span.append(el("strong",n)); $("target-states").append(span); }
      $("cancel").hidden=run.status!=="running"; $("resume").hidden=!run.resumable;
      for(const format of ["html","csv"]) { $(format+"-export").setAttribute("aria-disabled",String(run.status==="running")); }
      if(refreshCount++%10===0 || lastStatus!==run.status) { await analytics(); await results(); }
      lastStatus=run.status; renderSummary(p,analyticsData);
      if(run.status==="running") pollTimer=setTimeout(progress,1000);
    } catch(error) { notice(error.message); pollTimer=setTimeout(progress,3000); }
  }
  async function results() {
    const serial=++resultRequest;
    const query=new URLSearchParams({filter:$("filter").value,search:$("search").value,limit:$("page-size").value,after:cursors[cursors.length-1]});
    const data=await api(`/api/audits/${runID}/results?${query}`);
    if(serial!==resultRequest) return;
    rows=data.rows; next=data.next; renderTable(); $("next").disabled=!next; $("previous").disabled=cursors.length===1;
    $("page-number").textContent=cursors.length; $("row-count").textContent=rows.length+" результатів на сторінці";
  }
  function renderTable() {
    const selected=fields.filter(f=>columns.includes(f.key)); const header=el("tr");
    for(const field of selected) header.append(el("th",field.label)); $("results-table").querySelector("thead").replaceChildren(header);
    const body=$("results-table").querySelector("tbody"); body.replaceChildren();
    for(const record of rows) {
      const row=el("tr"); row.tabIndex=0;
      for(const field of selected) { const td=el("td"); td.append(field.key==="scan_status" ? statusBadge(record[field.key]) : metricStatusKeys.has(field.key) ? metricBadge(record[field.key]) : el("span",text(record[field.key]),"cell-text")); row.append(td); }
      row.addEventListener("click",()=>details(record)); row.addEventListener("keydown",e=>{if(e.key==="Enter") details(record);}); body.append(row);
    }
    if(!rows.length) { const row=el("tr"),td=el("td","Результатів за цим фільтром немає","empty"); td.colSpan=Math.max(1,selected.length); row.append(td); body.append(row); }
  }
  function details(record) {
    $("detail-url").textContent=text(record.safe_url); const content=$("detail-content"); content.replaceChildren();
    let group="",list;
    for(const field of fields) { if(group!==field.group) { group=field.group; list=el("dl"); content.append(el("h3",group),list); } const value=el("dd"); value.append(metricStatusKeys.has(field.key) ? metricBadge(record[field.key]) : document.createTextNode(text(record[field.key]))); list.append(el("dt",field.label),value); }
    $("details-dialog").showModal();
  }
  function columnOptions() {
    const container=$("column-options"); container.replaceChildren();
    for(const field of fields) {
      const label=el("label"), input=el("input"); input.type="checkbox"; input.checked=columns.includes(field.key);
      input.addEventListener("change",()=>{ if(input.checked) columns.push(field.key); else columns=columns.filter(k=>k!==field.key); if(!columns.length) { columns=["safe_url"]; columnOptions(); } try{localStorage.setItem("seo-columns",JSON.stringify(columns));}catch{} renderTable(); });
      label.append(input,document.createTextNode(field.label)); container.append(label);
    }
  }
  const safely = fn => async (...args) => { try { await fn(...args); } catch(error) { notice(error.message); } };
  async function init() {
    const fragment=new URLSearchParams(location.hash.slice(1)); const token=fragment.get("access_token");
    if(token) { window.history.replaceState(null,"",location.pathname+location.search); await api("/api/session",{},token); }
    const schema=await api("/api/schema"); fields=schema.fields;
    try{const saved=JSON.parse(localStorage.getItem("seo-columns"));if(Array.isArray(saved)&&saved.length) columns=saved.filter(key=>fields.some(f=>f.key===key));}catch{}
    if(!columns.length) columns=defaults.slice();
    $("limit-label").textContent="До "+schema.max_urls.toLocaleString("uk-UA")+" URL";
    $("home").hidden=Boolean(runID)||location.pathname==="/audits"; $("report").hidden=!runID; $("history-section").hidden=Boolean(runID);
    $(runID||location.pathname==="/audits"?"history-nav":"new-nav").classList.add("active");
    for(const [key,label] of Object.entries(filters)){ const option=el("option",label);option.value=key;$("filter").append(option); }
    $("urls").addEventListener("input",countURLs); $("clear").onclick=()=>{$("urls").value="";countURLs();$("urls").focus();}; $("audit-form").onsubmit=submit;
    $("history-refresh").onclick=safely(()=>{historyCursor="";return loadHistory();}); $("history-next").onclick=safely(()=>{historyCursor=historyNext;return loadHistory();});
    document.querySelectorAll("[data-close]").forEach(button=>button.onclick=()=>$(button.dataset.close).close());
    if(!runID) { await loadHistory(); return; }
    for(const format of ["html","csv"]) $(format+"-export").href=`/api/audits/${runID}/export/${format}`;
    for(const view of ["overview","pages"]) $("tab-"+view).onclick=()=>{for(const name of ["overview","pages"]) { $(name).hidden=name!==view; $("tab-"+name).setAttribute("aria-selected",String(name===view)); }};
    $("columns-button").onclick=()=>{columnOptions();$("columns-dialog").showModal();}; $("reset-columns").onclick=()=>{columns=defaults.slice();try{localStorage.removeItem("seo-columns");}catch{}columnOptions();renderTable();};
    $("cancel").onclick=safely(async()=>{ $("cancel").disabled=true; try{await api(`/api/audits/${runID}/cancel`,{});$("cancel").textContent="Завершення…";await progress();}finally{$("cancel").disabled=false;} });
    $("resume").onclick=safely(async()=>{ $("resume").disabled=true;try{await api(`/api/audits/${runID}/resume`,{});await progress();}finally{$("resume").disabled=false;} });
    const reset=()=>{cursors=[""];return results();}; $("filter").onchange=safely(reset);$("page-size").onchange=safely(reset);
    let searchTimer; $("search").oninput=()=>{clearTimeout(searchTimer);searchTimer=setTimeout(safely(reset),300);};
    $("next").onclick=safely(()=>{cursors.push(next);return results();});$("previous").onclick=safely(()=>{cursors.pop();return results();});
    await progress();
  }
  init().catch(error=>notice(error.message));
})();
