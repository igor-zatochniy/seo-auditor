"use strict";
(() => {
  const id = location.pathname.match(/^\/audits\/([a-f0-9-]{36})$/i)?.[1];
  if (!id) return;
  const $ = name => document.getElementById("international-" + name);
  const el = (tag, text, cls) => { const n = document.createElement(tag); n.textContent = text; if (cls) n.className = cls; return n; };
  const labels = {ok:"Без виявлених проблем",review:"Потребує перевірки",error:"Помилки",not_checked:"Не перевірено"};
  let cursors = [0], next = 0, version = 0;
  async function load() {
    const request = ++version;
    $("status").textContent = "Завантаження…";
    try {
      const response = await fetch(`/api/audits/${id}/international?after=${cursors.at(-1)}`, {headers:{"X-SEO-Auditor-Request":"1"},credentials:"same-origin"});
      const data = await response.json();
      if (!response.ok) throw Error(data.error?.message || "Не вдалося завантажити hreflang");
      if (request !== version) return;
      $("rows").replaceChildren(); next = data.next;
      for (const p of data.pages) {
        const row = document.createElement("tr"), source = document.createElement("td"), links = document.createElement("td"), issues = document.createElement("td");
        const page = document.createElement("td"); page.append(el("div", p.url),el("small",`HTTP ${p.http ?? "—"} · Target ${p.target_id}`),el("div",labels[p.status],"badge "+(p.status==="error"?"error":p.status==="ok"?"success":"warning")));
        for (const [name,e] of [["HTML",p.html],["Sitemap",p.sitemap]]) {
          if (!e) continue;
          source.append(el("div",name+(e.complete?"":" · неповні дані")));
          for (const a of e.links ?? []) links.append(el("div",`${name} · ${a.language}: ${a.url} · HTTP ${p.observations[a.fingerprint]?.http ?? "не перевірено"}`));
        }
        if (!links.childNodes.length) links.append(el("span",p.html||p.sitemap?"Hreflang не оголошено":"Потрібен новий аудит після оновлення"));
        for (const i of p.issues) issues.append(el("p",`${i.source}${i.language?" · "+i.language:""}: ${i.message}`,i.severity==="error"?"error-text":"metric-note"));
        row.append(page,source,links,issues); $("rows").append(row);
      }
      $("previous").disabled = cursors.length===1; $("next").disabled = !next;
      $("status").textContent = `${data.pages.length} сторінок · Response HTML + sitemap. HTTP і взаємність перевірено лише для URL у цьому запуску. x-default — рекомендація, не обов'язкова вимога. Rendered DOM переглядайте окремо у деталях JavaScript.`;
    } catch (e) { if (request===version) { $("rows").replaceChildren(); $("status").textContent=e.message; $("next").disabled=true; } }
  }
  document.addEventListener("seo:international-open",load);
  $("refresh").onclick=load;
  $("next").onclick=()=>{if(next){cursors.push(next);load();}};
  $("previous").onclick=()=>{if(cursors.length>1){cursors.pop();load();}};
})();
