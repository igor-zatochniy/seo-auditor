"use strict";
(() => {
  const $ = id => document.getElementById(id);
  const el = (tag,text,cls) => {const n=document.createElement(tag);if(text!=null)n.textContent=String(text);if(cls)n.className=cls;return n;};
  const dates = value => value ? new Date(value).toLocaleString("uk-UA") : "—";
  const states={running:"Виконується",completed:"Завершено",completed_with_errors:"З помилками URL",failed:"Помилка запуску",canceled:"Скасовано",abandoned:"Перервано"};
  const comparisons={pending:"Порівняння очікується",baseline:"Початкова точка",compared:"Порівняно",interrupted:"Порівняння пропущено"};
  const kinds={http_error:"Новий HTTP 4xx / 5xx",noindex:"Нова заборона індексації Google",robots_blocked:"Нове блокування robots.txt",request_failed:"Не вдалося перевірити сторінку",canonical_changed:"Canonical змінено",redirect_changed:"Редирект змінено",title_missing:"Зник Title",description_missing:"Зник Description",h1_missing:"Зник H1",text_drop:"Текст скоротився понад 50%",audit_interrupted:"Аудит не завершено"};
  let editID="",requestID="",signature="",renderAvailable=false,items=[],cursorStack=[""],next="",selectedRun="",loading=false,timer,changeGeneration=0;
  const message=(id,text)=>{$(id).textContent=text;$(id).hidden=!text;};
  async function api(path,body,token){const headers={"X-SEO-Auditor-Request":"1"};if(body!==undefined)headers["Content-Type"]="application/json";if(token)headers.Authorization="Bearer "+token;
    const response=await fetch(path,{method:body===undefined?"GET":"POST",headers,credentials:"same-origin",body:body===undefined?undefined:JSON.stringify(body)});
    const data=await response.json();if(!response.ok)throw Error(data.error?.message||"Запит не виконано");return data;}
  const safe=fn=>async(...args)=>{try{message("notice","");await fn(...args);}catch(e){message("notice",e.message);}};
  const icon=(symbol,title,fn)=>{const b=el("button",symbol);b.type="button";b.title=title;b.setAttribute("aria-label",title);b.onclick=safe(async()=>{b.disabled=true;try{await fn();}finally{b.disabled=false;}});return b;};
  function link(text,id){const a=el("a",text);a.href="/audits/"+encodeURIComponent(id);return a;}
  function localTime(at){const d=new Date(at);return new Date(d.getTime()-d.getTimezoneOffset()*60000).toISOString().slice(0,16);}
  function mode(){const site=document.querySelector('input[name="schedule-mode"]:checked').value==="site";
    $("schedule-list").hidden=site;$("schedule-site").hidden=!site;$("schedule-urls").disabled=site;$("schedule-urls").required=!site;
    for(const id of ["schedule-root","schedule-pages","schedule-depth","schedule-sitemaps"])$(id).disabled=!site;
    $("schedule-root").required=site;}
  function openForm(item){$("schedule-form").reset();message("form-error","");editID=item?.id||"";signature="";requestID="";
    for(const input of $("schedule-source").querySelectorAll("input,textarea"))input.disabled=false;
    $("schedule-heading").textContent=item?"Редагувати розклад":"Новий розклад";$("schedule-name").value=item?.name||"";$("interval-hours").value=item?.interval_hours||24;
    $("next-run").value=localTime(Math.max(Date.now()+60000,item?new Date(item.next_run_at).getTime():Date.now()+3600000));
    $("time-label").textContent=(item?"Наступний запуск":"Перший запуск")+" · "+Intl.DateTimeFormat().resolvedOptions().timeZone;
    $("schedule-enabled").checked=item?.enabled??true;$("enabled-label").hidden=!item;$("schedule-source").hidden=Boolean(item);mode();
    if(item)for(const input of $("schedule-source").querySelectorAll("input,textarea"))input.disabled=true;
    else $("schedule-render").disabled=!renderAvailable;
    $("schedule-dialog").showModal();}
  async function save(event){event.preventDefault();message("form-error","");$("save-schedule").disabled=true;
    try{const base={name:$("schedule-name").value.trim(),interval_hours:Number($("interval-hours").value),next_run_at:new Date($("next-run").value).toISOString()};
      if(editID)await api(`/api/schedules/${editID}/update`,{...base,enabled:$("schedule-enabled").checked});
      else{const site=document.querySelector('input[name="schedule-mode"]:checked').value==="site";
        const input={...base,render_javascript:$("schedule-render").checked,mode:site?"site":"list",urls:site?"":$("schedule-urls").value,site:site?{root_url:$("schedule-root").value,max_pages:Number($("schedule-pages").value),max_depth:Number($("schedule-depth").value),use_sitemaps:$("schedule-sitemaps").checked}:null};
        const key=JSON.stringify(input);if(signature!==key){requestID=crypto.randomUUID();signature=key;}await api("/api/schedules",{...input,id:requestID});}
      $("schedule-dialog").close();await refresh();
    }catch(e){message("form-error",e.message);}finally{$("save-schedule").disabled=false;}}
  async function schedules(){const data=await api("/api/schedules");const body=$("schedules").querySelector("tbody");body.replaceChildren();
    for(const item of data){const row=el("tr"),name=el("td");name.append(el("strong",item.name),el("span",item.source_label,"schedule-label"),el("span",(item.mode==="site"?"Обхід сайту · до ":"Список · ")+item.target_count+" URL"+(item.render_javascript?" · JavaScript":""),"schedule-label"));
      if(item.last_error)name.append(el("span",item.last_error,"badge error"));const cadence=el("td","Кожні "+item.interval_hours+" год");cadence.append(el("div",item.enabled?"Активний":"На паузі","badge "+(item.enabled?"success":"")));
      const previous=el("td");if(item.last_run_id){previous.append(link(states[item.last_status]||item.last_status,item.last_run_id),el("span",(comparisons[item.comparison_status]||"")+" · "+item.change_count+" змін","schedule-label"));}else previous.textContent="Ще не запускався";
      const actions=el("td"),buttons=el("div",null,"monitor-actions");buttons.append(icon("▶","Запустити зараз",async()=>{await api(`/api/schedules/${item.id}/run`,{});await refresh();}),icon("✎","Редагувати розклад",()=>openForm(item)),icon(item.enabled?"Ⅱ":"▷",item.enabled?"Поставити на паузу":"Увімкнути розклад",async()=>{await api(`/api/schedules/${item.id}/update`,{name:item.name,enabled:!item.enabled,interval_hours:item.interval_hours,next_run_at:new Date(Math.max(Date.now()+60000,new Date(item.next_run_at).getTime())).toISOString()});await refresh();}),icon("↺","Історія розкладу",()=>history(item)));actions.append(buttons);
      row.append(name,cadence,el("td",item.enabled?dates(item.next_run_at):"—"),previous,actions);body.append(row);}
    if(!data.length){const row=el("tr"),td=el("td","Розкладів ще немає","empty");td.colSpan=5;row.append(td);body.append(row);}}
  async function history(item){const data=await api(`/api/schedules/${item.id}/runs`);$("schedule-history-heading").textContent=item.name;const list=$("schedule-history");list.replaceChildren();
    for(const run of data){const row=el("div",null,"monitor-history-row");row.append(el("time",dates(run.started_at)),link(states[run.status]||run.status,run.id),el("span",comparisons[run.comparison_status]||run.comparison_status,"badge"));
      const b=el("button",run.change_count+" змін");b.onclick=safe(async()=>{selectedRun=run.id;cursorStack=[""];$("selected-run").hidden=false;$("selected-run-label").textContent="Аудит "+run.id.slice(0,8);$("history-dialog").close();await changes();$("changes-title").scrollIntoView({block:"start"});});row.append(b);if(run.baseline_run_id)row.append(link("Попередній аудит",run.baseline_run_id));list.append(row);}
    if(!data.length)list.append(el("p","Запусків ще немає","empty"));$("history-dialog").showModal();}
  async function changes(){const gen=++changeGeneration;const q=new URLSearchParams({before:cursorStack.at(-1),run_id:selectedRun,unread:String($("change-filter").value==="unread")});const data=await api("/api/changes?"+q);if(gen!==changeGeneration)return;
    items=data.items;next=data.next;$("unread-count").textContent=data.unread?data.unread+" непрочитаних":"";const body=$("changes").querySelector("tbody");body.replaceChildren();
    for(const item of items){const row=el("tr",null,item.read?"":"unread"),target=el("td",item.safe_url||"Увесь запуск");target.append(el("span",item.schedule_name,"schedule-label"));const kind=el("td",kinds[item.kind]||item.kind);kind.append(el("div",item.severity==="critical"?"Критична зміна":"Увага","badge "+(item.severity==="critical"?"error":"warning")));
      const time=el("td",dates(item.created_at)),links=el("div",null,"monitor-links");links.append(link("Поточний",item.run_id));if(item.baseline_run_id)links.append(link("Попередній",item.baseline_run_id));time.append(links);
      row.append(target,kind,el("td",item.before_value||"—"),el("td",states[item.after_value]||item.after_value||"—"),time);body.append(row);}
    if(!items.length){const row=el("tr"),td=el("td","Нових змін за цим фільтром немає","empty");td.colSpan=5;row.append(td);body.append(row);}
    $("changes-next").disabled=!next;$("changes-prev").disabled=cursorStack.length===1;$("read-page").disabled=!items.some(i=>!i.read);$("change-count").textContent=items.length+" сповіщень · сторінка "+cursorStack.length;}
  async function refresh(){if(loading)return;loading=true;try{await schedules();await changes();$("updated").textContent="Оновлено "+dates(Date.now());}finally{loading=false;}}
  async function poll(){clearTimeout(timer);try{if(!document.hidden&&!$("schedule-dialog").open&&!$("history-dialog").open)await refresh();}catch(e){message("notice",e.message);}timer=setTimeout(poll,20000);}
  async function init(){const token=new URLSearchParams(location.hash.slice(1)).get("access_token");if(token){window.history.replaceState(null,"",location.pathname);await api("/api/session",{},token);}
    const schema=await api("/api/schema");renderAvailable=schema.rendering_available;
    $("new-schedule").onclick=()=>openForm();$("schedule-form").onsubmit=save;$("refresh").onclick=safe(refresh);
    document.querySelectorAll('[name="schedule-mode"]').forEach(input=>input.onchange=mode);document.querySelectorAll("[data-close]").forEach(b=>b.onclick=()=>$(b.dataset.close).close());
    $("change-filter").onchange=safe(async()=>{cursorStack=[""];await changes();});$("changes-next").onclick=safe(async()=>{cursorStack.push(next);await changes();});$("changes-prev").onclick=safe(async()=>{cursorStack.pop();await changes();});
    $("all-changes").onclick=safe(async()=>{selectedRun="";cursorStack=[""];$("selected-run").hidden=true;await changes();});
    $("read-page").onclick=safe(async()=>{await api("/api/changes/read",{ids:items.filter(i=>!i.read).map(i=>i.id)});await changes();});
    await refresh();timer=setTimeout(poll,20000);}
  init().catch(e=>message("notice",e.message));
})();
