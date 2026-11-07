package main

// neGlyphDbgJS — the `?dbg` WHERE panel (docs/ne-glyph.md §dbg where): a fixed
// pre in the top-right corner that names the spot under the pointer so an
// operator can hand it to the observed-layer peer without a second tool —
// lon/lat, the H3 ancestry res 7…12, the res-7 frame index and its overview
// row, the LOD tile keys (+ the obs tile URL), the fidx bin, the view bbox,
// a share link, the peer's landscape / KG URLs for the cell, our cell / stats
// URLs, and the raw observed row(s) decoded field by field from
// /ne/glyph/cell?dbg=1. Hover follows the pointer (120 ms debounce); a click
// pins; Escape unpins; a click on the panel copies it. Only with ?dbg.
const neGlyphDbgJS = `
if(DBG)(function(){var el=document.createElement('pre');el.className='glyph-dbgw';el.style.cssText='position:fixed;right:0;top:0;z-index:99999;max-width:min(100vw,46vw);max-height:70vh;overflow:auto;margin:0;padding:6px 8px;font:10px/1.3 monospace;background:rgba(0,0,0,.86);color:#ffd;white-space:pre-wrap;word-break:break-all;user-select:text;cursor:copy;box-shadow:0 0 0 1px #663';el.title='WHERE \u00b7 click = copy \u00b7 click the map = pin \u00b7 Esc = unpin';document.body.appendChild(el);
var pinP=null,T=0,A=null,srv=null,srvK='',txt='';
function f6(v){return(+v).toFixed(6)}function bb(a){return a?a.map(function(v){return(+v).toFixed(6)}).join(','):'-'}
function tileKey(lod,ll){var L=LOD[lod][0];return lod+'/'+Math.floor(ll[0]/L)+'/'+Math.floor(ll[1]/L)}
function obsLayer(){return view==='obs'?'obs':''}
function lines(p,pinned){var ll=lonlat(p[0],p[1]),z=W/vb.w,a=lonlat(vb.x,vb.y+vb.h),b=lonlat(vb.x+vb.w,vb.y),o=[],j=near(p[0],p[1]),c=j>=0?cells[j]:null,lod=hiLOD(),tk=[0,1,2].map(function(l){return tileKey(l,ll)}),origin=location.origin,ts=new Date().toISOString();
 o.push('WHERE '+(pinned?'[PINNED \u00b7 Esc unpins]':'[hover \u00b7 click pins]')+'  '+ts);
 o.push('lon,lat  '+f6(ll[0])+','+f6(ll[1])+'   lat,lon '+f6(ll[1])+','+f6(ll[0])+'   xy '+p[0].toFixed(1)+','+p[1].toFixed(1));
 o.push('view  z='+z.toFixed(1)+' lod='+(hi.on?lod:'-')+' profile='+(view||'declared')+' full='+hi.on+' bbox='+[a[0],a[1],b[0],b[1]].map(function(v){return(+v).toFixed(5)}).join(','));
 o.push('link  '+origin+location.pathname+'#pt='+ll[0].toFixed(5)+','+ll[1].toFixed(5)+';v=obs;full;z='+Math.round(Math.max(z,HI_LOD1)));
 o.push('hash  '+location.hash);
 if(c){var code=codeOf(c),nm=typeof c[8]==='string'?c[8]:(data&&data.kgs&&c[8]>=0?data.kgs[c[8]]:'');o.push('r7 frame #'+j+'  xy '+c[0]+','+c[1]+'  chunks='+(c[2]||0)+' passes='+(c[3]||0)+' reports='+(c[4]||0)+' age_h='+(c[5]==null?'-':c[5])+' observed='+(c[6]?1:0)+' chg='+(c[7]||0)+' cand='+(c[10]||0)+'  kg='+(code||'-')+(nm?' '+nm:''));o.push('r7 peer bbox  {OBS_WEB}/api/v1/landscape?include=cells&bbox='+bboxOf(c))}
 o.push('tiles  '+tk.join('  ')+'   obs tile: '+origin+'/api/v1/ne/glyph/tile?lod='+(hi.on?lod:1)+'&i='+tk[hi.on?lod:1].split('/')[1]+'&j='+tk[hi.on?lod:1].split('/')[2]+'&v=obs   fidx bin: '+tk[0]);
 var d=hi.on?hiFind(p):null;if(d)o.push('doc under pointer  '+d.cell+(d.pic_obs?'  pic_obs='+JSON.stringify(d.pic_obs):''));
 var s=srv&&srv.dbg;if(s&&srvK===f6(ll[0])+','+f6(ll[1])){o.push('r12  '+s.cell+'  centre '+f6(s.lon)+','+f6(s.lat)+'  bbox12 '+bb(s.bbox12));var ps=s.parents||{};o.push('h3   r11 '+(ps['11']||'-')+'  r10 '+(ps['10']||'-')+'  r9 '+(ps['9']||'-')+'  r8 '+(ps['8']||'-')+'  r7 '+(ps['7']||'-')+'  chunk9 '+s.chunk9);if(s.r7)o.push('r7   '+s.r7.cell+' frame#'+s.r7.index+' centre '+f6(s.r7.lon||0)+','+f6(s.r7.lat||0)+' bbox7 '+bb(s.r7.bbox));var kgs=srv.kgs||{},kl=[];for(var k in kgs)kl.push(k+':'+kgs[k]);o.push('kgs  '+(kl.join(' ')||'-')+(srv.held===false?'  (no declared row held'+(srv.pending_kgs?' \u00b7 pending '+srv.pending_kgs.join(','):'')+')':''));
  o.push('peer cell  {OBS_WEB}/api/v1/landscape?include=cells&bbox='+bb(s.bbox12));for(var k2 in kgs)o.push('peer kg    {OBS_WEB}/api/v1/kg/'+k2+'/cells?format=json');
  o.push('ours  '+origin+'/api/v1/ne/glyph/cell?cell='+s.cell+'&dbg=1   '+origin+'/api/v1/ne/cell/'+s.cell+'   '+origin+'/api/v1/ne/stats/unit?pt='+ll[0].toFixed(5)+','+ll[1].toFixed(5));
  if(srv.pic_obs)o.push('pic_obs  '+JSON.stringify(srv.pic_obs));if(srv.pic_dif)o.push('pic_dif  '+JSON.stringify(srv.pic_dif));
  (s.obs||[]).forEach(function(r){o.push('OBS row kg='+r.kg+' source='+(r.source||'-')+' epoch='+(r.epoch||'-')+' algo='+(r.algo||'-')+' built_at='+(r.built_at||'-')+' digest='+(r.digest||'-')+' rows='+(r.rows||'-')+' record='+(r.record||'-'));if(r.row){var ks=Object.keys(r.row),rr=[];ks.forEach(function(k){rr.push(k+'='+JSON.stringify(r.row[k]))});o.push('  '+rr.join(' '))}});if(!(s.obs||[]).length)o.push('OBS row  none held for this cell');
  if(s.lu&&s.lu.length)o.push('lu rows  '+s.lu.map(function(r){return r.kg+':'+r.share}).join(' '))}
 else o.push('r12 \u2026 '+(srv===null&&srvK?'loading':'')); 
 return o.join('\n')}
function paint(p,pinned){txt=lines(p,pinned);el.textContent=txt}
function ask(p,pinned){var ll=lonlat(p[0],p[1]),k=f6(ll[0])+','+f6(ll[1]);if(srvK===k&&srv)return;srvK=k;srv=null;if(A)A.abort();var a=A=new AbortController();fetch('/api/v1/ne/glyph/cell?dbg=1&lon='+ll[0].toFixed(6)+'&lat='+ll[1].toFixed(6)+(view==='obs'?'':''),{credentials:'omit',signal:a.signal}).then(function(r){return r.json()}).then(function(dd){if(a!==A)return;srv=dd||{};paint(p,pinned);dbgLog('where '+k+' r12='+(dd&&dd.cell))}).catch(function(e){if(e&&e.name==='AbortError')return;srv={err:String(e)};paint(p,pinned)})}
function where(p,pinned){paint(p,pinned);clearTimeout(T);T=setTimeout(function(){ask(p,pinned)},pinned?0:220)}
main.addEventListener('pointermove',function(e){if(pinP||e.pointerType!=='mouse'||ptrs[e.pointerId])return;where(at(e),false)},{passive:true});
main.addEventListener('click',function(e){if(e.target.closest&&e.target.closest('.glyph-tip,.glyph-tools,.glyph-view,.glyph-filt'))return;pinP=at(e);where(pinP,true);dbgLog('where pinned')},true);
window.addEventListener('keydown',function(e){if(e.key==='Escape'&&pinP){pinP=null;el.textContent=txt.replace('[PINNED \u00b7 Esc unpins]','[unpinned \u00b7 hover]')}},true);
el.addEventListener('click',function(e){e.stopPropagation();var s=txt;var done=function(ok){showHint(ok?'WHERE copied':'copy failed')};if(navigator.clipboard&&navigator.clipboard.writeText)navigator.clipboard.writeText(s).then(function(){done(true)},function(){done(false)});else done(false)});
window.neGlyphWhere=function(lon,lat){var p=[px(lon),py(lat)];pinP=p;where(p,true);return txt};
el.textContent='WHERE \u00b7 move the pointer over the map (click pins, Esc unpins, click here copies)';
})();
`
