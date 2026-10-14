"use strict";
window.seoImpactAnalytics = (() => {
  async function init({api,el,reportID,notice}) {
    const $=id=>document.getElementById(id),base=`/api/geo/reports/${reportID}/impact-analytics`;
    const sources={gsc_web:"GSC Web",gsc_ai:"GSC generative AI",ga4:"GA4"};
    const urlMatches={exact:"Точний",canonical_match:"Canonical",ambiguous:"Неоднозначний",unmatched:"Не зіставлено",unknown:"Не перевірено"};
    const confidence={high:"висока",medium:"середня",low:"низька",unknown:"невідомо"};
    const number=value=>value==null?"—":new Intl.NumberFormat("uk-UA",{maximumFractionDigits:1}).format(value);
    const percent=value=>value==null?"—":new Intl.NumberFormat("uk-UA",{style:"percent",maximumFractionDigits:2}).format(value);
    let cursors=[0],page=0,next=0,generation=0,selected={};
    const safe=fn=>async()=>{try{notice("");await fn();}catch(error){notice(error.message);}};
    function renderRows(items){
      const body=$("impact-analytics-table").querySelector("tbody");body.replaceChildren();
      for(const item of items){
        const row=el("tr"),url=el("td",item.url||"URL недоступний");
        if(item.title)url.append(el("small",item.title,"muted"));
        const state=[];
        state.push(item.http_status==null?"HTTP невідомий":"HTTP "+item.http_status);
        state.push(item.scan_status||"стан невідомий");
        if(item.self_canonical===true)state.push("self-canonical");
        else if(item.self_canonical===false)state.push("canonical інший");
        if(item.meta_robots)state.push("robots: "+item.meta_robots);
        state.push(item.custom_rule_failures==null?"власні правила не запускалися":"порушень правил: "+item.custom_rule_failures);
        const reserve=item.opportunity_clicks_estimate==null?"Немає baseline":`${number(item.opportunity_clicks_estimate)} · впевненість ${confidence[item.opportunity_confidence]||"невідомо"}`;
        const reserveCell=el("td",reserve);if(item.opportunity_note)reserveCell.title=item.opportunity_note;
        row.append(url,el("td",state.join(" · ")),el("td",number(item.gsc_impressions)),el("td",number(item.gsc_clicks)),el("td",percent(item.gsc_ctr)),el("td",number(item.gsc_position)),reserveCell,el("td",number(item.gsc_ai_impressions)),el("td",number(item.ai_referral_sessions)),el("td",number(item.verified_bot_visits)),el("td",urlMatches[item.url_match]||"Немає зіставлених рядків"));
        body.append(row);
      }
      if(!items.length){const row=el("tr"),cell=el("td","У цьому діапазоні сторінок немає даних","empty");cell.colSpan=11;row.append(cell);body.append(row);}
    }
    function updateSelects(data){
      for(const source of Object.keys(sources)){
        const select=$("impact-"+source.replace("_","-")),prior=selected[source],items=data.datasets.filter(item=>item.source===source);select.replaceChildren();
        if(!items.length){const empty=el("option","Немає імпортованих даних");empty.value="";empty.disabled=true;select.append(empty);}
        for(const item of items){const option=el("option",`${item.period_start} — ${item.period_end} · ${item.country||"усі країни"} · ${{all:"усі пристрої",desktop:"комп'ютер",mobile:"мобільний",tablet:"планшет"}[item.device]} · ${item.row_count} рядків`);option.value=item.id;select.append(option);}
        const chosen=data.selected[source]?.id||prior;
        select.value=items.some(item=>item.id===chosen)?chosen:"";
        selected[source]=select.value;
      }
    }
    async function load(reset){
      if(reset){cursors=[0];page=0;next=0;}
      const cursor=cursors[page],serial=++generation,params=new URLSearchParams({limit:"50",after:String(cursor)});
      for(const source of Object.keys(sources))if(selected[source])params.set(source,selected[source]);
      const data=await api(`${base}?${params}`);if(serial!==generation)return;
      updateSelects(data);renderRows(data.items);
      const labels=[];
      for(const source of Object.keys(sources))if(data.selected[source])labels.push(`${sources[source]}: ${data.selected[source].period_start} — ${data.selected[source].period_end}`);
      if(data.baseline)labels.push(`CTR baseline: ${data.baseline.period_start} — ${data.baseline.period_end}`);
      if(data.bot_import_id)labels.push("журнал ботів: останній імпорт");
      $("impact-analytics-meta").textContent=labels.length?labels.join(" · "):"Імпортуйте CSV GSC або GA4 для відображення спостережених метрик.";
      const warnings=$("impact-analytics-warnings");warnings.replaceChildren();for(const warning of data.warnings||[])warnings.append(el("li",warning));
      next=data.next;$("impact-analytics-page").textContent=`Сторінка ${page+1} · URL: ${data.items.length} · наступна: ${next?"є":"немає"}`;
      $("impact-analytics-prev").disabled=page===0;$("impact-analytics-next").disabled=!next;
    }
    for(const source of Object.keys(sources))$("impact-"+source.replace("_","-")).onchange=safe(async()=>{selected[source]=$("impact-"+source.replace("_","-")).value;await load(true);});
    $("impact-analytics-next").onclick=safe(async()=>{if(next){cursors[page+1]=next;page++;await load(false);}});
    $("impact-analytics-prev").onclick=safe(async()=>{if(page>0){page--;await load(false);}});
    window.addEventListener("geo:visibility-changed",safe(async()=>load(true)));
    await load(true);
  }
  return {init};
})();
