"use strict";
window.seoAIPolicy = {
  async init({api, el, reportID}) {
    const $ = id => document.getElementById(id);
    const labels = {allowed:"Дозволено правилами",blocked:"Заборонено правилами",unknown:"Не перевірено"};
    const desiredLabels = {...labels,unknown:"Не задано"};
    const driftLabels = {changed:"Змінилося",stable:"Без змін",not_comparable:"Немає порівнюваних даних",no_baseline:"Немає попереднього запуску"};
    const date = value => value ? new Date(value).toLocaleString("uk-UA") : "—";
    let cursor = null;
    const cursors = [null];

    function cell(value, className) {
      return el("td", value == null || value === "" ? "—" : value, className || "");
    }

    function showSummary(data) {
      const body = $("ai-policy-summary").querySelector("tbody");
      body.replaceChildren();
      for (const item of data.summary) {
        const spec = data.specs.find(entry => entry.token.toLowerCase() === item.token.toLowerCase());
        const row = el("tr");
        const token = el("td");
        token.append(el("strong", item.token), el("small", `${spec?.provider || ""} · ${spec?.purpose_label || ""}${spec && !spec.has_http_user_agent ? " · керувальний токен у robots.txt" : ""}`));
        const key = item.token.toLowerCase();
        row.append(token, cell(desiredLabels[data.desired_policy[key]] || "Не задано"), cell(item.allowed), cell(item.blocked), cell(item.unknown), cell(item.mismatches), cell(item.conflicts), cell(item.drift));
        body.append(row);
      }
    }

    function showPages(items) {
      const body = $("ai-policy-pages").querySelector("tbody");
      body.replaceChildren();
      for (const item of items) {
        const row = el("tr"), url = el("td");
        url.append(el("code", item.url), el("small", `Target ${item.target_id} · ${item.scan_status}${item.http_status ? ` · HTTP ${item.http_status}` : ""}`));
        const matrixCell = el("td"), details = el("details"), summary = el("summary", `Переглянути ${item.controls.length} правил`);
        const wrap = el("div", null, "table-wrap ai-policy-detail-wrap"), table = el("table"), head = el("thead"), headRow = el("tr");
        for (const title of ["Бот / призначення","Бажано","robots.txt","Директива","Попередній стан","Впевненість і перевірка"]) headRow.append(el("th", title));
        head.append(headRow); table.append(head);
        const detailBody = el("tbody");
        for (const control of item.controls) {
          const spec = control.spec, evidence = control.evidence, detail = el("tr"), name = el("td");
          name.append(el("strong", spec.token), el("small", `${spec.provider} · ${spec.purpose_label}${!spec.has_http_user_agent ? " · керувальний токен" : ""}`));
          const desired = el("td", desiredLabels[control.desired] || "Не задано");
          if (control.mismatch) desired.className = "ai-policy-mismatch";
          const status = el("td", labels[evidence.status] || labels.unknown);
          status.className = "ai-policy-state ai-policy-" + (evidence.status || "unknown");
          if (evidence.conflict) status.append(el("small", "Конфлікт Allow / Disallow"));
          const directive = el("td", evidence.directive || "—");
          directive.className = "ai-policy-directive";
          if (evidence.conflict_directive) directive.append(el("small", `Також збіглося: ${evidence.conflict_directive}`));
          const previous = el("td", `${labels[control.previous_status] || "Не перевірено"} · ${driftLabels[control.drift] || control.drift}`);
          const provenance = el("td", `${evidence.confidence || "unknown"} · ${date(evidence.checked_at)}${evidence.robots_http_status ? ` · HTTP ${evidence.robots_http_status}` : ""}`);
          if (evidence.robots_url) provenance.append(el("small", evidence.robots_url));
          if (evidence.explanation) provenance.append(el("small", evidence.explanation));
          detail.append(name, desired, status, directive, previous, provenance); detailBody.append(detail);
        }
        table.append(detailBody); wrap.append(table); details.append(summary, wrap); matrixCell.append(details);
        row.append(url, cell(item.http_status), cell(item.mismatches), cell(item.conflicts), cell(item.policy_changes), matrixCell);
        body.append(row);
      }
      if (!items.length) {
        const row = el("tr"), empty = cell("У цьому SEO-запуску немає збережених URL."); empty.colSpan = 6; row.append(empty); body.append(row);
      }
    }

    async function load() {
      const params = new URLSearchParams({limit:"50"});
      if (cursor != null) params.set("after", String(cursor));
      const data = await api(`/api/geo/reports/${reportID}/ai-discoverability?${params}`);
      const specs = await api("/api/geo/ai-policy-specs");
      data.specs = specs;
      const baseline = data.baseline_run_id ? `Порівняння з попереднім завершеним GEO-запуском ${data.baseline_run_id}.` : "Попереднього порівнюваного планового запуску немає.";
      $("ai-policy-meta").textContent = `${data.domain} · матриця збережена для ${data.pages_with_matrix} із ${data.pages_total} URL. ${data.desired_configured ? "Бажана політика зафіксована профілем на момент запуску." : "Для цього звіту бажану політику не налаштовано."} ${baseline} robots.txt не підтверджує доступність із мережі провайдера, індексацію або цитування.`;
      showSummary(data); showPages(data.items);
      $("ai-policy-page-label").textContent = data.pages_total ? `${cursor == null ? 1 : "після " + cursor} · на сторінці ${data.items.length} · усього URL ${data.pages_total}` : "Немає URL";
      $("ai-policy-prev").disabled = cursors.length <= 1;
      $("ai-policy-next").disabled = !data.next_after;
      $("ai-policy-next").dataset.nextAfter = data.next_after == null ? "" : String(data.next_after);
    }
    $("ai-policy-refresh").addEventListener("click", async () => { cursor = null; cursors.splice(0, cursors.length, null); await load(); });
    $("ai-policy-next").addEventListener("click", async () => { const next = $("ai-policy-next").dataset.nextAfter; if (!next) return; cursor = Number(next); cursors.push(cursor); await load(); });
    $("ai-policy-prev").addEventListener("click", async () => { if (cursors.length <= 1) return; cursors.pop(); cursor = cursors[cursors.length - 1]; await load(); });
    await load();
  }
};
