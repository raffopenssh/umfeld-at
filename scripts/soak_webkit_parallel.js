// scripts/soak_webkit_parallel.js — run the glyph soak in WebKit, one page per KG, in parallel (docs/ne-glyph.md §Soak, "Faster runs").
// needs: a dir with `npm i playwright` + `npx playwright install webkit`; run from there: node <this file> [kg1,kg2]
// parallel soak: one page per KG (default: the two KGs a seed-7 run picks), same browser; ~half the wall time of run.js
const {webkit}=require('playwright');
const kgs=(process.argv[2]||'83018,44114').split(',');
(async()=>{const b=await webkit.launch();const t0=Date.now();
const outs=await Promise.all(kgs.map(async(kg)=>{const ctx=await b.newContext({viewport:{width:1400,height:900}});const p=await ctx.newPage();const errs=[];p.on('pageerror',e=>errs.push(e.message));
 await p.goto('http://localhost:8000/',{waitUntil:'load'});
 const out=await p.evaluate(async(kg)=>{const m=await import('/api/v1/ne/glyph/soak.js?'+Date.now());return await m.run({kgs:[kg],spots:1,seed:7})},kg);return (errs.length?'PAGEERR '+errs.join(' | ')+'\n':'')+out}));
console.log(outs.join('\n\n'));console.log('wall',((Date.now()-t0)/1000).toFixed(0)+'s');await b.close()})();
