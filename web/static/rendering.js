"use strict";
window.seoRendering = (() => {
 const labels={completed:"Перевірено",partial:"Частково",failed:"Помилка",disabled:"Вимкнено",not_applicable:"Не застосовується"};
 const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
 const value=v=>v==null||v===""?"—":Array.isArray(v)?v.join("\n"):String(v);
 function badge(status){return el("span",labels[status]||"Не перевірено","badge "+(status==="completed"?"success":status==="failed"?"error":status==="partial"?"warning":""));}
 function details(record){
  const section=el("section",null,"render-comparison"),r=record.rendering;
  const heading=el("h3","JavaScript ");heading.append(badge(record.rendering_status));section.append(heading);
  if(!r)return section;
  if(r.error)section.append(el("p",r.error,"error"));
  section.append(el("p",`${r.browser||"Chromium"} · ${r.duration_ms} ms · запити ${r.requests} · заблоковано ${r.blocked}`,"muted"));
  for(const warning of r.warnings||[])section.append(el("p",warning,"metric-note"));
  if(r.status==="partial")section.append(el("p","Неповне завантаження або обмежена вибірка: відсутність змін не підтверджує незалежність від JavaScript.","metric-note"));
  const wrap=el("div",null,"table-wrap"),table=el("table"),head=el("tr");
  for(const name of ["Метрика","HTML відповіді","DOM після JavaScript","Зміна"])head.append(el("th",name));table.append(head);
  const metrics=[["Title","title","title"],["Description","description","description"],["Canonical","canonical","canonical"],["Meta robots","robots","robots"],["X-Robots-Tag","x_robots_tag"],["H1","h1","h1"],["Кількість H1","h1_count","h1"],["Слова","words","text"],["Вибірка тексту","text_sample","text"],["Внутрішні посилання","internal_links","links"],["Зовнішні посилання","external_links","links"],["Блоки JSON-LD","json_ld_count","json_ld"],["Hreflang","hreflang","hreflang"]];
  for(const [label,key,delta]of metrics){const row=el("tr");row.append(el("th",label));for(const source of [r.raw,r.rendered])row.append(el("td",value(source?.[key])));row.append(el("td",r.delta&&delta?(r.delta[delta]?"Змінено":"Без змін"):"—"));table.append(row);}
  wrap.append(table);section.append(wrap);
  if(r.delta){const d=r.delta,list=el("ul");for(const message of [d.canonical?"Canonical змінено після JavaScript.":"",d.h1_only_after_rendering?"H1 з’явився лише після рендерингу.":"",d.json_ld_injected?"JSON-LD додано після рендерингу.":"",d.added_internal_links?`Нових унікальних внутрішніх посилань: ${d.added_internal_links}.`:"",d.removed_internal_links?`Зниклих внутрішніх посилань: ${d.removed_internal_links}.`:"",d.text_growth_percent!=null?`Чистий приріст слів: ${d.text_growth_percent}% від тексту DOM. Це не частка тексту, залежного від JS.`:""].filter(Boolean))list.append(el("li",message));section.append(list);}
  return section;
 }
 return {badge,details};
})();
