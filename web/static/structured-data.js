"use strict";
window.seoStructuredData = {
  details(report) {
    const section = document.createElement("section");
	section.className="structured-report";
    const add = (tag, text, cls) => { const node = document.createElement(tag); node.textContent = text; if (cls) node.className = cls; section.append(node); return node; };
    add("h3", "Діагностика JSON-LD");
    if (!report) { add("p", "Не перевірено у цьому аудиті", "muted"); return section; }
    add("p", `${report.valid_blocks} синтаксично коректних блоків із ${report.blocks} · ${report.types?.join(", ") || "Типи не визначено"}`);
    if (!report.complete) add("p", "Перевірка неповна: досягнуто межу розбору", "badge warning");
    if (!report.issues?.length) add("p", report.blocks ? "У межах локальних правил застережень не знайдено" : "JSON-LD відсутня", "muted");
    for (const issue of report.issues || []) {
      const line = add("p", `Блок ${issue.block} · ${issue.path}${issue.offset ? " · byte " + issue.offset : ""}: ${issue.message}`);
      line.dataset.severity = issue.severity;
    }
    return section;
  }
};
