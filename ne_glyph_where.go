package main

// neGlyphDbgJS — the `?dbg` WHERE panel (docs/ne-glyph.md §dbg where): a fixed
// pre in the top-right corner that names the spot under the pointer so an
// operator can hand it to the observed-layer peer without a second tool. The
// body of the panel IS the text of GET /api/v1/ne/where?format=txt (ne_where.go)
// for the pointer's lon/lat and the current share hash — what a person reads
// here an agent curls, byte for byte; the panel only prefixes the client-side
// facts (viewBox xy, the live zoom, the doc the page holds under the pointer)
// and the curl line. Hover follows the pointer (220 ms debounce); a click
// pins; Escape unpins; a click on the panel copies it. Only with ?dbg.
const neGlyphDbgJS = `
if(DBG)(function(){var el=document.createElement('pre');el.className='glyph-dbgw';el.style.cssText='position:fixed;right:0;top:0;z-index:99999;max-width:min(100vw,52vw);max-height:76vh;overflow:auto;margin:0;padding:6px 8px;font:10px/1.3 monospace;background:rgba(0,0,0,.86);color:#ffd;white-space:pre-wrap;word-break:break-all;user-select:text;cursor:copy;box-shadow:0 0 0 1px #663';el.title='WHERE \u00b7 click = copy \u00b7 click the map = pin \u00b7 Esc = unpin';document.body.appendChild(el);
var pinP=null,T=0,A=null,srv='',srvK='',txt='';
function f6(v){return(+v).toFixed(6)}
function whereURL(ll,fmt){return location.origin+'/api/v1/ne/where?pt='+ll[0].toFixed(6)+','+ll[1].toFixed(6)+'&share='+encodeURIComponent(curHash())+'&ar='+(vb.h/vb.w).toFixed(4)+'&format='+fmt}
function head(p,pinned){var ll=lonlat(p[0],p[1]),z=W/vb.w,o=[];
 o.push('WHERE '+(pinned?'[PINNED \u00b7 Esc unpins]':'[hover \u00b7 click pins]')+'  xy '+p[0].toFixed(1)+','+p[1].toFixed(1)+'  live z='+z.toFixed(1)+' lod='+(hi.on?hiLOD():'-')+' full='+hi.on);
 o.push('agent  curl -s "'+whereURL(ll,'txt')+'"   (format=json for the structure)');
 var d=hi.on?hiFind(p):null;if(d)o.push('page doc under pointer  '+d.cell+(d.pic_obs?'  pic_obs='+JSON.stringify(d.pic_obs):''));
 return o.join('\n')}
function paint(p,pinned){var ll=lonlat(p[0],p[1]),k=f6(ll[0])+','+f6(ll[1]);txt=head(p,pinned)+'\n'+(srvK===k&&srv?srv:'\u2026 loading /api/v1/ne/where');el.textContent=txt}
function ask(p,pinned){var ll=lonlat(p[0],p[1]),k=f6(ll[0])+','+f6(ll[1]);if(srvK===k&&srv)return;srvK=k;srv='';if(A)A.abort();var a=A=new AbortController();fetch(whereURL(ll,'txt'),{credentials:'omit',signal:a.signal}).then(function(r){return r.text()}).then(function(t){if(a!==A)return;srv=t||'(empty)';paint(p,pinned);dbgLog('where '+k)}).catch(function(e){if(e&&e.name==='AbortError')return;srv='where failed: '+e;paint(p,pinned)})}
function where(p,pinned){paint(p,pinned);clearTimeout(T);T=setTimeout(function(){ask(p,pinned)},pinned?0:220)}
main.addEventListener('pointermove',function(e){if(pinP||e.pointerType!=='mouse'||ptrs[e.pointerId])return;where(at(e),false)},{passive:true});
main.addEventListener('click',function(e){if(e.target.closest&&e.target.closest('.glyph-tip,.glyph-tools,.glyph-view,.glyph-filt'))return;pinP=at(e);where(pinP,true);dbgLog('where pinned')},true);
window.addEventListener('keydown',function(e){if(e.key==='Escape'&&pinP){pinP=null;el.textContent=txt.replace('[PINNED \u00b7 Esc unpins]','[unpinned \u00b7 hover]')}},true);
el.addEventListener('click',function(e){e.stopPropagation();var s=txt;var done=function(ok){showHint(ok?'WHERE copied':'copy failed')};if(navigator.clipboard&&navigator.clipboard.writeText)navigator.clipboard.writeText(s).then(function(){done(true)},function(){done(false)});else done(false)});
window.neGlyphWhere=function(lon,lat){var p=[px(lon),py(lat)];pinP=p;where(p,true);return whereURL([lon,lat],'txt')};
el.textContent='WHERE \u00b7 move the pointer over the map (click pins, Esc unpins, click here copies)\nagent  curl -s "'+location.origin+'/api/v1/ne/where?share='+encodeURIComponent(curHash())+'&format=txt"';
})();
`
