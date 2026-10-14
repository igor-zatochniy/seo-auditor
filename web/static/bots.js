"use strict";
(() => {
  const $ = id => document.getElementById(id);
  const el = (tag,text,cls) => {const n=document.createElement(tag);if(text!=null)n.textContent=String(text);if(cls)n.className=cls;return n;};
  const verification={matched_range:"Збіг із поточним офіційним списком",range_mismatch:"IP не збігається зі списком",unverified:"Не підтверджено"};
  const logStatus={unknown:"Невідомо",not_available:"Немає журналу",key_mismatch:"Несумісний ключ",no_matching_events:"Подій URL немає"};
  let selected="",cursors=[0],next=0,requestID="",lastPayload="",generation=0;
  const notice=text=>{$("notice").textContent=text;$("notice").hidden=!text;};
  async function api(path,body,token){const headers={"X-SEO-Auditor-Request":"1"};if(body!==undefined)headers["Content-Type"]="application/json";if(token)headers.Authorization="Bearer "+token;const res=await fetch(path,{method:body===undefined?"GET":"POST",headers,credentials:"same-origin",body:body===undefined?undefined:JSON.stringify(body)});const data=await res.json();if(!res.ok)throw Error(data.error?.message||"Запит не виконано");return data;}
  const safe=fn=>async(...args)=>{try{notice("");await fn(...args);}catch(e){notice(e.message);}};
  const date=v=>new Date(v).toLocaleString("uk-UA");
  async function networks(){const items=await api("/api/bots/networks");const box=$("network-status");box.replaceChildren();for(const item of items)box.append(el("span",`${item.label}: ${item.fresh ? item.count+" діапазонів" : "потрібне оновлення"}`,"badge "+(item.fresh?"success":"warning")));}
  async function imports(){const items=await api("/api/bots/imports");const box=$("bot-imports");box.replaceChildren();for(const item of items){const button=el("button",`${item.domain} · ${item.stats.accepted} візитів · ${date(item.created_at)}`);button.onclick=safe(async()=>{selected=item.id;cursors=[0];await report();});box.append(button);}if(!items.length)box.append(el("p","Імпортів ще немає","muted"));}
  async function report(){
    const current=++generation;
    const data=await api(`/api/bots/imports/${encodeURIComponent(selected)}?after=${cursors.at(-1)}`);
    if(current!==generation)return;
    $("bot-report").hidden=false;$("bot-report-title").textContent=data.summary.domain;
    const stats=data.summary.stats;
    $("bot-report-stats").textContent=`${stats.lines} рядків · ${stats.accepted} візитів · ${stats.invalid} некоректних · ${stats.foreign_domain} поза доменом · імпорт ${date(data.summary.created_at)}`;
    const totals=$("bot-totals");totals.replaceChildren();
    for(const item of data.totals){
      const evidence=data.summary.range_evidence?.[item.bot];
      const stamp=evidence?" · список від "+date(evidence):"";
      totals.append(el("span",`${item.bot}: ${item.visits} · ${verification[item.verification]}${stamp}`,"badge"));
    }
    const rows=$("bot-rows");rows.replaceChildren();
    for(const item of data.items){
      const tr=el("tr");
      tr.append(el("td",item.safe_url),el("td",item.bot),el("td",verification[item.verification]),el("td",item.visits),el("td",(item.status_codes||[]).join(", ")+" · помилок "+item.errors),el("td",date(item.first)+" / "+date(item.last)));
      rows.append(tr);
    }
    if(!data.items.length){const td=el("td","Візитів ботів у цьому імпорті немає","empty");td.colSpan=6;const tr=el("tr");tr.append(td);rows.append(tr);}
    next=data.next;$("bot-prev").disabled=cursors.length===1;$("bot-next").disabled=!next;$("bot-page-count").textContent="Сторінка "+cursors.length;
  }
  const fetchLabels={html_received:"HTML отримано",challenge_suspected:"Підозра на challenge",http_rejected:"HTTP-відмова або тимчасова помилка",http_error:"HTTP-помилка",non_html:"Не HTML",empty_html:"Порожній HTML",blocked_by_robots:"Заборонено robots.txt",unknown:"Невідомо",network_error:"Мережева помилка",external_redirect:"Зовнішній редирект",redirect_limit:"Ліміт редиректів",redirect_invalid:"Некоректний редирект",body_read_error:"Помилка читання"};
  const challengeLabels={cloudflare_challenge:"ознаки Cloudflare challenge",challenge_platform:"challenge-platform",browser_check:"перевірка браузера",human_verification:"перевірка людини",captcha_required:"вимога CAPTCHA",attention_required:"Attention Required",access_denied:"Access denied",captcha_marker:"маркер CAPTCHA"};
  function fetchObservation(title,item,box){
    const section=el("section",null,"ai-fetch-observation");
    section.append(el("h3",title));
    const robots=item.robots_status==="allowed"?"дозволено robots.txt":item.robots_status==="blocked"?"заборонено robots.txt":"robots.txt: невідомо";
    section.append(el("p",`${fetchLabels[item.status]||"Не перевірено"} · ${robots}${item.http_status?` · HTTP ${item.http_status}`:""}${item.duration_ms?` · ${item.duration_ms} мс`:""}`,"badge"));
    if(item.explanation)section.append(el("p",item.explanation,"muted"));
    if(item.content_type)section.append(el("p",`Content-Type: ${item.content_type}; HTML: ${item.html_content_type?"так":"ні"}; видимих слів у відповіді: ${item.visible_words??0}; отримано байтів: ${item.body_bytes??0}${item.body_truncated?" (відповідь обрізано)":""}.`));
    if(item.challenge_suspected)section.append(el("p",`Знайдені сигнали: ${(item.challenge_signals||[]).map(v=>challengeLabels[v]||"ознака challenge").join(", ")||"евристичний збіг"}. Це підозра, а не остаточний висновок.`,"error"));
    if(item.redirects?.length){const list=el("ul");for(const redirect of item.redirects)list.append(el("li",redirect));section.append(list);}
    if(item.final_url)section.append(el("p",`Фінальний URL: ${item.final_url}`,"muted"));
    box.append(section);
  }
  function renderFetchDiagnostics(data){
    const box=$("ai-fetch-report");box.replaceChildren();box.hidden=false;
    box.append(el("h3",`${data.bot} · ${data.url}`));
    if(data.discrepancy)box.append(el("p","Є потенційна розбіжність: Auditor отримав HTML, а симуляція User-Agent — іншу відповідь. Перевірте CDN/WAF, редиректи й узгодження контенту; причина не встановлена.","warning"));
    fetchObservation("Запит SEO Auditor",data.auditor_simulation,box);
    fetchObservation("Симуляція User-Agent бота",data.bot_simulation,box);
    box.append(el("p",data.disclaimer,"metric-note"));
    box.append(el("h3","Фактичні події з імпортованих журналів"));
    const items=data.real_log_observations||[];
    if(!items.length){box.append(el("p","Немає сумісного імпорту журналів для URL.","muted"));return;}
    const table=el("table"),head=el("tr");for(const col of ["Джерело","Бот","Перевірка IP","Візити","Помилки HTTP","HTTP-коди","Період","Пояснення"])head.append(el("th",col));table.append(head);
    for(const item of items){const row=el("tr"),period=item.first&&item.last?`${date(item.first)} — ${date(item.last)}`:"—",explanation=`${item.explanation||"—"}${item.repeated_429_503?" Виявлені повторні відповіді 429/503.":""}`;for(const value of [item.import_domain||"—",item.bot||"—",verification[item.verification]||logStatus[item.status]||"—",item.visits??"—",item.failures??"—",(item.http_statuses||[]).join(", ")||"—",period,explanation])row.append(el("td",value));if(item.repeated_429_503)row.classList.add("warning");table.append(row);}
    const wrap=el("div",null,"table-wrap");wrap.append(table);box.append(wrap);
  }
  async function runFetchDiagnostics(event){
    event.preventDefault();const button=$("ai-fetch-submit"),report=$("ai-fetch-report");button.disabled=true;report.replaceChildren();report.hidden=true;$("ai-fetch-status").textContent="Перевірка виконується…";
    try{const data=await api("/api/bots/fetch-diagnostics",{url:$("ai-fetch-url").value,bot:$("ai-fetch-bot").value});renderFetchDiagnostics(data);$("ai-fetch-status").textContent="Готово";}
    catch(error){$("ai-fetch-status").textContent="Перевірку не завершено";throw error;}
    finally{button.disabled=false;}
  }
  async function importLog(event){
    event.preventDefault();$("bot-import").disabled=true;
    try{
      const input={domain:$("bot-domain").value,format:$("bot-format").value,log:$("bot-log").value};
      const signature=JSON.stringify(input);
      if(signature!==lastPayload){lastPayload=signature;requestID=crypto.randomUUID();}
      const result=await api("/api/bots/imports",{...input,id:requestID});selected=result.id;cursors=[0];
      $("bot-import-status").textContent=result.reused?"Цей журнал уже імпортовано; попередню оцінку не змінено":`Збережено ${result.stats.accepted} візитів`;
      $("bot-log").value="";$("bot-file").value="";lastPayload="";requestID="";
      $("bot-file-name").textContent="Файл не обрано";
      await imports();await report();
    }finally{$("bot-import").disabled=false;}
  }
  async function init(){
    const token=new URLSearchParams(location.hash.slice(1)).get("access_token");
    if(token){history.replaceState(null,"",location.pathname);await api("/api/session",{},token);}
    await networks();await imports();
    $("refresh-networks").onclick=safe(async()=>{
      $("refresh-networks").disabled=true;
      try{const items=await api("/api/bots/networks/refresh",{});await networks();if(items.some(i=>!i.ok))notice("Частину IP-списків не оновлено; перевірте підключення до офіційних джерел");}
      finally{$("refresh-networks").disabled=false;}
    });
    $("bot-file-pick").onclick=()=>$("bot-file").click();
    $("bot-file").onchange=safe(async()=>{const file=$("bot-file").files[0];if(!file)return;if(file.size>8*1024*1024)throw Error("Файл перевищує 8 МіБ");$("bot-file-name").textContent=file.name;$("bot-log").value=await file.text();});
    $("bot-import-form").onsubmit=safe(importLog);
    $("ai-fetch-form").onsubmit=safe(runFetchDiagnostics);
    $("bot-prev").onclick=safe(async()=>{cursors.pop();await report();});
    $("bot-next").onclick=safe(async()=>{cursors.push(next);await report();});
  }
  init().catch(e=>notice(e.message));
})();
