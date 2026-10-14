"use strict";
window.seoRendering = (() => {
 const labels={completed:"Перевірено",partial:"Частково",failed:"Помилка",disabled:"Вимкнено",not_applicable:"Не застосовується",high:"Висока залежність",warning:"Можлива залежність",unknown:"Невідомо",not_detected:"Не виявлено"};
 const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
 const value=v=>v==null||v===""?"—":Array.isArray(v)?v.join("\n"):String(v);
 function badge(status){return el("span",labels[status]||"Не перевірено","badge "+(status==="completed"||status==="not_detected"?"success":status==="failed"||status==="high"?"error":status==="partial"||status==="warning"?"warning":""));}
 function details(record){
  const section=el("section",null,"render-comparison"),r=record.rendering;
  const heading=el("h3","JavaScript ");heading.append(badge(record.rendering_status));section.append(heading);
  if(!r)return section;
  if(r.error)section.append(el("p",r.error,"error"));
  section.append(el("p",`${r.browser||"Chromium"} · ${r.duration_ms} ms · запити ${r.requests} · заблоковано ${r.blocked}`,"muted"));
  for(const warning of r.warnings||[])section.append(el("p",warning,"metric-note"));
	const reasons={invalid_target:"Заборонена адреса",robots_denied:"Заборонено robots.txt",request_failed:"Мережевий збій",http_status:"HTTP-помилка",body_read_failed:"Помилка читання або ліміт розміру",request_limit:"Ліміт запитів",transfer_limit:"Ліміт ресурсів",unsupported_request:"Запит не дозволений renderer",fulfill_failed:"Помилка передачі Chromium",resource_error:"Помилка ресурсу"};
	if(r.resources?.length){section.append(el("h4","Ресурси з помилками"));const list=el("ul");for(const issue of r.resources)list.append(el("li",`${issue.type}: ${issue.url} — ${reasons[issue.code]||"Помилка ресурсу"}${issue.http?" (HTTP "+issue.http+")":""}${issue.url_truncated?" · URL скорочено":""}`));section.append(list);}
	if(r.resources_truncated)section.append(el("p","Список ресурсів з помилками обмежено.","metric-note"));
	if(r.links_truncated)section.append(el("p","Набір DOM-посилань неповний.","metric-note"));
  if(r.status==="partial")section.append(el("p","Неповне завантаження або обмежена вибірка: відсутність змін не підтверджує незалежність від JavaScript.","metric-note"));
  const dependency=r.main_content_dependency;
  if(dependency){const panel=el("div",null,"main-content-dependency");const title=el("h4","Залежність основного вмісту від JavaScript");title.append(badge(dependency.status));panel.append(title);panel.append(el("p",dependency.evidence||"Оцінку не сформовано."));panel.append(el("p",`Основний текст: HTML ${dependency.raw_words??"—"} слів (${dependency.raw_source||"невідомо"}) → DOM ${dependency.rendered_words??"—"} слів (${dependency.rendered_source||"невідомо"})${dependency.similarity_percent!=null?` · схожість ${dependency.similarity_percent}%`:""}.`,"muted"));panel.append(el("p","Евристичний сигнал, а не підтвердження індексації чи цитування пошуковими або AI-системами.","metric-note"));section.append(panel);}
  const agentic=r.agentic;
  if(agentic){
    const panel=el("div",null,"main-content-dependency agentic-readiness");
    const title=el("h4","Доступність інтерфейсу для AI-агентів ");title.append(badge(agentic.status));panel.append(title);
    if(agentic.explanation)panel.append(el("p",agentic.explanation,"muted"));
    panel.append(el("p",`Орієнтири основного вмісту (main): ${agentic.main_landmarks??0} · навігаційні орієнтири: ${agentic.navigation_landmarks??0} · інтерактивних елементів без доступного імені: ${agentic.unnamed_interactive??0} · полів форм без доступного імені: ${agentic.unnamed_form_controls??0}.`));
    if(agentic.findings?.length){const list=el("ul");for(const finding of agentic.findings)list.append(el("li",`${finding.element||"Елемент"}${finding.role?` · роль ${finding.role}`:""}: ${finding.message||finding.code}`));panel.append(list);}
    else if(agentic.status==="completed")panel.append(el("p","У перевірених вузлах не виявлено інтерактивних елементів без доступного імені.","muted"));
    if(agentic.findings_truncated)panel.append(el("p","Перелік знахідок обмежено; агреговані лічильники охоплюють доступне дерево.","metric-note"));
    panel.append(el("p",`WebMCP: ${agentic.declarative_webmcp_forms>0?`виявлено декларативних форм: ${agentic.declarative_webmcp_forms}`:"декларативних форм не виявлено"}. ${agentic.webmcp_note||""}`,"metric-note"));
    panel.append(el("p","Це локальна перевірка accessibility tree через Chromium, а не оцінка Lighthouse, гарантія сумісності з агентами чи фактор ранжування.","metric-note"));
    section.append(panel);
  }
  const wrap=el("div",null,"table-wrap"),table=el("table"),head=el("tr");
  for(const name of ["Метрика","HTML відповіді","DOM після JavaScript","Зміна"])head.append(el("th",name));table.append(head);
  const metrics=[["Title","title","title"],["Description","description","description"],["Canonical","canonical","canonical"],["Meta robots","robots","robots"],["X-Robots-Tag","x_robots_tag"],["H1","h1","h1"],["Кількість H1","h1_count","h1"],["Слова","words","text"],["Слова основного вмісту","main_content_words","text"],["Джерело основного вмісту","main_content_source","text"],["Вибірка тексту","text_sample","text"],["Внутрішні посилання","internal_links","links"],["Зовнішні посилання","external_links","links"],["Блоки JSON-LD","json_ld_count","json_ld"],["Hreflang","hreflang","hreflang"]];
  for(const [label,key,delta]of metrics){const row=el("tr");row.append(el("th",label));for(const source of [r.raw,r.rendered])row.append(el("td",value(source?.[key])));row.append(el("td",r.delta&&delta?(r.delta[delta]?"Змінено":"Без змін"):"—"));table.append(row);}
  wrap.append(table);section.append(wrap);
	if(r.rendered?.structured_data&&window.seoStructuredData){const diagnostics=window.seoStructuredData.details(r.rendered.structured_data);diagnostics.querySelector("h3").textContent="Діагностика JSON-LD після JavaScript";section.append(diagnostics);}
  if(r.delta){const d=r.delta,list=el("ul");for(const message of [d.canonical?"Canonical змінено після JavaScript.":"",d.h1_only_after_rendering?"H1 з’явився лише після рендерингу.":"",d.json_ld_injected?"JSON-LD додано після рендерингу.":"",d.added_internal_links?`Нових унікальних внутрішніх посилань: ${d.added_internal_links}.`:"",d.removed_internal_links?`Зниклих внутрішніх посилань: ${d.removed_internal_links}.`:"",d.text_growth_percent!=null?`Чистий приріст слів: ${d.text_growth_percent}% від тексту DOM. Це не частка тексту, залежного від JS.`:""].filter(Boolean))list.append(el("li",message));section.append(list);}
  return section;
 }
 return {badge,details};
})();
