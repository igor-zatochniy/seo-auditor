"use strict";
window.seoDiagnostics = (() => {
 const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=String(text);if(cls)n.className=cls;return n;};
 const rules={allowed:"Дозволено правилами",blocked:"Заборонено",unknown:"Не перевірено"};
 const ratings={good:"Добре",needs_improvement:"Потребує покращення",poor:"Погано",unknown:"Не виміряно"};
 const states={completed:"Виміряно",partial:"Неповний вимір",failed:"Помилка вимірювання"};
 const eligibilityStates={allowed:"Заборони не виявлено",blocked:"Виявлено технічні обмеження",limited:"Фрагменти обмежено",unknown:"Недостатньо перевірених даних"};
 const aiPurpose={"Googlebot":"Пошук Google та AI-функції пошуку","Google-Extended":"Використання в окремих Google AI-продуктах","OAI-SearchBot":"Пошук ChatGPT","GPTBot":"Навчання моделей OpenAI","Claude-SearchBot":"Пошук у Claude","ClaudeBot":"Покращення моделей Anthropic","PerplexityBot":"Пошукове індексування Perplexity","Applebot":"Spotlight, Siri та пошук Apple","Applebot-Extended":"Навчання моделей Apple"};
 const confidence={robots_parsed:"robots.txt прочитано",robots_unavailable:"robots.txt недоступний · не підтверджує мережевий доступ",unknown:"Недостатньо даних"};
 function eligibility(parent,value){
  const grid=el("div",null,"eligibility-grid");
  for(const [key,title]of [["google_ai","Google AI · технічні передумови"],["chatgpt_search","ChatGPT Search · технічні передумови"]]){
   const check=value?.[key],status=check?.status||"unknown",section=el("section",null,"eligibility-section");
   section.append(el("h3",title),el("span",eligibilityStates[status]||eligibilityStates.unknown,"badge "+({allowed:"success",blocked:"error",limited:"warning"}[status]||"")));
   const list=el("ul");for(const reason of check?.reasons||["Для зведеної перевірки потрібен новий SEO-аудит."])list.append(el("li",reason));section.append(list);grid.append(section);
  }
  parent.append(grid);
 }
 function facts(section,items){const list=el("dl");for(const [k,v]of items)list.append(el("dt",k),el("dd",v));section.append(list);}
 function details(s,matched,includeEligibility=true){
  const section=el("section",null,"geo-diagnostics");section.append(el("h3","Технічні обмеження для AI-пошуку"));
  if(includeEligibility)eligibility(section,s?.eligibility);
  const r=s?.search;
  facts(section,[["Googlebot · robots.txt",rules[r?.googlebot_rules]||rules.unknown],["OAI-SearchBot · пошук ChatGPT",rules[r?.oai_searchbot_rules]||rules.unknown],["GPTBot · навчання",rules[r?.gptbot_rules]||rules.unknown],["Google · індексація за директивами",rules[r?.google_indexing]||rules.unknown],["Google · snippet за директивами",rules[r?.google_snippet]||rules.unknown],["HTTP нашого клієнта",s?.http?.status??"Не перевірено"]]);
  section.append(el("h4","Повна матриця robots.txt для AI-ботів"));
  if(!r?.ai_policies?.length)section.append(el("p","Матриця не збережена в цьому аудиті. Запустіть новий SEO-аудит."));
  else {
   const wrap=el("div",null,"table-wrap"),table=el("table"),head=el("thead"),header=el("tr");
   for(const title of ["Провайдер / токен","Призначення","Правило для URL","Директива","Джерело / час"])header.append(el("th",title));head.append(header);table.append(head);
   const body=el("tbody");
   for(const item of r.ai_policies){const row=el("tr"),status=el("td",rules[item.status]||rules.unknown);status.className="ai-policy-state ai-policy-"+(item.status||"unknown");
    const token=el("td",`${item.provider||""} · ${item.token||""}${["Google-Extended","Applebot-Extended"].includes(item.token)?" · керувальний токен, не HTTP User-Agent":""}`);
    const evidenceParts=[confidence[item.confidence]||confidence.unknown];
    if(item.robots_http_status)evidenceParts.push(`HTTP ${item.robots_http_status}`);
    if(item.checked_at)evidenceParts.push(new Date(item.checked_at).toLocaleString("uk-UA"));
    if(item.robots_url)evidenceParts.push(item.robots_url);
    const provenance=el("td",evidenceParts.join(" · "));
    if(item.explanation)provenance.append(el("small",item.explanation));
    const directive=el("td",item.directive||"—");if(item.conflict_directive)directive.append(el("small",`Також збіглося: ${item.conflict_directive}`));
    row.append(token,el("td",aiPurpose[item.token]||item.purpose||"Не визначено"),status,directive,provenance);body.append(row);
   }
   table.append(body);wrap.append(table);section.append(wrap);
  }
  section.append(el("p","Google AI потребує індексації та дозволеного snippet, але фактична індексація не перевіряється. Правила Google щодо snippet не переносяться на ChatGPT. Правила й HTTP нашого клієнта не підтверджують доступ із IP AI-ботів, відсутність WAF або цитування. Заборона GPTBot не є забороною ChatGPT Search.","metric-note"));
  section.append(el("h3","Швидкість і стабільність"));
  const http=s?.http,p=s?.performance;
  const timing=http?.response_ms;
  facts(section,[["Від надсилання HTTP-запиту до першого байта",timing==null?"Не виміряно":`${timing} мс${http.slow?" · понад внутрішній орієнтир 1 с":""}`],["Лабораторний прохід",states[p?.status]||"Не виконано"],["LCP",p?.lcp_ms==null?"Не виміряно":`${(p.lcp_ms/1000).toFixed(2)} с · ${ratings[p.lcp_rating]||ratings.unknown}`],["CLS",p?.cls==null?"Не виміряно":`${p.cls.toFixed(3)} · ${ratings[p.cls_rating]||ratings.unknown}`]]);
  section.append(el("p","Локальний desktop-вимір 1280×900 через захищений брокер із rate limit; спостереження 5 с після load. Не польові CWV та не критерій допуску AI. Орієнтири: LCP ≤2,5 с; CLS ≤0,1.","metric-note"));
  if(p?.error)section.append(el("p",p.error,"error"));
  if(p?.blocked)section.append(el("p",`Ресурсів заблоковано: ${p.blocked}. Показники можуть бути спотворені.`,"metric-note"));
  for(const warning of p?.warnings||[])section.append(el("p",warning,"metric-note"));
  section.append(el("h3","Фрагменти 300–500 символів"));
  const blocks=s?.blocks;
  if(!blocks)section.append(el("p","Немає даних цієї перевірки. Потрібен новий SEO-аудит."));
  else {
   section.append(el("p",`Знайдено абзаців: ${blocks.count}. ${blocks.complete?"Вибірка повна.":"Вибірка обмежена; відсутність збігу не є доказом відсутності відповіді."}`));
   const items=matched??blocks.items??[];
   if(matched&&!items.length)section.append(el("p","У збереженій вибірці немає лексичного збігу із запитом."));
   for(const block of items){section.append(el("h4",block.heading||"Без найближчого заголовка"),el("p",block.text),el("p",`${block.characters} символів${block.coverage==null?"":` · збіг термінів запиту: ${block.coverage}%`}`,"muted"));}
  }
  section.append(el("p","300–500 символів — вибраний орієнтир, не вимога AI-систем. Збіг термінів є евристикою; окремий намір і повноту відповіді потрібно перевірити вручну.","metric-note"));
  return section;
 }
 return {details,eligibility};
})();
