"use strict";
(() => {
  const $ = id => document.getElementById(id);
  const el = (tag, value, cls) => { const node = document.createElement(tag); if(value != null) node.textContent = String(value); if(cls) node.className = cls; return node; };
  const intents = {commercial:"Комерційне порівняння",transactional:"Транзакційний",informational:"Інформаційний",unknown:"Не визначено"};
  const levels = {strong:"Сильні сигнали",medium:"Часткові сигнали",weak:"Слабкі сигнали",unknown:"Недостатньо даних"};
  const reportID = location.pathname.match(/^\/geo\/([a-f0-9-]{36})$/i)?.[1];
  let rows = [], page = 0, current, requestID, lastPayload;
  const notice = message => { $("notice").textContent = message; $("notice").hidden = !message; };
  const date = value => value ? new Date(value).toLocaleString("uk-UA") : "—";
  async function api(path, body, token) {
    const headers = {"X-SEO-Auditor-Request":"1"};
    if(body !== undefined) headers["Content-Type"] = "application/json";
    if(token) headers.Authorization = "Bearer " + token;
    const response = await fetch(path,{method:body === undefined ? "GET" : "POST",headers,credentials:"same-origin",body:body === undefined ? undefined : JSON.stringify(body)});
    const data = await response.json();
    if(!response.ok) throw new Error(data.error?.message || "Не вдалося виконати запит");
    return data;
  }
  const safe = fn => async (...args) => { try { notice(""); await fn(...args); } catch(error) { notice(error.message); } };
  function badge(label, kind) { return el("span",label,"badge " + (kind || "")); }
  async function history() {
    const items = await api("/api/geo/reports");
    $("geo-history").replaceChildren();
    for(const item of items) {
      const link = el("a",null,"geo-history-row"); link.href = "/geo/"+item.id;
      for(const [label,value] of [["Домен / бренд",item.domain+(item.brand ? " · "+item.brand : "")],["Створено",date(item.created_at)],["Запитів",item.query_count],["Зіставлено",item.matched_count]]) {
        const cell = el("span"); cell.append(el("small",label),el("span",value)); link.append(cell);
      }
      $("geo-history").append(link);
    }
    if(!items.length) $("geo-history").append(el("p","Звітів ще немає","empty"));
  }
  async function submit(event) {
    event.preventDefault(); notice("");
    const input = {source_run_id:$("source-run").value.trim(),domain:$("domain").value.trim(),brand:$("brand").value.trim(),queries:$("queries").value};
    const signature = JSON.stringify(input);
    if(signature !== lastPayload) { requestID = crypto.randomUUID(); lastPayload = signature; }
    $("analyze").disabled = true; $("analyze").textContent = "Аналіз триває…";
    try { const report = await api("/api/geo/reports",{...input,id:requestID}); location.assign("/geo/"+report.id); }
    catch(error) { notice(error.message); }
    finally { $("analyze").disabled = false; $("analyze").textContent = "Проаналізувати"; }
  }
  function filteredRows() {
    const query = $("search-query").value.toLocaleLowerCase(), filter = $("geo-filter").value;
    return rows.filter(row => {
      const r = row.result;
      if(!(r.query+" "+r.target_url).toLocaleLowerCase().includes(query)) return false;
      return filter === "all" || (filter === "unmapped" && r.target_id == null) || (filter === "ambiguous" && r.ambiguous) ||
        (filter === "unchecked" && !row.citations.some(c=>c.citation!=="unknown"||c.brand_mention!=="unknown")) || r.level === filter;
    });
  }
  function renderTable() {
    const filtered = filteredRows(), limit = Number($("geo-page-size").value);
    page = Math.max(0,Math.min(page,Math.ceil(filtered.length/limit)-1));
    const body = $("geo-table").querySelector("tbody"); body.replaceChildren();
    for(const item of filtered.slice(page*limit,(page+1)*limit)) {
      const r = item.result, row = el("tr"); row.tabIndex = 0; row.setAttribute("aria-label","Деталі запиту: "+r.query);
      row.append(el("td",r.query),el("td",intents[r.intent]));
      const target = el("td",r.target_url || "Не знайдено в аудиті");
      if(r.ambiguous) target.append(badge("Близькі кандидати","warning"));
      row.append(target);
      const coverage = el("td",r.coverage+"%"), bar = el("progress"); bar.max=100; bar.value=r.coverage; bar.setAttribute("aria-label","Збіг термінів"); coverage.append(bar); row.append(coverage);
      const ready = el("td"); ready.append(badge(levels[r.level]+(r.readiness==null?"":" · "+r.readiness+"%"),r.level==="strong"?"success":r.level==="weak"?"error":"warning")); row.append(ready);
      row.append(el("td",r.gaps[0] || (r.readiness==null ? "Недостатньо даних для оцінки прогалин" : "Немає прогалин у перевірених сигналах")));
      const seen = item.citations.filter(c=>c.citation!=="unknown"||c.brand_mention!=="unknown");
      row.append(el("td",seen.length ? "Перевірено систем: "+seen.length : "Не перевірено"));
      row.onclick = () => details(item); row.onkeydown = event => { if(event.key==="Enter") details(item); }; body.append(row);
    }
    if(!filtered.length) { const row=el("tr"),cell=el("td","Немає результатів за цим фільтром","empty");cell.colSpan=7;row.append(cell);body.append(row); }
    $("geo-page-label").textContent=`Сторінка ${page+1} · Запитів: ${filtered.length}`;
    $("geo-prev").disabled=page===0; $("geo-next").disabled=(page+1)*limit>=filtered.length;
  }
  function addList(parent,title,items) {
    if(!items.length) return;
    parent.append(el("h3",title)); const list=el("ul"); for(const item of items) list.append(el("li",item)); parent.append(list);
  }
  function details(item) {
    current=item; const r=item.result; $("detail-query").textContent=r.query;
    const area=$("detail-analysis");area.replaceChildren();
    const facts=el("dl");
    for(const [name,value] of [["Намір (евристика)",intents[r.intent]],["Цільовий URL",r.target_url||"Не знайдено в аудиті"],["Збіг термінів",r.coverage+"%"],["Знайдені терміни",r.matched.join(", ")||"—"],["Не знайдені терміни",r.missing.join(", ")||"—"],["Запитів на цю сторінку",r.shared_queries||"—"],["Готовність",levels[r.level]+(r.readiness==null?"":" · "+r.readiness+"%")]]) facts.append(el("dt",name),el("dd",value));
    area.append(facts);
    addList(area,"Кандидати зі збереженого аудиту",r.alternatives.map(a=>`${a.coverage}% · ${a.url}`));
    addList(area,"Застереження",r.warnings);
    addList(area,"Прогалини та наступні дії",r.gaps);
    if(r.checks.length) area.append(el("h3","Перевірені сигнали"));
    for(const check of r.checks) { const section=el("div",null,"geo-check"),heading=el("strong");heading.append(badge(check.passed?"Виявлено":"Не виявлено",check.passed?"success":"warning"),el("span",check.name));section.append(heading,el("p",check.evidence||"—"));area.append(section); }
    $("citation-engine").value=item.citations[0]?.engine||"google_ai";loadCitation();$("geo-details").showModal();
  }
  function loadCitation() {
    const c=current.citations.find(item=>item.engine===$("citation-engine").value);
    $("citation-value").value=c?.citation||"unknown";$("mention-value").value=c?.brand_mention||"unknown";
    $("evidence-url").value=c?.evidence_url||"";$("citation-note").value=c?.note||"";
    $("citation-status").textContent=c?"Ручне спостереження: "+date(c.checked_at):"Спостереження ще не збережено";
  }
  async function saveCitation(event) {
    event.preventDefault(); $("save-citation").disabled=true;
    const item=current;
    try {
      const c=await api(`/api/geo/reports/${reportID}/queries/${item.ordinal}/citation`,{engine:$("citation-engine").value,citation:$("citation-value").value,brand_mention:$("mention-value").value,evidence_url:$("evidence-url").value.trim(),note:$("citation-note").value});
      item.citations=item.citations.filter(old=>old.engine!==c.engine).concat(c);if(current===item)loadCitation();renderTable();
    } catch(error) { $("citation-status").textContent=error.message; }
    finally { $("save-citation").disabled=false; }
  }
  async function init() {
    const token=new URLSearchParams(location.hash.slice(1)).get("access_token");
    if(token) { window.history.replaceState(null,"",location.pathname+location.search);await api("/api/session",{},token); }
    $("geo-home").hidden=Boolean(reportID);$("geo-report").hidden=!reportID;
    $("refresh-history").onclick=safe(history);$("geo-form").onsubmit=submit;
    $("citation-form").onsubmit=saveCitation;$("citation-engine").onchange=loadCitation;
    document.querySelectorAll("[data-close]").forEach(button=>button.onclick=()=>$(button.dataset.close).close());
    $("queries").oninput=()=>{$("query-count").textContent=$("queries").value.split(/\r?\n/).filter(line=>line.trim()).length+" / 1000 запитів";};
    $("clear-queries").onclick=()=>{$("queries").value="";$("queries").oninput();};
    if(reportID) {
      const data=await api("/api/geo/reports/"+reportID);rows=data.rows;
      const report=data.report;$("report-reference").textContent="GEO / "+report.id;
      $("report-title").textContent=report.brand||report.domain;$("report-meta").textContent=report.domain+" · "+date(report.created_at)+" · Сторінок: "+report.page_count;
      const source=el("a","Вихідний SEO-аудит");source.href="/audits/"+report.source_run_id;$("report-meta").append(document.createTextNode(" · "),source);
      for(const [label,value] of [["Усього запитів",report.query_count],["Зіставлено",report.matched_count],["Без сторінки",report.query_count-report.matched_count],["Близькі кандидати",report.ambiguous_count]]){const item=el("div",null,"summary-item");item.append(el("span",label,"summary-label"),el("strong",value,"summary-value"));$("geo-summary").append(item);}
      $("csv-export").href=`/api/geo/reports/${reportID}/export/csv`;
      $("search-query").oninput=()=>{page=0;renderTable();};$("geo-filter").onchange=()=>{page=0;renderTable();};$("geo-page-size").onchange=()=>{page=0;renderTable();};
      $("geo-prev").onclick=()=>{page--;renderTable();};$("geo-next").onclick=()=>{page++;renderTable();};renderTable();
    } else {
      const sources=await api("/api/geo/sources");
      for(const source of sources){const option=el("option",date(source.started_at)+" · "+source.total+" URL");option.value=source.id;$("source-list").append(option);}
      if(sources.length)$("source-run").value=sources[0].id;$("no-sources").hidden=Boolean(sources.length);
    }
    await history();
  }
  init().catch(error=>notice(error.message));
})();
