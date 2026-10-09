"use strict";
window.seoVisibility = (() => {
  async function init({api,el,reportID,rows,notice}) {
    const $=id=>document.getElementById(id),base=`/api/geo/reports/${reportID}/visibility/imports`;
    const sources={gsc_web:"GSC · Web (не AI-видимість)",gsc_ai:"GSC · generative AI",ai_observed:"Імпортовані AI-спостереження"};
    const engines={google_ai:"Google AI",chatgpt:"ChatGPT",gemini:"Gemini",perplexity:"Perplexity",other:"Інша система"};
    const scopes={query:"Лише запит",url:"Лише URL",query_url:"Запит + URL"};
    const levels={strong:"Сильні сигнали",medium:"Часткові сигнали",weak:"Слабкі сигнали",unknown:"Недостатньо даних"};
    const number=v=>v==null?"Немає даних":new Intl.NumberFormat("uk-UA").format(v);
    let cursors=[0],page=0,next=0,generation=0,requestID,lastPayload;
    const safe=fn=>async()=>{try{notice("");await fn();}catch(error){notice(error.message);}};
    function clear(){const body=$("visibility-table").querySelector("tbody");body.replaceChildren();const tr=el("tr"),td=el("td","Спостережень ще немає","empty");td.colSpan=7;tr.append(td);body.append(tr);$("visibility-meta").textContent="";$("visibility-scope").textContent="";$("visibility-page-label").textContent="";$("visibility-prev").disabled=true;$("visibility-next").disabled=true;$("visibility-delete").disabled=true;}
    async function load(){
      const id=$("visibility-import-select").value,serial=++generation;
      if(!id){clear();return;}
      const data=await api(`${base}/${id}?after=${cursors[page]}`);if(serial!==generation)return;
      const item=data.import,body=$("visibility-table").querySelector("tbody");body.replaceChildren();next=data.next;
      $("visibility-meta").textContent=`${sources[item.source]}${item.source==="ai_observed"?" · "+engines[item.engine]:""} · ${item.period_start} — ${item.period_end} · Країна: ${item.country||"усі"} · Пристрій: ${{all:"усі",desktop:"комп'ютер",mobile:"мобільний",tablet:"планшет"}[item.device]} · Рядків: ${item.row_count}`;
      $("visibility-scope").textContent=item.source==="gsc_web"?"Це загальний пошук Web, а не окрема AI-видимість. Дані лише запиту не визначають URL; дані лише URL не визначають запит.":item.source==="gsc_ai"?"Спостережені покази generative AI на рівні URL. Запит і AI-кліки з цього експорту не виводяться.":"Зовнішні дані, завантажені користувачем, не підтверджені Auditor через API. Область виміру визначена джерелом, періодом і фільтрами імпорту.";
      for(const record of data.rows){
        const linked=rows.find(row=>row.ordinal===record.report_ordinal),r=linked?.result,tr=el("tr");
        const query=el("td",record.query||"Запит не відомий");query.append(el("small",scopes[record.scope],"muted"));
        const citation=linked?.citations?.filter(c=>c.citation==="yes")||[];
        tr.append(query,el("td",record.url||"URL не відомий"),el("td",r?.matching?r.matching.relevance+" / 100":"Не зіставлено"),el("td",r?levels[r.level]:"Не зіставлено"),el("td",number(record.impressions)),el("td",number(record.clicks)),el("td",citation.length?citation.map(c=>engines[c.engine]).join(", "):linked?.citations?.length?"Не виявлено / не перевірено":"Не перевірено"));
        body.append(tr);
      }
      $("visibility-page-label").textContent=`Сторінка ${page+1} · Один набір даних, без сумування імпортів`;
      $("visibility-prev").disabled=page===0;$("visibility-next").disabled=!next;$("visibility-delete").disabled=false;
    }
    async function history(selected){
      const items=await api(base),select=$("visibility-import-select");select.replaceChildren();
      for(const item of items){const option=el("option",`${sources[item.source]} · ${item.period_start} — ${item.period_end} · ${item.country||"усі країни"}`);option.value=item.id;select.append(option);}
      if(selected&&items.some(i=>i.id===selected))select.value=selected;
      cursors=[0];page=0;await load();
    }
    function sourceChanged(){const source=$("visibility-source").value;$("visibility-engine").disabled=source!=="ai_observed";$("visibility-import-help").textContent=source==="gsc_web"?"Звичайний експорт GSC Web не підтверджує окрему AI-видимість.":source==="gsc_ai"?"Експорт generative AI: Page/URL та Impressions. Query і Clicks залиште порожніми; не додавайте їх зі звичайного Web-звіту.":"Оберіть реальну AI-систему й імпортуйте тільки виміряні дані. Відсутні покази чи кліки залиште порожніми.";}
    $("visibility-source").onchange=sourceChanged;sourceChanged();
    $("visibility-import-open").onclick=()=>{$("visibility-import-status").textContent="";$("visibility-import-dialog").showModal();};
    $("visibility-import-select").onchange=safe(async()=>{cursors=[0];page=0;await load();});
    $("visibility-prev").onclick=safe(async()=>{page--;await load();});$("visibility-next").onclick=safe(async()=>{if(next){cursors[++page]=next;await load();}});
    $("visibility-delete").onclick=safe(async()=>{const id=$("visibility-import-select").value;if(id&&window.confirm("Видалити лише цей імпорт? SEO-результати та ручні спостереження залишаться.")){await api(`${base}/${id}/delete`,{});await history();}});
    $("visibility-import-form").onsubmit=async event=>{
      event.preventDefault();$("visibility-import-save").disabled=true;$("visibility-import-status").textContent="";
      try{
        const file=$("visibility-file").files[0];if(!file||file.size>2*1024*1024)throw new Error("Оберіть CSV-файл до 2 МіБ");
        const input={source:$("visibility-source").value,engine:$("visibility-engine").value,period_start:$("visibility-start").value,period_end:$("visibility-end").value,country:$("visibility-country").value.trim(),device:$("visibility-device").value,csv:await file.text()};
        const signature=JSON.stringify(input);if(signature!==lastPayload){lastPayload=signature;requestID=crypto.randomUUID();}
        const result=await api(base,{...input,id:requestID});$("visibility-import-dialog").close();await history(result.id);
      }catch(error){$("visibility-import-status").textContent=error.message;}finally{$("visibility-import-save").disabled=false;}
    };
    await history();
  }
  return {init};
})();
