"use strict";
window.seoStrategy = {
  async init({api, el, reportID, rows, notice}) {
    const $ = id => document.getElementById(id);
		const semanticOption=el("label",null,"checkbox-label"),semanticInput=el("input");semanticInput.type="checkbox";semanticInput.id="geo-profile-semantic";semanticOption.append(semanticInput,el("span","Багатомовні поняття та синоніми · EN / UA / RU / PL / ES / RO"));$("geo-profile-form").insertBefore(semanticOption,$("geo-profile-form").querySelector(".input-footer"));
    const safe = fn => async (...args) => { try { await fn(...args); } catch (error) { notice(error.message); } };
    const date = value => value ? new Date(value).toLocaleString("uk-UA") : "—";
    const states = {covered:"Покрито",partial:"Частково",no_target:"Без цілі",pending:"У черзі",scanning:"Сканування / аналіз",ready:"Готово",failed:"Помилка",interrupted:"SEO перервано",canceled:"Скасовано"};
    const rules = {allowed:"Дозволено правилами",blocked:"Заборонено",limited:"Обмежено",unknown:"Не перевірено",found:"Виявлено",missing:"Не виявлено",mismatch:"Не узгоджено",not_applicable:"Не застосовується",yes:"Так",no:"Не виявлено"};
    const engines = {google_ai:"Google AI",chatgpt:"ChatGPT",gemini:"Gemini",perplexity:"Perplexity",other:"Інша система"};
    const changes = {target_url:"Цільовий URL",relevance:"Релевантність",google_ai_eligibility:"Правила Google AI",chatgpt_search_eligibility:"Правила ChatGPT Search",oai_searchbot_rules:"Правила OAI-SearchBot",brand_entity:"Бренд у контенті","entity:organization":"Назва організації","entity:same_as":"Посилання sameAs організації","entity:publisher":"Видавець","entity:author":"Сутність автора","entity:product_brand":"Бренд продукту",answer_blocks:"Блоки відповіді зі збігом запиту"};
    const localLink = (path, text) => { const a = el("a", text); a.href = path; return a; };
    const empty = (id, text, span) => { const td = el("td", text, "muted"); td.colSpan = span; const tr = el("tr"); tr.append(td); $(id).querySelector("tbody").replaceChildren(tr); };
    const list = (node, title, values) => { node.append(el("h3", title)); const ul = el("ul"); for (const value of values) ul.append(el("li", value)); node.append(values.length ? ul : el("p", "Немає даних", "muted")); };
    const pager = (label, prev, next, page, total) => { $(label).textContent = total ? `${page * 50 + 1}–${Math.min((page + 1) * 50, total)} із ${total}` : "Немає даних"; $(prev).disabled = page === 0; $(next).disabled = (page + 1) * 50 >= total; };
    let view = "queries", topics = null, topicPage = 0, activeTopic = null, topicCount = 0;
    let setData = null, competitorPage = 0, competitorGeneration = 0;
    let profiles = [], timelinePage = 0, profileGeneration = 0, changeGeneration = 0;
    let competitorRequest, competitorPayload, profileRequest, profilePayload, observedPage = 0, observedGeneration = 0;

    function topicTable() {
      const search = $("topic-search").value.toLocaleLowerCase(), filter = $("topic-filter").value;
      const items = (topics || []).filter(t => (t.label + " " + t.main_url + " " + t.terms.join(" ")).toLocaleLowerCase().includes(search) && (filter === "all" || filter === "gaps" && t.no_target > 0 || filter === "overlap" && t.overlapping_candidates.length > 0));
      topicPage = Math.max(0, Math.min(topicPage, Math.ceil(items.length / 50) - 1));
      const body = $("topic-table").querySelector("tbody"); body.replaceChildren();
      for (const t of items.slice(topicPage * 50, (topicPage + 1) * 50)) {
        const tr = el("tr"); tr.tabIndex = 0; tr.setAttribute("aria-label", "Кластер: " + t.label);
        for (const value of [t.label,t.queries.length,t.covered,t.partial,t.no_target,t.main_url || "—",t.overlapping_candidates.length,t.gaps[0] || "—"]) tr.append(el("td", value));
        tr.addEventListener("click", () => openTopic(t)); tr.addEventListener("keydown", e => { if (e.key === "Enter") openTopic(t); }); body.append(tr);
      }
      if (!items.length) empty("topic-table", "Кластерів за фільтром немає", 8);
      pager("topic-page-label", "topic-prev", "topic-next", topicPage, items.length);
    }
    function moreTopic() {
      const body = $("geo-topic-content").querySelector("tbody");
      for (const q of activeTopic.queries.slice(topicCount, topicCount + 50)) { const tr = el("tr"); for (const value of [q.query,states[q.state],q.target_url || "—",q.relevance ?? "Не зіставлено"]) tr.append(el("td", value)); body.append(tr); }
      topicCount += 50; $("geo-topic-more").hidden = topicCount >= activeTopic.queries.length;
    }
    function openTopic(t) {
      activeTopic = t; topicCount = 0; $("geo-topic-heading").textContent = t.label; const node = $("geo-topic-content"); node.replaceChildren();
      list(node,"Цільові URL",t.urls.map(u => `${u.url} · ${u.queries} запитів`)); list(node,"Близькі кандидати — не доказ канібалізації",t.overlapping_candidates.map(u=>`${u.url} · ${u.queries} запитів`)); list(node,"Прогалини",t.gaps);
      const wrap = el("div", null, "table-wrap"), table = el("table"), head = el("thead"), tr = el("tr");
      for (const v of ["Запит","Покриття","Ціль","Релевантність"]) tr.append(el("th",v)); head.append(tr); table.append(head,el("tbody")); wrap.append(table); node.append(wrap); moreTopic(); $("geo-topic-details").showModal();
    }
    async function selectView(value) {
      view = value;
      for (const v of ["queries","topics","competitors"]) { $(v === "queries" ? "geo-query-view" : v === "topics" ? "geo-topic-view" : "geo-competitor-view").hidden = v !== value; $("geo-tab-"+v).setAttribute("aria-selected", String(v === value)); }
      if (value === "topics" && topics === null) { const data = await api(`/api/geo/reports/${reportID}/topics`); topics = data.topics; $("topic-model").textContent = data.model; topicTable(); }
      if (value === "competitors") await competitorHistory();
    }
    for (const button of document.querySelectorAll("[data-geo-view]")) {
      button.addEventListener("click", safe(() => selectView(button.dataset.geoView)));
      button.addEventListener("keydown", e => { if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return; e.preventDefault(); const tabs = [...document.querySelectorAll("[data-geo-view]")]; tabs[(tabs.indexOf(button) + (e.key === "ArrowRight" ? 1 : 2)) % 3].focus(); });
    }
    $("topic-search").addEventListener("input", () => { topicPage = 0; topicTable(); }); $("topic-filter").addEventListener("change", () => { topicPage = 0; topicTable(); });
    $("topic-prev").addEventListener("click", () => { topicPage--; topicTable(); }); $("topic-next").addEventListener("click", () => { topicPage++; topicTable(); }); $("geo-topic-more").addEventListener("click", moreTopic);

    function resultCell(row) {
      const td = el("td"); if (!row) { td.textContent = "Не перевірено"; return td; }
      const r = row.result; td.append(el("div",r.target_url || "Ціль не знайдено"),el("small",r.matching ? `Релевантність: ${r.matching.relevance} / 100` : "Немає актуального зіставлення"));
      for (const c of r.categories || []) td.append(el("div",`${c.name}: ${c.passed} / ${c.total}`,"muted")); return td;
    }
    function compareDetails(own, other, competitor) {
      $("geo-comparison-heading").textContent = own.result.query; const node = $("geo-comparison-content"); node.replaceChildren();
      for (const [label,row] of [["Наш сайт",own],[competitor.brand || competitor.domain,other]]) {
        node.append(el("h3",label)); if (!row) { node.append(el("p","Ціль не знайдено / аналіз не завершений")); continue; }
        node.append(el("p",row.result.target_url || "Ціль не знайдено"));
        list(node,"Перевірені сигнали",(row.result.checks || []).map(c=>`${c.name}: ${c.passed ? "Виявлено" : "Не виявлено"} · ${c.evidence || ""}`));
        list(node,"Сутності бренду",(row.result.entities?.checks || []).map(c=>`${c.name}: ${rules[c.status] || c.status}`));
        if (row.result.eligibility) window.seoDiagnostics?.eligibility(node,row.result.eligibility);
        list(node,"Ручні спостереження",(row.citations || []).map(c=>`${engines[c.engine] || c.engine}: ${rules[c.citation] || "Не перевірено"} · ${date(c.checked_at)}`));
      }
      list(node,"У конкурента виявлено, у нашої цілі не виявлено",(competitor.gaps?.[own.ordinal] || []).map(g=>`${g.signal}: ${g.evidence}`));
      node.append(localLink("/geo/"+competitor.report_id,"Повний GEO-звіт конкурента")); $("geo-competitor-details").showModal();
    }
    async function competitorHistory(selected) {
      if (!reportID) return;
      const items = await api(`/api/geo/reports/${reportID}/competitors`); const chosen = selected || $("competitor-set-select").value;
      $("competitor-set-select").replaceChildren(); for (const s of items) { const option = el("option",`${date(s.at)} · ${s.ready} / ${s.total} готово${s.canceled ? " · скасовано" : ""}`); option.value = s.id; $("competitor-set-select").append(option); }
      if (items.some(s=>s.id===chosen)) $("competitor-set-select").value = chosen;
      await loadCompetitors();
    }
    async function loadCompetitors() {
      const id = $("competitor-set-select").value, generation = ++competitorGeneration;
      if (!id) { setData = null; $("competitor-status").textContent = "Порівнянь ще немає"; empty("competitor-table","Додайте конкурентів",1); $("competitor-cancel").disabled = true; pager("competitor-page-label","competitor-prev","competitor-next",0,0); return; }
      const data = await api(`/api/geo/competitors/${id}?after=${competitorPage*50}`); if (generation !== competitorGeneration || id !== $("competitor-set-select").value) return; setData = data;
      const status = $("competitor-status"); status.replaceChildren();
      data.competitors.forEach((c,index) => { const line = el("p",`${c.brand || c.domain}: ${states[c.status]} · ${c.reused ? "повторно використано свіжий аудит" : "окремий обмежений аудит"}${c.error ? " · "+c.error : ""}`); if(c.run_id) line.append(" · ",localLink("/audits/"+c.run_id,"SEO-запуск")); if(c.status === "failed" && !data.canceled) { const retry = el("button","Повторити"); retry.addEventListener("click",safe(async()=>{ await api(`/api/geo/competitors/${id}/retry`,{ordinal:index+1}); await loadCompetitors(); })); line.append(" ",retry); } status.append(line);
        const d = c.discovery; if(d) { const summary = el("p",`Sitemap: ${d.candidate_urls} URL-кандидатів · відібрано ${d.selected_urls} сторінок разом зі стартовою · ${d.matching_queries} / ${d.query_count} запитів мають URL-кандидатів · ${d.sitemap_files} файлів${d.state === "partial" ? " · discovery неповне" : ""}${d.selection_limited ? " · застосовано ліміт сканування" : ""}${d.warning ? " · "+d.warning : ""}`); summary.className = "muted"; status.append(summary); }
      });
      $("competitor-cancel").disabled = data.canceled || !data.competitors.some(c=>c.status === "pending" || c.status === "scanning");
      const head = $("competitor-table").querySelector("thead"), tr = el("tr"); head.replaceChildren(); for(const label of ["Запит","Наш сайт",...data.competitors.map(c=>c.brand || c.domain)]) tr.append(el("th",label)); head.append(tr);
      const body = $("competitor-table").querySelector("tbody"); body.replaceChildren();
      for(const own of data.own) { const tr = el("tr"); tr.append(el("td",own.result.query),resultCell(own)); for(const c of data.competitors) { const other = c.rows.find(r=>r.ordinal === own.ordinal), td = resultCell(other); if(other) { const button = el("button",`Порівняти · ${(c.gaps?.[own.ordinal] || []).length} відмінностей`); button.addEventListener("click",()=>compareDetails(own,other,c)); td.append(button); } tr.append(td); } body.append(tr); }
      pager("competitor-page-label","competitor-prev","competitor-next",competitorPage,rows.length);
    }
    $("competitor-create-open").addEventListener("click",()=>$("geo-competitor-create").showModal());
    $("competitor-set-select").addEventListener("change",safe(async()=>{ competitorPage = 0; await loadCompetitors(); })); $("competitor-refresh").addEventListener("click",safe(()=>competitorHistory()));
    $("competitor-prev").addEventListener("click",safe(async()=>{ competitorPage--; await loadCompetitors(); })); $("competitor-next").addEventListener("click",safe(async()=>{ competitorPage++; await loadCompetitors(); }));
    $("competitor-cancel").addEventListener("click",safe(async()=>{ await api(`/api/geo/competitors/${setData.id}/cancel`,{}); await competitorHistory(); }));
    $("competitor-form").addEventListener("submit",async event => {
      event.preventDefault(); const competitors = $("competitor-input").value.split(/\r?\n/).filter(s=>s.trim()).map(s=>{ const [url,...brand] = s.split("|"); return {url:url.trim(),brand:brand.join("|").trim()}; });
      const payload = {competitors,max_pages:Number($("competitor-max-pages").value),fresh_hours:Number($("competitor-fresh-hours").value)}, signature = JSON.stringify(payload); if(signature !== competitorPayload) { competitorPayload = signature; competitorRequest = crypto.randomUUID(); }
      $("competitor-submit").disabled = true; $("competitor-form-status").textContent = "Збереження…";
      try { const result = await api(`/api/geo/reports/${reportID}/competitors`,{...payload,id:competitorRequest}); $("geo-competitor-create").close(); competitorPage = 0; await competitorHistory(result.id); }
      catch(error) { $("competitor-form-status").textContent = error.message; } finally { $("competitor-submit").disabled = false; }
    });

    async function profileList(selected) {
      profiles = await api("/api/geo/profiles"); const chosen = selected || $("geo-profile-select").value; $("geo-profile-select").replaceChildren();
      for(const p of profiles) { const o = el("option",`${p.name} · ${p.domain}${p.enabled ? "" : " · призупинено"}`); o.value = p.id; $("geo-profile-select").append(o); } if(profiles.some(p=>p.id===chosen)) $("geo-profile-select").value = chosen; await loadProfile();
    }
    async function loadProfile() {
      const id = $("geo-profile-select").value, generation = ++profileGeneration, profile = profiles.find(p=>p.id===id); $("geo-profile-toggle").disabled = !profile;
      if(!profile) { $("geo-profile-meta").textContent = "Профілів ще немає"; empty("geo-profile-history","Створіть профіль для SEO-розкладу",5); empty("geo-timeline","Немає даних",1); empty("geo-change-table","Немає даних",4); $("geo-change-run").replaceChildren(); pager("geo-timeline-label","geo-timeline-prev","geo-timeline-next",0,0); return; }
      $("geo-profile-toggle").textContent = profile.enabled ? "Призупинити" : "Увімкнути"; $("geo-profile-meta").textContent = `${profile.schedule_name} · ${profile.query_count} запитів · ${profile.brand || "Бренд не заданий"}`;
      const history = await api(`/api/geo/profiles/${id}/history`); if(generation !== profileGeneration) return; const body = $("geo-profile-history").querySelector("tbody"); body.replaceChildren(); const prior = $("geo-change-run").value; $("geo-change-run").replaceChildren();
      for(const h of history) { const tr = el("tr"), report = el("td"), action = el("td",h.error || "—"); if(h.report_id) report.append(localLink("/geo/"+h.report_id,"GEO-звіт")); else report.textContent = "—";
        if(h.status === "failed") { const b = el("button","Повторити аналіз"); b.addEventListener("click",safe(async()=>{ await api(`/api/geo/profiles/${id}/retry`,{run_id:h.run_id}); await loadProfile(); })); action.append(b); }
        tr.append(el("td",date(h.scheduled_at)),el("td",states[h.status]),report,el("td",h.changes),action); body.append(tr);
        if(h.status === "ready") { const option = el("option",date(h.scheduled_at)); option.value = h.run_id; $("geo-change-run").append(option); }
      }
      if(!history.length) empty("geo-profile-history","Очікуємо завершений SEO-запуск розкладу",5); if(history.some(h=>h.run_id===prior && h.status==="ready")) $("geo-change-run").value = prior;
      await loadTimeline(); await loadChanges(); await loadObserved();
    }
    async function loadTimeline() {
      const id = $("geo-profile-select").value, generation = profileGeneration, requestedPage = timelinePage; if(!id) return;
      const data = await api(`/api/geo/profiles/${id}/timeline?after=${requestedPage*50}`); if(generation !== profileGeneration || requestedPage !== timelinePage) return;
      const points = data.points, reports = [...new Map(points.map(p=>[p.report_id,p])).values()].sort((a,b)=>new Date(a.at)-new Date(b.at));
      const head = $("geo-timeline").querySelector("thead"), tr = el("tr"); head.replaceChildren(); tr.append(el("th","Запит")); for(const p of reports) { const th = el("th"); th.append(localLink("/geo/"+p.report_id,date(p.at)),el("small",p.model || "")); tr.append(th); } head.append(tr);
      const body = $("geo-timeline").querySelector("tbody"); body.replaceChildren(); for(const ordinal of [...new Set(points.map(p=>p.ordinal))]) { const qs = points.filter(p=>p.ordinal===ordinal), tr = el("tr"); tr.append(el("td",qs[0].query)); for(const r of reports) { const p = qs.find(p=>p.report_id===r.report_id), td = el("td",p?.relevance == null ? "Не зіставлено" : p.relevance+" / 100"); if(p) { td.append(el("small",p.url || "Без цілі")); td.title = `Google AI: ${rules[p.google_ai] || "Не перевірено"}; ChatGPT Search: ${rules[p.chatgpt_search] || "Не перевірено"}`; } tr.append(td); } body.append(tr); }
      if(!points.length) empty("geo-timeline","Завершених GEO-запусків ще немає",1); pager("geo-timeline-label","geo-timeline-prev","geo-timeline-next",timelinePage,points.length ? profiles.find(p=>p.id===id).query_count : 0);
    }
    async function loadChanges() {
      const id = $("geo-profile-select").value, run = $("geo-change-run").value, generation = ++changeGeneration; if(!run) { empty("geo-change-table","Немає завершених порівнянь",4); return; }
      const data = await api(`/api/geo/profiles/${id}/changes?run_id=${run}&after=${timelinePage*50}`); if(generation !== changeGeneration || id !== $("geo-profile-select").value) return;
      const body = $("geo-change-table").querySelector("tbody"); body.replaceChildren(); for(const c of data.items) { const tr = el("tr"), name = c.kind.startsWith("observed_citation:") ? "Ручне AI-цитування · "+engines[c.kind.split(":")[1]] : changes[c.kind] || c.kind; for(const v of [c.query,name,rules[c.before] || c.before || "—",rules[c.after] || c.after || "—"]) tr.append(el("td",v)); body.append(tr); }
      if(!data.items.length) empty("geo-change-table",data.baseline_report_id ? "Перевірених змін для цих 50 запитів немає" : "Перший звіт — базовий, без попереднього порівняння",4);
    }
    async function loadObserved() {
      const id = $("geo-profile-select").value, run = $("geo-change-run").value, generation = ++observedGeneration;
      if(!id || !run) { empty("geo-observed-changes","Немає порівнюваних імпортів",4); pager("geo-observed-page","geo-observed-prev","geo-observed-next",0,0); return; }
      const data = await api(`/api/geo/profiles/${id}/observations?run_id=${run}&after=${observedPage*50}`); if(generation !== observedGeneration || id !== $("geo-profile-select").value || run !== $("geo-change-run").value) return;
      const body = $("geo-observed-changes").querySelector("tbody"); body.replaceChildren();
      for(const item of data.items) { const tr = el("tr"); for(const value of [item.query || "Лише URL — без запиту",item.url || "Лише запит",item.before,item.after]) tr.append(el("td",value)); body.append(tr); }
      if(!data.items.length) empty("geo-observed-changes","Порівнюваних змін 0 ↔ позитивні покази не виявлено",4);
      $("geo-observed-meta").textContent = data.current ? `${engines[data.current.engine]} · ${data.current.country || "Усі країни"} · ${data.current.device} · ${data.previous.period_start}–${data.previous.period_end} → ${data.current.period_start}–${data.current.period_end}. Лише спостереження у вибраних імпортах, не доказ постійної втрати видимості.` : "Немає пари AI-імпортів однакового джерела, системи, країни й пристрою з неперекривними періодами. GSC Web і відсутні рядки не є нульовою AI-видимістю.";
      pager("geo-observed-page","geo-observed-prev","geo-observed-next",observedPage,data.total || 0);
    }
    $("geo-observed-prev").addEventListener("click",safe(async()=>{ observedPage--; await loadObserved(); })); $("geo-observed-next").addEventListener("click",safe(async()=>{ observedPage++; await loadObserved(); }));
    $("geo-profile-select").addEventListener("change",safe(async()=>{ timelinePage = 0; observedPage = 0; await loadProfile(); })); $("geo-profile-refresh").addEventListener("click",safe(()=>profileList()));
    $("geo-profile-toggle").addEventListener("click",safe(async()=>{ const p = profiles.find(p=>p.id === $("geo-profile-select").value); await api(`/api/geo/profiles/${p.id}/update`,{enabled:!p.enabled}); await profileList(p.id); }));
    $("geo-change-run").addEventListener("change",safe(async()=>{ observedPage = 0; await loadChanges(); await loadObserved(); })); $("geo-timeline-prev").addEventListener("click",safe(async()=>{ timelinePage--; await loadTimeline(); await loadChanges(); })); $("geo-timeline-next").addEventListener("click",safe(async()=>{ timelinePage++; await loadTimeline(); await loadChanges(); }));
    $("geo-profile-open").addEventListener("click",safe(async()=>{
      const schedules = await api("/api/schedules"); $("geo-profile-schedule").replaceChildren(); for(const s of schedules) { const option = el("option",s.name+(s.enabled ? "" : " · розклад призупинено")); option.value = s.id; $("geo-profile-schedule").append(option); }
      if(!schedules.length) throw new Error("Спочатку створіть SEO-розклад у Моніторингу");
      if(reportID) { const data = await api(`/api/geo/reports/${reportID}`); $("geo-profile-domain").value = data.report.domain; $("geo-profile-brand").value = data.report.brand; $("geo-profile-queries").value = rows.map(r=>r.result.query).join("\n");semanticInput.checked=data.report.semantic; } else { $("geo-profile-domain").value = $("domain").value; $("geo-profile-brand").value = $("brand").value; $("geo-profile-queries").value = $("queries").value;semanticInput.checked=$("semantic-matching").checked; }
      $("geo-profile-form-status").textContent = ""; $("geo-profile-create").showModal();
    }));
    $("geo-profile-form").addEventListener("submit",async event => {
      event.preventDefault(); const payload = {schedule_id:$("geo-profile-schedule").value,name:$("geo-profile-name").value,domain:$("geo-profile-domain").value,brand:$("geo-profile-brand").value,queries:$("geo-profile-queries").value,semantic:semanticInput.checked}, signature = JSON.stringify(payload); if(signature !== profilePayload) { profilePayload = signature; profileRequest = crypto.randomUUID(); }
      $("geo-profile-submit").disabled = true; $("geo-profile-form-status").textContent = "Збереження…";
      try { const p = await api("/api/geo/profiles",{...payload,id:profileRequest}); $("geo-profile-create").close(); timelinePage = 0; await profileList(p.id); } catch(error) { $("geo-profile-form-status").textContent = error.message; } finally { $("geo-profile-submit").disabled = false; }
    });
    let refreshing = false;
    const timer = setInterval(async()=>{ if(document.hidden || refreshing) return; refreshing = true; try { await loadProfile(); if(view === "competitors") await competitorHistory(); } catch(error) { notice(error.message); } finally { refreshing = false; } },15000);
    window.addEventListener("pagehide",()=>clearInterval(timer),{once:true});
    window.addEventListener("geo:targets-changed",safe(async()=>{topics=null;if(view==="topics")await selectView("topics");}));
    await profileList();
  }
};
