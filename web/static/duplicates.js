"use strict";
(() => {
  const id = location.pathname.match(/^\/audits\/([a-f0-9-]{36})$/i)?.[1];
  if (!id) return;
  const $ = name => document.getElementById("duplicates-" + name);
  const el = (tag, text, cls) => { const n = document.createElement(tag); n.textContent = text; if (cls) n.className = cls; return n; };
  const contexts = {technical_candidate:"Можливий технічний дубль",regional_review:"Мовний / регіональний варіант",product_review:"Товарний варіант"};
  const canonical = {consistent:"Єдиний canonical",divergent:"Різні canonical — перевірити",missing:"Canonical відсутній у частини або всіх сторінок",unverified:"Canonical не перевірено повністю"};
  let cursors = [0], next = 0, version = 0, selected = 0, memberCursors = [0], memberNext = 0, memberVersion = 0;
  async function get(query) {
    const response = await fetch(`/api/audits/${id}/duplicates?${query}`, {headers:{"X-SEO-Auditor-Request":"1"},credentials:"same-origin"});
    const data = await response.json();
    if (!response.ok) throw Error(data.error?.message || "Не вдалося завантажити дублі");
    return data;
  }
  async function members() {
    const request = ++memberVersion;
    $("member-status").textContent = "Завантаження…";
    $("member-next").disabled = true;
    try {
      const data = await get(`cluster=${selected}&member_after=${memberCursors.at(-1)}`);
      if (request !== memberVersion) return;
      const cluster = data.clusters[0]; memberNext = cluster.member_next;
      $("member-rows").replaceChildren();
      for (const p of cluster.members) {
        const row = document.createElement("tr"), page = document.createElement("td"), title = document.createElement("td"), target = document.createElement("td");
        page.append(el("div",p.url),el("small",`Ціль ${p.target_id} · ${p.language || "мова не оголошена"} · ${p.words} слів`));
        title.append(el("div",p.title || "Title відсутній"),el("small",p.h1)); target.append(el("div",p.canonical || "Не оголошено"));
        row.append(page,title,target); $("member-rows").append(row);
      }
      $("member-status").textContent = `Група ${cluster.id} · ${cluster.count} сторінок · показано ${cluster.members.length}`;
      $("member-previous").disabled = memberCursors.length === 1; $("member-next").disabled = !memberNext;
    } catch(e) { if(request===memberVersion) { $("member-rows").replaceChildren(); $("member-status").textContent=e.message; } }
  }
  async function load() {
    const request = ++version;
    $("status").textContent = "Аналіз основного контенту…";
    $("next").disabled = true;
    try {
      const data = await get(`after=${cursors.at(-1)}`);
      if (request !== version) return;
      const s = data.summary; next = data.next;
      $("summary").replaceChildren();
      for (const [key,label] of [["similar_pages","Схожі сторінки"],["exact_clusters","Точні групи"],["near_clusters","Групи схожості"],["canonical_reviews","Розбіжності canonical"],["potential_overlaps","Можливі перекриття тем"]]) {
        const n = el("span",`${label}: `); n.append(el("strong",String(s[key]))); $("summary").append(n);
      }
      $("rows").replaceChildren();
      for (const c of data.clusters) {
        const row = document.createElement("tr"), group = document.createElement("td"), urls = document.createElement("td"), context = document.createElement("td"), signals = document.createElement("td");
        group.append(el("div",`${c.kind==="exact"?"Точний текстовий збіг":"Схожий основний текст"} · ${c.count}`),el("small",`Група ${c.id}`));
        const button = el("button","Переглянути сторінки"); button.onclick = () => { selected=c.id; memberCursors=[0]; $("dialog").showModal(); members(); }; group.append(button);
        for (const m of c.members) urls.append(el("div",m.url));
        if(c.count>c.members.length) urls.append(el("small",`Ще ${c.count-c.members.length} сторінок`));
        context.append(el("span",contexts[c.context],"badge "+(c.context==="technical_candidate"?"warning":"")));
        signals.append(el("div",canonical[c.canonical],c.canonical==="divergent"?"error-text":""));
        if(c.potential_overlap) signals.append(el("small","Повторюється Title або H1: можливе перекриття тем, не доказ канібалізації"));
        row.append(group,urls,context,signals); $("rows").append(row);
      }
      if (!data.clusters.length) { const row=document.createElement("tr"),cell=el("td","Груп у цій частині звіту не знайдено"); cell.colSpan=4; row.append(cell); $("rows").append(row); }
      const partial = !s.near_search_complete || !s.signatures_complete;
      $("status").textContent = `${partial?"Неповна перевірка. ":""}HTML відповіді: ${s.pages} успішних HTTP 200; проаналізовано ${s.analyzed} із ${s.eligible} придатних. Без повного підпису: ${s.unverified}; короткі тексти (<80 слів): ${s.short_content}; поза лімітом: ${s.capacity_excluded}. ${!s.near_search_complete?"Ліміт пошуку схожості спрацював; підсумки є нижньою межею.":""}`;
      $("previous").disabled=cursors.length===1; $("next").disabled=!next;
    } catch(e) { if(request===version) { $("rows").replaceChildren(); $("summary").replaceChildren(); $("status").textContent=e.message; } }
  }
  document.addEventListener("seo:duplicates-open",load);
  $("refresh").onclick=()=>{cursors=[0];load();};
  $("next").onclick=()=>{if(next){cursors.push(next);load();}};
  $("previous").onclick=()=>{if(cursors.length>1){cursors.pop();load();}};
  $("member-next").onclick=()=>{if(memberNext){memberCursors.push(memberNext);members();}};
  $("member-previous").onclick=()=>{if(memberCursors.length>1){memberCursors.pop();members();}};
})();
