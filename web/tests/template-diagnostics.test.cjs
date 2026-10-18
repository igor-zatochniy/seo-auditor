const {test}=require("node:test");
const assert=require("node:assert/strict");
const fs=require("node:fs"),path=require("node:path"),vm=require("node:vm");
const source=fs.readFileSync(path.join(__dirname,"../static/template-diagnostics.js"),"utf8");
class Element {
  constructor(tag="div",text=""){this.tag=tag;this.textContent=text;this.value="";this.children=[];}
  append(...children){this.children.push(...children);}
  replaceChildren(...children){this.children=children;if(this.tag==="select")this.value=children[0]?.value||"";}
  setAttribute(){}
}
function fixture(){
  const nodes=new Map(),events=new Map(),requests=[],tabs=new Element(),origins=["https://a.example.com","https://b.example.com"];
  const $=id=>{if(!nodes.has(id))nodes.set(id,new Element(["types-origin","types-filter","types-edit-value","cdn-origin","cdn-import"].includes(id)?"select":"div"));return nodes.get(id);};
  const fetch=async raw=>{const url=new URL(raw,"http://localhost");requests.push(url);let data;
    if(url.pathname.endsWith("/search-imports"))data={origins};
    else if(url.pathname.endsWith("/page-types"))data={origin:url.searchParams.get("origin"),checked:0,matched:0,summary:{},reviews:{},items:[],rules:[],revision:0};
    else if(url.pathname.endsWith("/cdn-diagnostics")){
      const origin=url.searchParams.get("origin"),id=origin.includes("b.example")?"import-b":"import-a",requested=url.searchParams.get("import_id");
      if(requested&&requested!==id)return{ok:false,json:async()=>({error:{message:"Імпорт належить іншому домену"}})};
      const item={id,format:"fastly",stats:{lines:1,duplicates:0},created_at:"2026-10-18T00:00:00Z"};data={origin,imports:[item],import:item,items:[],more:false};
    }else throw Error(url.pathname);
    return{ok:true,json:async()=>data};
  };
  vm.runInNewContext(source,{location:{pathname:"/audits/11111111-1111-4111-8111-111111111111"},URLSearchParams,fetch,document:{getElementById:$,createElement:tag=>new Element(tag),querySelector:()=>tabs,addEventListener:(name,fn)=>events.set(name,fn)}});
  return{$,events,requests,origins};
}
test("CDN refresh preserves origin and its selected import",async()=>{
  const f=fixture();await f.events.get("seo:cdn-open")();
  f.$("cdn-origin").value="https://b.example.com";await f.$("cdn-origin").onchange();
  assert.equal(f.$("cdn-import").value,"import-b");
  await f.$("cdn-refresh").onclick();
  const last=f.requests.at(-1);
  assert.equal(f.$("cdn-origin").value,"https://b.example.com");assert.equal(last.searchParams.get("origin"),"https://b.example.com");assert.equal(last.searchParams.get("import_id"),"import-b");assert(!f.$("cdn-status").textContent.includes("іншому домену"));
});
test("Page type refresh preserves origin; removed CDN origin resets import",async()=>{
  const f=fixture();await f.events.get("seo:types-open")();
  f.$("types-origin").value="https://b.example.com";await f.$("types-origin").onchange();await f.$("types-refresh").onclick();
  assert.equal(f.requests.at(-1).searchParams.get("origin"),"https://b.example.com");
  await f.events.get("seo:cdn-open")();f.$("cdn-origin").value="https://b.example.com";await f.$("cdn-origin").onchange();
  f.origins.splice(1,1);await f.$("cdn-refresh").onclick();
  assert.equal(f.$("cdn-origin").value,"https://a.example.com");assert.equal(f.requests.at(-1).searchParams.get("import_id"),"");assert.equal(f.$("cdn-import").value,"import-a");
});
