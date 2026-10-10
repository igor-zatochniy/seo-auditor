"use strict";
window.seoOpportunities = {
  async init({api, el, reportID, rows, notice, refreshRows}) {
    if (!reportID) return;
    const $ = id => document.getElementById(id);
    const safe = fn => async (...args) => { try { await fn(...args); } catch (error) { notice(error.message); } };
    const priority = {high:"Високий",medium:"Середній",low:"Низький"};
    const confidence = {high:"Висока",medium:"Середня",low:"Низька",unknown:"Недостатньо даних"};
    const kinds = {technical:"Технічний доступ",content:"Контент",mapping:"Вибір цілі",entity:"Сутності",internal_link:"Перелінковка",competitor_gap:"Відмінність конкурента",intent_review:"Перевірка наміру"};
    let data, taskPage=0, impactPage=0, impact=[], generation=0, chartGeneration=0, historyGeneration=0, impactGeneration=0, decisionID, decisionSignature;
    const empty=(table,text,count)=>{const body=$(table).querySelector("tbody");body.replaceChildren();const cell=el("td",text,"empty");cell.colSpan=count;const tr=el("tr");tr.append(cell);body.append(tr);};
    const paginate=(prefix,page,total)=>{ $(prefix+"-page").textContent=total ? `${page*50+1}–${Math.min((page+1)*50,total)} / ${total}` : "0";$(prefix+"-prev").disabled=page===0;$(prefix+"-next").disabled=(page+1)*50>=total; };
    function taskTable() {
      if(!data) return;
      const search=$("opportunity-search").value.toLocaleLowerCase("uk"), selected=$("opportunity-priority").value;
      const tasks=data.plan.tasks.filter(t=>(selected==="all"||t.priority===selected)&&[t.action,t.target_url,...t.clusters,...t.queries].join(" ").toLocaleLowerCase("uk").includes(search));
      taskPage=Math.max(0,Math.min(taskPage,Math.ceil(tasks.length/50)-1));const body=$("opportunity-table").querySelector("tbody");body.replaceChildren();
      for(const t of tasks.slice(taskPage*50,(taskPage+1)*50)) {
        const tr=el("tr"),p=el("td",priority[t.priority],"opportunity-"+t.priority), target=el("td"),action=el("td"),conf=el("td"),proof=el("td");
        target.append(el("div",t.clusters.join(" · ")||"Окрема сторінка"),el("small",t.target_url||"Ціль ще не обрана"));
        action.append(el("small",kinds[t.kind]),el("p",t.action));conf.append(el("strong",confidence[t.confidence.level]),el("small",t.confidence.reason));
        for(const e of t.evidence){const block=el("div",null,"opportunity-proof");block.append(el("strong",e.source.startsWith("competitor:")?"Звіт конкурента":e.source==="graph"?"Граф обходу":e.source==="seo"?"SEO-аудит":e.source==="mapping"?"Зіставлення":e.source==="entity"?"Сутності":"Контент"),el("div",e.url),el("div",e.detail));proof.append(block);}
        tr.append(p,target,action,conf,proof);body.append(tr);
      }
      if(!tasks.length) empty("opportunity-table","Для вибраного фільтра завдань немає",5);paginate("opportunity",taskPage,tasks.length);
    }
    async function loadPlan() {
      const current=++generation;$("opportunity-refresh").disabled=true;
      try {const loaded=await api(`/api/geo/reports/${reportID}/opportunities`);if(current!==generation)return;data=loaded;
        $("opportunity-meta").textContent=`${data.plan.model} · ${data.matching_model} · ${new Date(data.generated_at).toLocaleString("uk-UA")}`;
        $("opportunity-warnings").textContent=[...data.plan.warnings,data.plan.limited?"Показано не більше 500 завдань; частину рекомендацій обмежено.":""].filter(Boolean).join(" ");
        $("opportunity-counts").textContent=`Завдань: ${data.plan.tasks.length} · Високий: ${data.plan.tasks.filter(t=>t.priority==="high").length} · Посилань: ${data.plan.links.length}`;taskTable();
        const body=$("opportunity-links").querySelector("tbody");body.replaceChildren();
        for(const l of data.plan.links){const tr=el("tr");for(const v of [l.source_url,l.target_url,l.suggested_anchor,l.observed_inlinks,`${confidence[l.confidence.level]} · ${l.confidence.reason} · ${l.reason}`])tr.append(el("td",v));body.append(tr);}
        if(!data.plan.links.length)empty("opportunity-links","Недостатньо даних графа або відповідних відсутніх зв'язків",5);
      } finally {if(current===generation)$("opportunity-refresh").disabled=false;}
    }
    $("opportunity-export").href=`/api/geo/reports/${reportID}/tasks.csv`;
    $("opportunity-refresh").onclick=safe(loadPlan);
    $("opportunity-search").oninput=()=>{taskPage=0;taskTable();};$("opportunity-priority").onchange=()=>{taskPage=0;taskTable();};
    $("opportunity-prev").onclick=()=>{taskPage--;taskTable();};$("opportunity-next").onclick=()=>{taskPage++;taskTable();};
    async function targetQuery() {
      const selected=data.rows.find(r=>r.ordinal===Number($("target-edit-query").value));if(!selected)return;
      $("target-edit-original").textContent=`Автоматична ціль: ${(selected.automatic_result||selected.result).target_url||"Не знайдено"} · ${selected.mapping_mode==="manual"?"Зараз застосовано ручне рішення":"Автоматичний вибір"}`;
      $("target-edit-page").value=selected.mapping_mode==="manual"?String(selected.result.target_id):"";$("target-edit-reason").value="";$("target-edit-status").textContent="";
      const g=++historyGeneration,items=await api(`/api/geo/reports/${reportID}/queries/${selected.ordinal}/target-history`);if(g!==historyGeneration)return;
      $("target-edit-history").replaceChildren();for(const item of items){const block=el("div",null,"geo-check");block.append(el("strong",`${new Date(item.at).toLocaleString("uk-UA")} · Ревізія ${item.revision}`),el("p",item.url||"Повернення до автоматичного вибору"),el("p",item.reason));$("target-edit-history").append(block);}if(!items.length)$("target-edit-history").append(el("p","Ручних рішень ще немає"));
    }
    $("target-edit-open").onclick=safe(async()=>{
      if(!data)await loadPlan();$("target-edit-query").replaceChildren();$("target-edit-page").replaceChildren();const automatic=el("option","Автоматичний вибір (скасувати ручне рішення)");automatic.value="";$("target-edit-page").append(automatic);
      for(const row of data.rows){const o=el("option",row.result.query);o.value=row.ordinal;$("target-edit-query").append(o);}
      for(const p of data.targets.filter(p=>p.http_status===200&&p.scan_status==="completed")){const o=el("option",`Ціль №${p.target_id} · ${p.url} · ${p.title||"Без title"}`);o.value=p.target_id;$("target-edit-page").append(o);}await targetQuery();$("geo-target-edit").showModal();
    });
    $("target-edit-query").onchange=safe(targetQuery);
    $("target-edit-form").onsubmit=async event=>{
      event.preventDefault();const row=data.rows.find(r=>r.ordinal===Number($("target-edit-query").value));if(!row)return;
      const payload={target_id:$("target-edit-page").value?Number($("target-edit-page").value):null,expected_revision:row.decision_revision||0,reason:$("target-edit-reason").value};const signature=JSON.stringify([row.ordinal,payload]);if(signature!==decisionSignature){decisionID=crypto.randomUUID();decisionSignature=signature;}
      $("target-edit-save").disabled=true;$("target-edit-status").textContent="Збереження…";
      try{await api(`/api/geo/reports/${reportID}/queries/${row.ordinal}/target`,{...payload,id:decisionID});invalidateImpact();invalidateSeries();$("geo-target-edit").close();await refreshRows();await loadPlan();window.dispatchEvent(new Event("geo:targets-changed"));await series();}catch(error){$("target-edit-status").textContent=error.message;if(!$("geo-target-edit").open)notice(error.message);}finally{$("target-edit-save").disabled=false;}
    };
    function impactTable() {
      impactPage=Math.max(0,Math.min(impactPage,Math.ceil(impact.length/50)-1));const body=$("impact-table").querySelector("tbody");body.replaceChildren();
      for(const c of impact.slice(impactPage*50,(impactPage+1)*50)){const tr=el("tr");for(const v of [c.query,c.before_url||"Без цілі",c.after_url||"Без цілі",c.relevance_delta==null?"Не порівнюється":(c.relevance_delta>0?"+":"")+c.relevance_delta,c.reason])tr.append(el("td",v));body.append(tr);}if(!impact.length)empty("impact-table","Оберіть попередній звіт цього домену",5);paginate("impact",impactPage,impact.length);
    }
    $("impact-prev").onclick=()=>{impactPage--;impactTable();};$("impact-next").onclick=()=>{impactPage++;impactTable();};
    function invalidateImpact() {impactGeneration++;impact=[];impactPage=0;impactTable();$("impact-meta").textContent="";}
    $("impact-baseline").onchange=invalidateImpact;
    $("impact-compare").onclick=safe(async()=>{const baseline=$("impact-baseline").value;if(!baseline)return;const g=++impactGeneration;const result=await api(`/api/geo/reports/${reportID}/impact?baseline=${encodeURIComponent(baseline)}`);if(g!==impactGeneration)return;impact=result.items;impactPage=0;impactTable();$("impact-meta").textContent=`${result.baseline.model} → ${result.current.model} · ${result.same_model?"Одна версія моделі":"Різні версії — числове порівняння вимкнено"}. Спостереження не доводять причинного ефекту рекомендацій.`;});
    function chart(points) {
      const canvas=$("impact-chart"),ctx=canvas.getContext("2d");if(!ctx)return;const w=canvas.width,h=canvas.height;ctx.clearRect(0,0,w,h);ctx.font="14px sans-serif";ctx.fillStyle="#52717b";
      const values=points.filter(p=>p.impressions!=null);if(!values.length){ctx.fillText("Немає числових AI-спостережень для вибраної області",20,40);return;}
      const maximum=Math.max(1,...values.map(p=>p.impressions)),step=(w-60)/Math.max(points.length,1);ctx.fillText(`${maximum.toLocaleString("uk-UA")} показів`,10,20);
      points.forEach((p,i)=>{const x=45+i*step;if(p.impressions==null){ctx.fillStyle="#84959b";ctx.fillText("?",x,h-25);return;}const height=Math.max(2,(h-60)*p.impressions/maximum);ctx.fillStyle=p.overlaps_previous?"#b17d2d":"#007d88";ctx.fillRect(x,h-30-height,Math.max(2,step-6),height);});
    }
    function invalidateSeries() {
      chartGeneration++;chart([]);empty("impact-series-table","Оновіть графік для поточної цілі та вибраної області",4);$("impact-series-status").textContent="Дані графіка потребують оновлення.";return chartGeneration;
    }
    async function series() {
      const g=invalidateSeries(),params=new URLSearchParams({ordinal:$("impact-query").value,source:$("impact-source").value,engine:$("impact-engine").value,country:$("impact-country").value.trim().toUpperCase(),device:$("impact-device").value,scope:$("impact-scope").value});
      $("impact-series-status").textContent="Завантаження AI-спостережень…";
      const result=await api(`/api/geo/reports/${reportID}/visibility-series?${params}`);if(g!==chartGeneration)return;chart(result.points);$("impact-series-status").textContent=`${result.query} · ${result.scope==="url"?"Лише обраний URL, без атрибуції запиту":"Дані запиту"} · Періодів: ${result.points.length} · Значення не підсумовуються`;const body=$("impact-series-table").querySelector("tbody");body.replaceChildren();
      for(const p of result.points){const tr=el("tr");for(const v of [`${p.period_start}–${p.period_end}`,p.impressions==null?"Невідомо":p.impressions,p.clicks==null?"Невідомо":p.clicks,p.overlaps_previous?"Період перекривається з попереднім":p.matched_rows?"Лише імпортовані рядки":"Для вибраного рівня немає зіставленого рядка"])tr.append(el("td",v));body.append(tr);}if(!result.points.length)empty("impact-series-table","AI-імпортів цієї області немає",4);
    }
    $("impact-series-refresh").onclick=safe(series);$("impact-source").onchange=()=>{const google=$("impact-source").value==="gsc_ai";$("impact-engine").disabled=google;if(google)$("impact-engine").value="google_ai";invalidateSeries();};$("impact-source").onchange();
    for(const id of ["impact-query","impact-engine","impact-device","impact-scope"])$(id).onchange=invalidateSeries;
    $("impact-country").oninput=invalidateSeries;
    window.addEventListener("geo:visibility-changed",safe(series));
    await loadPlan();const history=await api("/api/geo/reports");$("impact-baseline").replaceChildren();for(const r of history.filter(r=>r.id!==reportID&&r.domain===data.report.domain&&new Date(r.created_at)<=new Date(data.report.created_at))){const o=el("option",`${new Date(r.created_at).toLocaleString("uk-UA")} · ${r.model}`);o.value=r.id;$("impact-baseline").append(o);}$("impact-compare").disabled=!$("impact-baseline").options.length;
    $("impact-query").replaceChildren();for(const r of rows){const o=el("option",r.result.query);o.value=r.ordinal;$("impact-query").append(o);}impactTable();await series();
  }
};
