// ne_glyph_export.go — the landing page's link control: a split button
// (link icon = copy the share link, as before | ▾ = the export menu). Every
// entry is the SAME hash rendered by another endpoint: the share link
// (`#kg=…;f=…`) and `/api/v1/ne/stats/unit?share=<hash>` are one URL logic
// (api_ne_stats_unit.go), so what a user looks at is what the sheet gets.
//
// Menu (two short rows, one URL line — nothing to configure):
//
//	EXPORT  JSON · CSV · flat · Sheets        this unit (KG / Gemeinde / cell / point)
//	QGIS    .qgs · tiles · PMTiles            .qgs = project of this view (mvt.qgs, every toggle
//	                                          as a legend entry); tiles = copy the MVT URL; PMTiles = archive
//	                                          of the CURRENT VIEW (z 9–14); when the view
//	                                          would exceed ~100 MB the link is replaced by
//	                                          "view too big — zoom in" (xArchMB estimate).
//	…/ne/stats/unit?share=#…&format=csv&key=[your key]   key optional, localStorage only.
//
// Nothing in the menu is a fetch: links only; the export endpoint counts.
// Docs: docs/ne-glyph.md row *EXPORT*, docs/stats-envelope.md, docs/qgis-layer.md.
package main

const neGlyphExportBtn = `<span class="gt-kk"><button type="button" class="gt-b gt-k" aria-label="copy a link to this view" title="copy link to this view"><svg viewBox="0 0 16 16" aria-hidden="true" class="ln"><path d="M6.6 9.4a3 3 0 0 0 4.2 0l2.1-2.1a3 3 0 0 0-4.2-4.2l-1 1"/><path d="M9.4 6.6a3 3 0 0 0-4.2 0L3.1 8.7a3 3 0 0 0 4.2 4.2l1-1"/></svg><svg viewBox="0 0 16 16" aria-hidden="true" class="ok"><path d="M3.5 8.5l3 3 6-6.5"/></svg><span class="gt-t">link</span></button><button type="button" class="gt-b gt-kx" aria-haspopup="menu" aria-expanded="false" aria-label="export this view" title="export · table, JSON, Sheets, QGIS"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4.5 6.5l3.5 3.5 3.5-3.5"/></svg></button><div class="gt-xm" role="menu" aria-label="export" hidden></div></span>`

const neGlyphExportCSS = `
.gt-kk{position:relative;display:inline-flex;align-items:center}.gt-kk .gt-k{border-radius:3px 0 0 3px}.gt-kx{padding:.1em .1em .1em .05em;margin-left:-2px;border-radius:0 3px 3px 0;opacity:.5}.gt-kx svg{width:.8em;height:.8em}.gt-kx:hover,.gt-kx[aria-expanded="true"]{opacity:1}.gt-kk.open .gt-k,.gt-kk.open .gt-kx{color:var(--fg);background:color-mix(in srgb,var(--fg) 7%,transparent);opacity:1}
.gt-xm{position:absolute;top:calc(100% + .5em);left:0;z-index:7;width:22em;max-width:calc(100vw - 2em);padding:.55em .75em .6em;background:var(--bg);border:1px solid var(--rule);border-radius:6px;box-shadow:0 4px 18px rgba(0,0,0,.08);font-size:.92em;line-height:1.5;text-align:left}
.gt-xm .fm{display:flex;flex-wrap:wrap;align-items:center;gap:.35em}.gt-xm .fm+.fm{margin-top:.3em}.gt-xm .fm .lb{flex:0 0 3.6em;color:var(--mut);font-size:.8em;letter-spacing:.05em;text-transform:uppercase}
.gt-xm .fm a,.gt-xm .fm button{display:inline-block;background:none;border:1px solid var(--rule);border-radius:4px;padding:.05em .55em;font:inherit;font-size:.9em;color:var(--fg);text-decoration:none;cursor:pointer;line-height:1.5}.gt-xm .fm a:hover,.gt-xm .fm button:hover,.gt-xm .fm a:focus-visible,.gt-xm .fm button:focus-visible{border-color:color-mix(in srgb,var(--fg) 45%,transparent);background:color-mix(in srgb,var(--fg) 4%,transparent);outline:none}
.gt-xm .url{display:block;margin-top:.45em;padding-top:.45em;border-top:1px solid var(--rule);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.78em;color:var(--mut);white-space:normal;word-break:break-all;line-height:1.7;cursor:copy}.gt-xm .url b{font-weight:400;color:var(--fg)}.gt-xm .url:hover{color:var(--fg)}
.gt-xm .url input{font:inherit;color:var(--fg);background:none;border:0;border-bottom:1px dashed color-mix(in srgb,var(--fg) 45%,transparent);padding:0 .1em;width:5.5em;min-width:3em;outline:none;cursor:text;vertical-align:baseline}.gt-xm .url input:focus{border-bottom-style:solid}.gt-xm .url input::placeholder{color:var(--mut);opacity:.7;font-style:italic}
.gt-xm .off{color:var(--mut);font-style:italic;font-size:.9em}/* the QGIS row stays one line: .qgs · tiles · PMTiles */.gt-xm .fm.q{flex-wrap:nowrap;white-space:nowrap}.gt-xm .fm.q a,.gt-xm .fm.q button{flex:0 0 auto}
/* the bar sits at the bottom in full screen (and under a phone keyboard): the menu opens upward when there is no room below (xOpen measures) */.gt-kk.up .gt-xm{top:auto;bottom:calc(100% + .5em)}/* caret: points at the ▾ button that opened it (--ax set by xOpen, flips with .up) */.gt-xm::before,.gt-xm::after{content:'';position:absolute;left:var(--ax,1.5em);width:0;height:0;border:7px solid transparent;margin-left:-7px;pointer-events:none}.gt-xm::before{top:-14px;border-bottom-color:var(--rule)}.gt-xm::after{top:-12px;border-bottom-color:var(--bg)}.gt-kk.up .gt-xm::before,.gt-kk.up .gt-xm::after{top:auto;border-bottom-color:transparent}.gt-kk.up .gt-xm::before{bottom:-14px;border-top-color:var(--rule)}.gt-kk.up .gt-xm::after{bottom:-12px;border-top-color:var(--bg)}.gt-xm:not([hidden]){animation:gt-xm-in .16s cubic-bezier(.2,.8,.2,1);transform-origin:var(--ax,1.5em) top}.gt-kk.up .gt-xm:not([hidden]){transform-origin:var(--ax,1.5em) bottom}@keyframes gt-xm-in{from{opacity:0;transform:translateY(-4px) scale(.98)}to{opacity:1;transform:none}}@media(prefers-reduced-motion:reduce){.gt-xm:not([hidden]){animation:none}}
@media(max-width:640px),(hover:none) and (pointer:coarse){.gt-xm{left:auto;right:0;font-size:14px;max-width:calc(100vw - 1.5em)}.gt-xm .fm a,.gt-xm .fm button{padding:.25em .7em}.gt-xm .url{font-size:.8em}}
@media(prefers-color-scheme:dark){.gt-xm{box-shadow:0 4px 18px rgba(0,0,0,.4)}}
`

const neGlyphExportJS = `
/* ── export menu (ne_glyph_export.go): links only, same hash as the share link; QGIS tile URL + an archive of the current view */
var tKX=tools.querySelector('.gt-kx'),tXM=tools.querySelector('.gt-xm'),XK='ne.key',XKM2=1500;/* XKM2 = the server's cap for a z 9–14 archive (ne_mvt_jobs.go nePMTMaxKm2); beyond it the menu offers the z ≤ 11 overview of the view */
function unitHash(){var h=curHash().replace(/^#/,'');if(!h)return '';var u='',f='',t=h.split(';');for(var i=0;i<t.length;i++){if(!u&&/^(kg|h3|adm|pt)=/.test(t[i]))u=t[i];else if(/^kg=/.test(u)&&/^adm=/.test(t[i]))u+=';'+t[i];/* a combined selection: kg= and adm= lists side by side — the server answers with ONE row, their union (every cell once) */else if(/^f=/.test(t[i])&&!/^f=off:/i.test(t[i]))f=t[i]}return u?(f?u+';'+f:u):''}
function xKey(){try{return localStorage.getItem(XK)||''}catch(e){return ''}}
function xK(u){var k=xKey();return k?u+(u.indexOf('?')<0?'?':'&')+'key='+encodeURIComponent(k):u}
/* the unit the table covers: the picked KG / Gemeinde / cell / point (share=, the hash verbatim) or, without one, the
   CURRENT VIEW as bbox= (+ the filter): cell-exact while it fits the index (≤ XBINS bins of 0.025°, api_ne_stats_unit.go
   neStatsMaxBins), farther out the server answers from the national 1-in-49 sample (approx:true) — the row says so (≈) */
var XBINS=48;function xViewBins(b){var d=typeof FB!=='undefined'?FB:0.025,i0=Math.floor(b[0]/d),i1=Math.floor((b[2]-1e-9)/d),j0=Math.floor(b[1]/d),j1=Math.floor((b[3]-1e-9)/d);return (i1-i0+1)*(j1-j0+1)}
function xUnit(){var h=unitHash();if(h)return{q:'share='+encodeURIComponent('#'+h),show:'?share=<b>#'+esc(decodeURIComponent(h))+'</b>',n:'this unit',view:false};
 var b=xViewBox(),f=xF(),bs=b.map(function(v){return v.toFixed(4)}).join(',');
 return{q:'bbox='+bs+(f?'&f='+f:''),show:'?bbox=<b>'+bs+'</b>'+(f?'&amp;f=<b>'+esc(decodeURIComponent(f))+'</b>':''),n:'the current view',view:true,approx:xViewBins(b)>XBINS}}
function xBase(){var u=xUnit();return u?pubOrigin()+'/api/v1/ne/stats/unit?'+u.q:''}
function xURL(fmt,extra){var b=xBase();if(!b)return '';return xK(b+'&format='+fmt+'&src=glyph'+(extra||''))}
/* the active filter (the hash's ;f=) rides on every QGIS URL: tiles and archives then hold only the matching cells, like the veil.
   < > | are percent-encoded (mail clients and messengers stop linkifying at them; the server unescapes) */
function xF(){var t=curHash().replace(/^#/,'').split(';');for(var i=0;i<t.length;i++)if(/^f=/.test(t[i])&&t[i].length>2&&!/^f=off:/i.test(t[i]))return linkSafe(t[i].slice(2).replace(/\+/g,'%2B'));return ''}
function xMVT(){var f=xF();return xK(pubOrigin()+'/api/v1/ne/mvt/{z}/{x}/{y}.pbf'+(f?'?f='+f:''))}
/* the QGIS project: the WHOLE hash (view, profile, overlays, filter, unit) — the server turns it into a .qgs that opens on this view with the same layers ticked (ne_mvt_qgs.go) */
/* the view as a hash without the page-only tokens: the state a project is styled from */
function xView(){return curHash().replace(/^#/,'').split(';').filter(function(t){return t&&t!=='export'&&t!=='open'&&t!=='min'&&t!=='below'&&t!=='screen'}).join(';')}
function xProj(){return xK(pubOrigin()+'/api/v1/ne/mvt.qgs?share='+encodeURIComponent('#'+xView()))}
/* current view as a lon/lat box (the SVG viewBox), its km² and a size estimate for a z 9–14 archive (~0.17 MB/km², measured) */
function xViewBox(){var a=lonlat(vb.x,vb.y),b=lonlat(vb.x+vb.w,vb.y+vb.h);return[Math.min(a[0],b[0]),Math.min(a[1],b[1]),Math.max(a[0],b[0]),Math.max(a[1],b[1])]}
function xViewKm2(b){var la=(b[1]+b[3])/2*Math.PI/180;return (b[2]-b[0])*111.32*Math.cos(la)*(b[3]-b[1])*110.57}
/* archive target: the selected KG / area (its bbox, via share=) or, without one, the current view (bbox=) */
function xArchT(){var h=unitHash(),u=h?h.split(';').filter(function(x){return!/^f=/.test(x)}).join(';'):'';if(sel&&/^kg=/.test(u)){var a=lonlat(sel.b[0],sel.b[1]),c=lonlat(sel.b[2],sel.b[3]);return{box:[Math.min(a[0],c[0]),Math.min(a[1],c[1]),Math.max(a[0],c[0]),Math.max(a[1],c[1])],u:u,n:'this KG'}}
 if(asel&&/^(adm|kg)=/.test(u)){var a2=lonlat(asel.b[0],asel.b[1]),c2=lonlat(asel.b[2],asel.b[3]);return{box:[Math.min(a2[0],c2[0]),Math.min(a2[1],c2[1]),Math.max(a2[0],c2[0]),Math.max(a2[1],c2[1])],u:u,n:asel.set?'this selection':asel.kind||'this area'}}
 if(/^h3=/.test(u))return{box:[0,0,0,0],u:u,n:'this cell'};return{box:xViewBox(),u:'',n:'the current view'}}
function xArchMB(t){return Math.max(0.1,xViewKm2(t.box)*0.17)}
/* maxz 14 = every cell (z 9–14, ≤ XKM2 km²); maxz 11 = the overview from the national table, any area up to all of Austria (docs/qgis-layer.md) */
function xArch(t,maxz){var f=xF();return xK(pubOrigin()+'/api/v1/ne/pmtiles?'+(t.u?'share='+encodeURIComponent('#'+t.u+(f?';f='+f:'')):'bbox='+t.box.map(function(v){return v.toFixed(4)}).join(',')+(f?'&f='+f:''))+'&maxz='+(maxz||14)+'&format=zip&view='+encodeURIComponent('#'+xView())+'&src=glyph')}
function xCopy(s,msg){var done=function(ok){showHint(ok?msg:s)};if(navigator.clipboard&&navigator.clipboard.writeText)navigator.clipboard.writeText(s).then(function(){done(true)},function(){done(fallback(s))});else done(fallback(s))}
function xMenu(){var u=xUnit(),t=xArchT(),mb=xArchMB(t),s='';
 if(u)s+='<div class="fm"><span class="lb">export</span><a href="'+esc(xURL('json'))+'" target="_blank" rel="noopener" data-f="json" title="'+u.n+' as JSON">JSON</a><a href="'+esc(xURL('csv','&download=1'))+'" download data-f="csv" title="'+u.n+' as CSV">CSV</a><a href="'+esc(xURL('flat'))+'" target="_blank" rel="noopener" data-f="flat" title="'+u.n+', one flat row">flat</a><button type="button" data-a="sheet" title="copy an =IMPORTDATA() formula for Google Sheets">Sheets</button>'+(u.view?'<span class="off" title="no KG, Gemeinde, cell or point picked: the table covers the cells in view'+(xF()?', matching the filter':'')+(u.approx?' \u2014 estimated from the national 1-in-49 sample (the view spans more than '+XBINS+' bins of 0.025\u00b0); zoom in for cell-exact figures':'')+'">'+(u.approx?'\u2248 ':'')+'the view</span>':'')+'</div>';
 else s+='<div class="fm"><span class="lb">export</span><span class="off">pick a KG, Gemeinde, cell or point</span></div>';
 s+='<div class="fm q"><span class="lb">QGIS</span><a href="'+esc(xProj())+'" download data-f="qgs" title="QGIS project file (.qgs) of this view \u2014 live cells with legend, tree crowns, subsidy claims, roads, relief and orthophoto as in the picture; double-click to open">.qgs</a><button type="button" data-a="mvt" title="copy the vector-tile URL \u2014 paste it as a vector tile layer'+(xF()?'; only cells matching the filter':'')+'">tiles</button>'+(xViewKm2(t.box)<=XKM2?'<a href="'+esc(xArch(t))+'" target="_blank" rel="noopener" data-f="pmtiles" title="offline archive of '+t.n+', every cell, zoom 9\u201314 \u2014 about '+(mb<1?'1':Math.round(mb))+' MB. A zip: the .pmtiles, its style (.qml) and a project (.qgs) that opens it as in the picture">PMTiles</a>':'<a href="'+esc(xArch(t,11))+'" target="_blank" rel="noopener" data-f="pmtiles" data-z="11" title="'+t.n+' spans about '+Math.round(xViewKm2(t.box))+' km\u00b2 \u2014 too large for an archive of every cell (\u2264 '+XKM2+' km\u00b2, about '+Math.round(mb)+' MB); this one holds the overview, zoom 9\u201311, of the whole view. Zoom in for every cell.">PMTiles <span class="off">z\u2264\u202f11</span></a>')+'</div>';
 s+='<span class="url" title="copy this URL" data-a="url">'+(u?'<b>\u2026/ne/stats/unit</b>'+u.show+'&amp;format=csv':'<b>\u2026/ne/mvt/</b>{z}/{x}/{y}.pbf'+(xF()?'?f=<b>'+esc(decodeURIComponent(xF()))+'</b>':''))+'&amp;key=<input type="text" autocomplete="off" spellcheck="false" placeholder="your key" value="'+esc(xKey())+'" aria-label="export key, optional, stays in this browser"></span>';
 tXM.innerHTML=s}
/* ;export in the hash = the menu is open (share links restore it, applyHash passes quiet=true) */
function xOpen(o,quiet){if(o===undefined)o=tXM.hidden;if(o){xMenu();var r=tKX.getBoundingClientRect(),vh=window.visualViewport?window.visualViewport.height:window.innerHeight,kk=tKX.parentNode;kk.classList.toggle('up',vh-r.bottom<300&&r.top>vh-r.bottom);tXM.hidden=false;tKX.setAttribute('aria-expanded','true');kk.classList.add('open');tXM.style.left='';tXM.style.right='';var mr=tXM.getBoundingClientRect(),kr=kk.getBoundingClientRect(),vw=document.documentElement.clientWidth||window.innerWidth,mw=mr.width,want=mr.left,lo=6,hi=vw-mw-6;if(hi<lo)hi=lo;if(want<lo||want>hi){want=Math.max(lo,Math.min(hi,want));tXM.style.right='auto';tXM.style.left=(want-kr.left)+'px'}var ax=kr.left+kr.width/2-want;tXM.style.setProperty('--ax',Math.max(12,Math.min(mw-12,ax))+'px')}else{tXM.hidden=true;tKX.setAttribute('aria-expanded','false');tKX.parentNode.classList.remove('open')}if(!quiet)setHash()}
tKX.addEventListener('click',function(e){e.stopPropagation();closeTools();xOpen()});
tXM.addEventListener('click',function(e){if(e.target.tagName==='INPUT')return;var b=e.target.closest('[data-a],[data-f]');if(!b)return;var a=b.getAttribute('data-a');
 if(a==='sheet'){xCopy('=IMPORTDATA("'+xURL('csv')+'")','Sheets formula copied')}else if(a==='url'){xCopy(xUnit()?xURL('csv'):xMVT(),'URL copied')}else if(a==='mvt'){xCopy(xMVT(),'tile URL copied \u2014 add it as a vector tile layer in QGIS')}else{dbgLog('export '+b.getAttribute('data-f'))}xOpen(false)});
tXM.addEventListener('input',function(e){if(e.target.tagName!=='INPUT')return;try{var v=e.target.value.trim();if(v)localStorage.setItem(XK,v);else localStorage.removeItem(XK)}catch(x){}
 var as=tXM.querySelectorAll('a[data-f]');for(var i=0;i<as.length;i++){var f=as[i].getAttribute('data-f');as[i].href=f==='pmtiles'?xArch(xArchT(),as[i].getAttribute('data-z')?11:14):f==='qgs'?xProj():xURL(f,f==='csv'?'&download=1':'')}});
tXM.addEventListener('keydown',function(e){if(e.key==='Escape'){xOpen(false);tKX.focus();e.stopPropagation()}});
document.addEventListener('pointerdown',function(e){if(!tXM.hidden&&!tXM.contains(e.target)&&!tKX.contains(e.target))xOpen(false)});
`
