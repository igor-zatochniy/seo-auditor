"use strict";
window.seoAnswer = {
  init({api,el,reportID,onSaved}) {
    const $=id=>document.getElementById(id);
    const relations={own:"Мій сайт",competitor:"Конкурент",external:"Інший сайт"};
    let current,preview,generation=0,busy=false,requestID,requestSignature,before=0;
    const input=()=>({engine:$("answer-engine").value,answer:$("answer-text").value,sources:$("answer-sources").value,sources_complete:$("answer-sources-complete").checked});
    const invalidate=()=>{generation++;preview=null;$("answer-review").hidden=true;$("answer-save").disabled=true;$("answer-confirm").checked=false;};
    for(const id of ["answer-text","answer-sources","answer-engine"])$(id).addEventListener("input",invalidate);
    $("answer-sources-complete").addEventListener("change",invalidate);
    const loadIntelligence=async()=>{
      const data=await api(`/api/geo/reports/${reportID}/citation-intelligence`),s=data.overall,node=$("citation-intelligence-summary");node.replaceChildren();
      const mentionRate=s.brand_mention_known?` · ${Math.round(s.brand_mention_rate*100)}%`:" · немає відомих даних";
      const citationRate=s.own_citation_known?` · ${Math.round(s.own_citation_rate*100)}%`:" · немає відомих даних";
      const items=[["Перевірені відповіді",s.reviewed_answers],["Згадка бренду",`${s.brand_mention_yes} / ${s.brand_mention_known}${mentionRate}`],["Цитування власного домену",`${s.own_citation_yes} / ${s.own_citation_known}${citationRate}`],["Відповіді з цитатами конкурентів",s.competitor_cited],["Бренд згадано без власного джерела",s.brand_mentioned_not_cited],["Конкурент процитований без власного джерела",s.competitor_without_own_citation],["Неоднозначні назви бренду",s.ambiguous_brand_matches],["Неповні / непідтверджені джерела",s.incomplete_sources],["Спостережувані прогалини",s.actionable_gaps]];
      for(const [label,value] of items){const item=el("div",null,"summary-item");item.append(el("span",label,"summary-label"),el("strong",value,"summary-value"));node.append(item);}
      $("citation-intelligence-scope").textContent=data.disclaimer;
    };
    $("citation-intelligence-refresh").addEventListener("click",()=>loadIntelligence().catch(e=>{$("citation-intelligence-scope").textContent=e.message;}));
    loadIntelligence().catch(e=>{$("citation-intelligence-scope").textContent=e.message;});
    const history=async(reset=false)=>{
      const item=current,g=++generation;
      if(reset){before=0;$("answer-history").replaceChildren();}
      const data=await api(`/api/geo/reports/${reportID}/queries/${item.ordinal}/answers?before=${before}`);
      if(g!==generation||current!==item)return;
      for(const saved of data.items){const section=el("details"),heading=el("summary",`${new Date(saved.created_at).toLocaleString("uk-UA")} · ${saved.engine}`),analysis=saved.analysis||{},entity=analysis.brand_entity||{},states={matched:"збіг підтверджено",ambiguous:"неоднозначний збіг",not_found:"автоматичного збігу немає"},coverage={reviewed_complete:"повний список підтверджено",provided_unreviewed:"список надано, повноту не підтверджено",limited:"аналіз обмежено",unknown:"не перевірено"};section.append(heading,el("p",saved.answer),el("p",`Згадка бренду після ручної перевірки: ${analysis.brand_mention?"так":"ні"} · сутність: ${states[entity.status]||"не перевірено"}${entity.matched_aliases?.length?" · "+entity.matched_aliases.join(", "):""}${entity.ambiguous_aliases?.length?" · неоднозначно: "+entity.ambiguous_aliases.join(", "):""}`),el("p",`Повнота джерел: ${coverage[analysis.source_coverage]||"не перевірено"}`));if(entity.same_as_referenced)section.append(el("p","У відповіді є точне посилання sameAs; це не означає цитування власного домену."));for(const c of analysis.citations||[])if(c.approved)section.append(el("p",`${relations[c.relation]||"Джерело"} · ${c.url}`));$("answer-history").append(section);}
      if(reset&&!data.items.length)$("answer-history").append(el("p","Підтверджених відповідей ще немає"));before=data.next;$("answer-history-more").hidden=!before;
    };
    this.selectQuery=item=>{current=item;invalidate();$("answer-text").value="";$("answer-sources").value="";$("answer-sources-complete").checked=false;$("answer-status").textContent="";history(true).catch(e=>{$("answer-status").textContent=e.message;});};
    $("answer-form").addEventListener("submit",async e=>{
      e.preventDefault();if(busy||!current)return;const item=current,payload=input(),g=++generation;busy=true;$("answer-preview").disabled=true;
      try{
        const data=await api(`/api/geo/reports/${reportID}/queries/${item.ordinal}/answer/preview`,payload);if(g!==generation||current!==item)return;preview={payload,ordinal:item.ordinal,data};$("answer-candidates").replaceChildren();
        for(const c of data.citations){const label=el("label",null,"checkbox-label"),box=el("input");box.type="checkbox";box.value=c.id;box.dataset.citation="1";label.append(box,el("span",`${relations[c.relation]} · ${c.url}`));$("answer-candidates").append(label);}
        $("answer-brand").checked=data.brand_mention;
        const ambiguous=data.brand_entity?.ambiguous_aliases?.length?` Неоднозначний збіг: ${data.brand_entity.ambiguous_aliases.join(", ")}; підтвердьте згадку вручну.`:"";
        const matched=data.brand_entity?.matched_aliases?.length?` Збіг бренду (${data.brand_entity.entity_type}): ${data.brand_entity.matched_aliases.join(", ")}.`:" Бренд автоматично не підтверджено.";
        const sameAs=data.brand_entity?.same_as_referenced?" У відповіді є точне посилання sameAs; воно не вважається цитуванням власного домену.":"";
        const coverage=payload.sources_complete&&!data.limited&&payload.sources.trim()?"Повноту списку буде збережено як підтверджену.":"Цитування без повного списку джерел залишиться невідомим.";
        $("answer-summary").textContent=`Знайдено URL: ${data.citations.length}.${matched}${sameAs} Згадки конкурентів: ${data.competitor_mentions.join(", ")||"не виявлено"}.${ambiguous} ${coverage} ${data.limited?"Вибірку обмежено.":""}`;
        $("answer-review").hidden=false;$("answer-confirm").checked=false;$("answer-save").disabled=true;$("answer-status").textContent="Перевірте розпізнані джерела та підтвердьте спостереження.";
      } catch(error){if(current===item)$("answer-status").textContent=error.message;}finally{busy=false;$("answer-preview").disabled=false;}
    });
    $("answer-confirm").addEventListener("change",()=>{$("answer-save").disabled=!$("answer-confirm").checked||!preview||busy;});
    $("answer-save").addEventListener("click",async()=>{
      if(!preview||busy||!$("answer-confirm").checked)return;const item=current,snapshot=preview;
      const payload={...snapshot.payload,confirmed:true,selected:[...$("answer-candidates").querySelectorAll("input:checked")].map(n=>n.value),brand_mention:$("answer-brand").checked};const signature=JSON.stringify([snapshot.ordinal,payload]);if(signature!==requestSignature){requestSignature=signature;requestID=crypto.randomUUID();}
      busy=true;$("answer-save").disabled=true;$("answer-preview").disabled=true;
      try{await api(`/api/geo/reports/${reportID}/queries/${snapshot.ordinal}/answer`,{...payload,id:requestID});if(current===item){invalidate();$("answer-status").textContent="Підтверджений аналіз збережено.";}await Promise.all([onSaved(),loadIntelligence()]);if(current?.ordinal===snapshot.ordinal)await history(true);}
      catch(error){if(current===item)$("answer-status").textContent=error.message;}finally{busy=false;$("answer-preview").disabled=false;$("answer-save").disabled=!preview||!$("answer-confirm").checked;}
    });
    $("answer-history-more").addEventListener("click",()=>{history().catch(e=>{$("answer-status").textContent=e.message;});});
  },
  selectQuery() {}
};
