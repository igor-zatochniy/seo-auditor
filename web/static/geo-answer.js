"use strict";
window.seoAnswer = {
  init({api,el,reportID,onSaved}) {
    const $=id=>document.getElementById(id);
    const relations={own:"Мій сайт",competitor:"Конкурент",external:"Інший сайт"};
    let current,preview,generation=0,busy=false,requestID,requestSignature,before=0;
    const input=()=>({engine:$("answer-engine").value,answer:$("answer-text").value,sources:$("answer-sources").value});
    const invalidate=()=>{generation++;preview=null;$("answer-review").hidden=true;$("answer-save").disabled=true;$("answer-confirm").checked=false;};
    for(const id of ["answer-text","answer-sources","answer-engine"])$(id).addEventListener("input",invalidate);
    const history=async(reset=false)=>{
      const item=current,g=++generation;
      if(reset){before=0;$("answer-history").replaceChildren();}
      const data=await api(`/api/geo/reports/${reportID}/queries/${item.ordinal}/answers?before=${before}`);
      if(g!==generation||current!==item)return;
      for(const saved of data.items){const section=el("details"),heading=el("summary",`${new Date(saved.created_at).toLocaleString("uk-UA")} · ${saved.engine}`);section.append(heading,el("p",saved.answer));for(const c of saved.analysis.citations.filter(c=>c.approved))section.append(el("p",`${relations[c.relation]} · ${c.url}`));$("answer-history").append(section);}
      if(reset&&!data.items.length)$("answer-history").append(el("p","Підтверджених відповідей ще немає"));before=data.next;$("answer-history-more").hidden=!before;
    };
    this.selectQuery=item=>{current=item;invalidate();$("answer-text").value="";$("answer-sources").value="";$("answer-status").textContent="";history(true).catch(e=>{$("answer-status").textContent=e.message;});};
    $("answer-form").addEventListener("submit",async e=>{
      e.preventDefault();if(busy||!current)return;const item=current,payload=input(),g=++generation;busy=true;$("answer-preview").disabled=true;
      try{const data=await api(`/api/geo/reports/${reportID}/queries/${item.ordinal}/answer/preview`,payload);if(g!==generation||current!==item)return;preview={payload,ordinal:item.ordinal,data};$("answer-candidates").replaceChildren();for(const c of data.citations){const label=el("label",null,"checkbox-label"),box=el("input");box.type="checkbox";box.value=c.id;box.dataset.citation="1";label.append(box,el("span",`${relations[c.relation]} · ${c.url}`));$("answer-candidates").append(label);}$("answer-brand").checked=data.brand_mention;$("answer-summary").textContent=`Розпізнано джерел: ${data.citations.length}. Згадки конкурентів: ${data.competitor_mentions.join(", ")||"Не виявлено"}. ${data.limited?"Вибірку обмежено; відсутність джерела не доводить відсутність цитування.":""}`;$("answer-review").hidden=false;$("answer-confirm").checked=false;$("answer-save").disabled=true;$("answer-status").textContent="Оберіть лише джерела, які справді процитовано, і підтвердьте.";}
      catch(error){if(current===item)$("answer-status").textContent=error.message;}finally{busy=false;$("answer-preview").disabled=false;}
    });
    $("answer-confirm").addEventListener("change",()=>{$("answer-save").disabled=!$("answer-confirm").checked||!preview||busy;});
    $("answer-save").addEventListener("click",async()=>{
      if(!preview||busy||!$("answer-confirm").checked)return;const item=current,snapshot=preview;
      const payload={...snapshot.payload,confirmed:true,selected:[...$("answer-candidates").querySelectorAll("input:checked")].map(n=>n.value),brand_mention:$("answer-brand").checked};const signature=JSON.stringify([snapshot.ordinal,payload]);if(signature!==requestSignature){requestSignature=signature;requestID=crypto.randomUUID();}
      busy=true;$("answer-save").disabled=true;$("answer-preview").disabled=true;
      try{await api(`/api/geo/reports/${reportID}/queries/${snapshot.ordinal}/answer`,{...payload,id:requestID});if(current===item){invalidate();$("answer-status").textContent="Підтверджений аналіз збережено.";}await onSaved();if(current?.ordinal===snapshot.ordinal)await history(true);}
      catch(error){if(current===item)$("answer-status").textContent=error.message;}finally{busy=false;$("answer-preview").disabled=false;$("answer-save").disabled=!preview||!$("answer-confirm").checked;}
    });
    $("answer-history-more").addEventListener("click",()=>{history().catch(e=>{$("answer-status").textContent=e.message;});});
  },
  selectQuery() {}
};
