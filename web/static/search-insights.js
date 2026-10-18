"use strict";
(() => {
  const id = location.pathname.match(/^\/audits\/([a-f0-9-]{36})$/i)?.[1];
  if (!id) return;
  const $ = id => document.getElementById(id);
  const el = (tag, text) => { const n = document.createElement(tag); if (text != null) n.textContent = String(text); return n; };
  for (const [view, label] of [["landing", "Посадкові сторінки"], ["structure", "Структура сайту"]]) {
    const b = el("button", label); b.id = "tab-" + view; b.setAttribute("role", "tab"); b.setAttribute("aria-selected", "false"); b.setAttribute("aria-controls", view); document.querySelector(".view-tabs").append(b);
  }
  const base = `/api/audits/${id}`;
  const api = async (path, body, method) => {
    const response = await fetch(path, { method: method || (body === undefined ? "GET" : "POST"), credentials: "same-origin", headers: { "X-SEO-Auditor-Request": "1", ...(body === undefined ? {} : { "Content-Type": "application/json" }) }, body: body === undefined ? undefined : JSON.stringify(body) });
    const data = await response.json(); if (!response.ok) throw Error(data.error?.message || "Запит не виконано"); return data;
  };
  const value = v => v == null ? "Немає даних" : typeof v === "number" ? v.toLocaleString("uk-UA") : String(v);
  const share = v => v == null ? "Невідомо" : (v * 100).toFixed(1) + "%";
  const safe = (view, fn) => async (...args) => { try { await fn(...args); } catch (e) { $(view + "-status").textContent = e.message; } };
  let imports = null, origin = "", landingOffset = 0, structureOffset = 0, prefix = "/", historyOffset = 0, historyItem = null;
  let landingSerial = 0, structureSerial = 0, historySerial = 0, importSerial = 0;
  function geoOptions() {
    const select = $("landing-geo"), previous = select.value;
    select.replaceChildren(el("option", "Останній GEO-звіт")); select.firstChild.value = "";
    for (const v of imports?.geo_reports || []) if (origin && v.domain === new URL(origin).hostname) { const o = el("option", `${new Date(v.created_at).toLocaleString("uk-UA")} · ${v.queries} запитів · ${v.id.slice(0, 8)}`); o.value = v.id; select.append(o); }
    if (Array.from(select.options).some(v => v.value === previous)) select.value = previous;
  }
  async function loadImports() {
    const serial = ++importSerial;
    const data = await api(base + "/search-imports"); if (serial !== importSerial) return;
    imports = data;
    for (const control of [$("search-origin"), $("structure-origin")]) {
      const previous = control.value;
      control.replaceChildren(...data.origins.map(v => { const o = el("option", v); o.value = v; return o; }));
      if (data.origins.includes(previous)) control.value = previous;
    }
    origin = $("search-origin").value;
    geoOptions();
    $("search-import-list").replaceChildren(...data.items.map(v => {
      const row = el("div", `${v.origin} · ${v.start} — ${v.end} · ${v.rows} рядків `), button = el("button", "Видалити імпорт");
      button.type = "button"; button.onclick = safe("search-import", async () => { if (!window.confirm("Видалити цей імпорт GSC? Початковий SEO-аудит не зміниться.")) return; button.disabled = true; try { await api(base + "/search-imports/" + v.id, {}, "DELETE"); await loadImports(); landingOffset = 0; await loadLanding(); } finally { button.disabled = false; } }); row.append(button); return row;
    }));
    if (!data.items.length) $("search-import-list").append(el("p", "Імпортів ще немає"));
  }
  async function loadLanding() {
    const serial = ++landingSerial;
    if (!imports) await loadImports();
    if (!origin) throw Error("Аудит ще не має результатів HTTP/HTTPS");
    const params = new URLSearchParams({ origin, offset: landingOffset, country: $("landing-country").value.trim(), device: $("landing-device").value, geo_report_id: $("landing-geo").value });
    const data = await api(base + "/landing-pages?" + params); if (serial !== landingSerial) return;
    $("landing-notes").textContent = data.note;
    $("landing-status").textContent = "Дані імпортованих CSV; сигнали потребують перевірки, не доводять канібалізацію." + (data.geo_report_id ? ` GEO-звіт: ${data.geo_report_id.slice(0, 8)}.` : " GEO-цілі для цієї адреси сайту відсутні.") + (data.items.length && !data.identity_compatible ? " HMAC-збіг із URL аудиту не підтверджено; порівняння цілей невідоме." : "") + (imports.geo_reports_limited ? " У списку останні 200 GEO-звітів." : "");
    const rows = $("landing-rows"); rows.replaceChildren();
    for (const v of data.items) {
      const row = el("tr"), query = el("td", v.query); query.append(el("small", `${v.country} · ${v.device}`));
      const notes = []; if (v.changed === true) notes.push("Змінилася посадкова сторінка"); if (v.target_mismatch === true) notes.push("Лідер відрізняється від обраної цілі"); if (v.tied) notes.push("Кілька URL з однаковими показами");
      const previous = el("td", value(v.previous_url)); previous.append(el("small", value(v.previous_day)));
      const current = el("td", value(v.current_url)); current.append(el("small", `${v.day} · ${share(v.leader_share)} · сума показів URL: ${value(v.impressions)}`));
      const signal = el("td", notes.join(". ") || (v.changed === false ? "Зміни лідера не виявлено" : "Недостатньо даних для порівняння")); if (notes.length) signal.className = "diagnostic-warning";
      const action = el("td"), button = el("button", "Історія"); button.onclick = safe("landing-history", async () => { historyItem = { ...v, origin }; historyOffset = 0; $("landing-history-title").textContent = v.query + " · " + v.country + " · " + v.device; $("landing-history-rows").replaceChildren(); $("landing-history-dialog").showModal(); await loadHistory(); }); action.append(button);
      row.append(query, el("td", value(v.expected_url)), previous, current, signal, action); rows.append(row);
    }
    if (!data.items.length) { const row = el("tr"), cell = el("td", "Спостережень для обраного сегмента немає"); cell.colSpan = 6; row.append(cell); rows.append(row); }
    $("landing-prev").disabled = landingOffset === 0; $("landing-next").disabled = !data.more;
  }
  async function loadHistory() {
    const serial = ++historySerial;
    const params = new URLSearchParams({ origin: historyItem.origin, query: historyItem.query_key, country: historyItem.country, device: historyItem.device, offset: historyOffset });
    const data = await api(base + "/landing-history?" + params); if (serial !== historySerial) return;
    $("landing-history-status").textContent = "Імпортовані спостереження. Відсутні URL або дати не означають нуль показів. Ідентичності розрізняють URL із замаскованими query-параметрами.";
    $("landing-history-rows").replaceChildren(...data.items.map(v => { const row = el("tr"), url = el("td", v.url); url.append(el("small", v.url_id.slice(0, 12))); row.append(el("td", v.day), url, el("td", `${value(v.impressions)} / ${share(v.share)}`), el("td", value(v.clicks)), el("td", value(v.position))); return row; }));
    $("landing-history-prev").disabled = historyOffset === 0; $("landing-history-next").disabled = !data.more;
  }
  async function loadStructure() {
    const serial = ++structureSerial;
    if (!imports) await loadImports();
    const selected = $("structure-origin").value; if (!selected) throw Error("Аудит ще не має результатів HTTP/HTTPS");
    const params = new URLSearchParams({ origin: selected, prefix, offset: structureOffset, day: $("structure-day").value, country: $("structure-country").value.trim(), device: $("structure-device").value });
    const data = await api(base + "/structure?" + params); if (serial !== structureSerial) return;
    $("structure-path").textContent = prefix; $("structure-up").disabled = prefix === "/";
    $("structure-notes").textContent = data.note;
    $("structure-status").textContent = (data.meta.partial ? "Неповне спостережене покриття. " : "") + `Результатів: ${data.meta.saved_results}; цілей аудиту: ${data.meta.snapshot_targets}. ` + (data.day ? `GSC: ${data.day}; раніше: ${data.previous_day || "немає"}. ` : "GSC не імпортовано для цієї дати. ") + (data.geo_report_id ? `GEO-запитів без цілі: ${data.unmapped_queries}. ` : "GEO-даних немає. ") + (data.bot_import_id ? "" : "Журнал ботів не прикріплений.");
    $("structure-rows").replaceChildren(...data.items.map(v => {
      const row = el("tr"), section = el("td");
      if (v.children && v.path !== prefix) { const b = el("button", v.path); b.className = "node-link"; b.onclick = safe("structure", () => { prefix = v.path; structureOffset = 0; return loadStructure(); }); section.append(b); } else section.textContent = v.path === prefix ? prefix + " (без вкладених URL)" : v.path;
      const traffic = el("td", `${value(v.impressions)} / ${value(v.clicks)} / ${value(v.previous_clicks)}`);
      if (v.traffic_comparable && v.clicks < v.previous_clicks) traffic.append(el("small", "Кліків менше на " + value(v.previous_clicks - v.clicks)));
      else if (v.previous_clicks != null && !v.traffic_comparable) traffic.append(el("small", "Набори спостережень різні: зміна не розрахована"));
      row.append(section, el("td", value(v.pages)), el("td", `${v.http_errors} / ${v.failed} / ${v.robots_blocked}`), el("td", `${v.noindex} / ${v.indexing_unknown}`), el("td", value(v.nonself_canonical)), el("td", data.geo_report_id ? `${v.geo_pages} / ${v.geo_queries}` : "Немає даних"), traffic, el("td", `${value(v.bot_visits)} / ${value(v.verified_bot_visits)} / ${value(v.bot_errors)}`)); return row;
    }));
    $("structure-prev").disabled = structureOffset === 0; $("structure-next").disabled = !data.more;
  }
  document.addEventListener("seo:landing-open", safe("landing", loadLanding));
  document.addEventListener("seo:structure-open", safe("structure", loadStructure));
  $("landing-refresh").onclick = safe("landing", async () => { await loadImports(); await loadLanding(); });
  $("structure-refresh").onclick = safe("structure", async () => { await loadImports(); await loadStructure(); });
  for (const key of ["landing-country", "landing-device", "landing-geo"]) $(key).onchange = safe("landing", () => { landingOffset = 0; return loadLanding(); });
  $("search-origin").onchange = safe("landing", () => { origin = $("search-origin").value; $("landing-geo").value = ""; geoOptions(); landingOffset = 0; return loadLanding(); });
  for (const key of ["structure-origin", "structure-day"]) $(key).onchange = safe("structure", () => { prefix = "/"; structureOffset = 0; return loadStructure(); });
  for (const key of ["structure-country", "structure-device"]) $(key).onchange = safe("structure", () => { structureOffset = 0; return loadStructure(); });
  $("structure-up").onclick = safe("structure", () => { prefix = prefix.replace(/[^/]+\/$/, ""); structureOffset = 0; return loadStructure(); });
  for (const [view, size, load] of [["landing", 50, loadLanding], ["structure", 50, loadStructure], ["landing-history", 100, loadHistory]]) for (const direction of ["prev", "next"]) $(view + "-" + direction).onclick = safe(view, () => { const step = size * (direction === "prev" ? -1 : 1); if (view === "landing") landingOffset = Math.max(0, landingOffset + step); else if (view === "structure") structureOffset = Math.max(0, structureOffset + step); else historyOffset = Math.max(0, historyOffset + step); return load(); });
  $("search-import-form").onsubmit = safe("search-import", async event => {
    event.preventDefault(); const button = event.target.querySelector('button[type="submit"]'); button.disabled = true;
    try { const file = $("search-file").files[0]; if (!file || file.size > 4 * 1024 * 1024) throw Error("Потрібен CSV до 4 МіБ"); const data = await api(base + "/search-imports", { id: crypto.randomUUID(), origin: $("search-origin").value, csv: await file.text() }); $("search-import-status").textContent = `Збережено рядків: ${data.rows}`; landingOffset = 0; await loadImports(); await loadLanding(); } finally { button.disabled = false; }
  });
})();
