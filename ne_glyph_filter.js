/* ne_glyph_filter.js — the glyph filter's Worker (docs/ne-filter.md §3c).
   Served at /api/v1/ne/glyph/filter.js; the page starts it as a module-less
   classic Worker. It owns: the field catalogue (/api/v1/ne/filter/fields),
   the expression parser (mirrors ne_filter_expr.go), the fidx bins
   (/api/v1/ne/glyph/fidx, NFX1), the evaluator and the veil painter
   (OffscreenCanvas → PNG blob). The page only places <image>s.

   in : {t:'init', base, veil:[r,g,b], acc:[r,g,b]}
        {t:'expr', f}                        → {t:'expr', canon, words, ignored, ext, empty}
        {t:'want', tiles:[{k,b,bi,bj,x,y,w,h}], px} → one {t:'tile', k, b, url|null, err} per tile as it lands / repaints, plus
                                             {t:'bin', b, n, match, state} per bin (counts) — b = bin key 'i/j' (0.025°), tiles are the view's
                                             LOD-0/1 tiles painted at px screen pixels per frame unit (crisp at any zoom, bounded canvas)
        {t:'at', x, y}                       → {t:'at', hit:{match, rows:[[label, text]]}|null}
        {t:'theme', veil, acc}
   The page revokes blob URLs it drops. */
'use strict';
var ESEQ=0,BASE='',CAT=null,CATP=null,FIELDS={},WORDS={},EXPR=null,EXT=false,BINS={},ORDER=[],WANT={},TILES={},PX=0,VEIL=[255,255,255],ACC=[40,110,70],KEEP=32,SEQ=0;
var NBR=[[1,0],[-1,0],[0,1],[0,-1],[1,1],[-1,-1]];
function catalogue(){if(CATP)return CATP;return CATP=fetch(BASE+'/api/v1/ne/filter/fields').then(function(r){return r.json()}).then(function(j){CAT=j;FIELDS={};j.fields.forEach(function(f){FIELDS[f.id]=f});WORDS=j.words||{};return j})}
/* ── parser (= neFiltParse) ── */
function rangeOK(fd){return fd.kind==='num'||fd.kind==='bucket'}
function member(fd,m){if(!fd.members)return-1;var i,lm=m.toLowerCase();for(i=0;i<fd.members.length;i++)if(fd.members[i]&&fd.members[i].toLowerCase()===lm)return i;if(fd.labels)for(i=0;i<fd.labels.length&&i<fd.members.length;i++)if(fd.labels[i]&&fd.labels[i].toLowerCase().replace(/ /g,'-')===lm.replace(/ /g,'-'))return i;return-1}
function parseTerm(t){var m=/^([a-z][a-z0-9]*)(.*)$/.exec(t);if(!m)return null;var f=m[1],rest=m[2],term={f:f,lo:NaN,hi:NaN,set:[],unk:false,flag:false};
 if(f==='k'){var r=rest.indexOf('>=')===0?rest.slice(2):rest.indexOf('=')===0?rest.slice(1):null;if(r==null)return null;var v=+r;if(!isFinite(v))return null;term.lo=v;return term}
 var fd=FIELDS[f];if(!fd)return null;
 if(rest===''){if(fd.kind==='flag'||fd.kind==='sites'){term.flag=true;return term}if(f==='chg'){term.set=['cand','conf'];return term}return null}
 var num=function(s){var v=+s;return s!==''&&isFinite(v)?v:null};
 if(rest.indexOf('>=')===0){if(!rangeOK(fd))return null;var a=num(rest.slice(2));if(a==null)return null;term.lo=a;return term}
 if(rest.indexOf('<=')===0){if(!rangeOK(fd))return null;var b=num(rest.slice(2));if(b==null)return null;term.hi=b;return term}
 if(rest[0]!=='=')return null;var v=rest.slice(1);if(v==='unk'){term.unk=true;return term}
 if(rangeOK(fd)){var p=v.indexOf('..');if(p>=0){var a2=v.slice(0,p),b2=v.slice(p+2);if(a2===''&&b2==='')return null;if(a2!==''){var x=num(a2);if(x==null)return null;term.lo=x}if(b2!==''){var y=num(b2);if(y==null)return null;term.hi=y}if(!isNaN(term.lo)&&!isNaN(term.hi)&&term.lo>term.hi){var tmp=term.lo;term.lo=term.hi;term.hi=tmp}return term}var x2=num(v);if(x2==null)return null;term.lo=term.hi=x2;return term}
 var seen={};var parts=v.split('+');for(var i=0;i<parts.length;i++){var mm=parts[i].trim().toLowerCase();if(!mm||seen[mm])continue;if(fd.kind==='set'||fd.kind==='bits'){var mi=member(fd,mm);if(mi<0)return null;mm=fd.members[mi]}else if(fd.kind==='sites'){mm=mm.toUpperCase();if(mm.length!==9||mm.indexOf('AT')!==0)return null}else if(fd.kind==='flag'){if(mm==='1'||mm==='true'||mm==='yes'){term.flag=true;return term}return null}seen[mm]=true;term.set.push(mm)}term.set.sort()
 if(!term.set.length)return null;return term}
function parse(s){var e={groups:[],k:0,ignored:[]};s=(s||'').trim().toLowerCase();if(!s)return e;if(s.length>2048)s=s.slice(0,2048);s.split(',').forEach(function(g){g=g.trim();if(!g)return;if(e.groups.length>=64){e.ignored.push(g);return}var terms=[];g.split('|').forEach(function(t){t=t.trim();if(!t)return;var w=WORDS[t.toLowerCase()];if(w)t=w;var term=parseTerm(t);if(term){if(term.f==='k'){if(!isNaN(term.lo)&&term.lo>=2)e.k=Math.min(4096,Math.floor(term.lo));else e.ignored.push(t);return}terms.push(term)}else e.ignored.push(t)});if(terms.length)e.groups.push(terms)});return e}
function fnum(v){return Math.abs(v-Math.round(v))<1e-9?String(Math.round(v)):String(+v.toFixed(3))}
function termStr(t){if(t.f==='k')return'k>='+fnum(t.lo);if(t.unk)return t.f+'=unk';if(t.flag)return t.f;if(t.set.length)return t.f+'='+t.set.join('+');if(!isNaN(t.lo)&&!isNaN(t.hi))return t.lo===t.hi?t.f+'='+fnum(t.lo):t.f+'='+fnum(t.lo)+'..'+fnum(t.hi);if(!isNaN(t.lo))return t.f+'>='+fnum(t.lo);return t.f+'<='+fnum(t.hi)}
function canonical(e){var p=e.groups.map(function(g){return g.map(termStr).sort().join('|')});if(e.k>1)p.push('k>='+e.k);return p.join(',')}
function termWords(t){var fd=FIELDS[t.f];if(!fd)return t.f;var L=fd.label,u=fd.unit||'';if(u&&u!=='%'&&u!=='°')u='\u202f'+u;if(t.f==='k')return'clusters ≥ '+t.lo+' cells';if(t.unk)return L+' unknown';if(t.flag)return L;if(t.set.length){if(fd.kind==='sites')return'site '+t.set.join(' / ');return L+' '+t.set.map(function(m){var i=member(fd,m);return fd.labels&&fd.labels[i]!=null?fd.labels[i]:(m===''?'none':m)}).join(' / ')}
 if(fd.kind==='bucket'){var E=fd.edges;if(!isNaN(t.hi)&&isNaN(t.lo))return L+' < '+E[Math.min(bucketOf(E,t.hi)+1,E.length-1)]+u;if(!isNaN(t.lo)&&isNaN(t.hi))return L+' ≥ '+E[bucketOf(E,t.lo)]+u}
 if(!isNaN(t.lo)&&!isNaN(t.hi))return t.lo===t.hi?L+' = '+fnum(t.lo)+u:L+' '+fnum(t.lo)+'–'+fnum(t.hi)+u;if(!isNaN(t.lo))return L+' ≥ '+fnum(t.lo)+u;return L+' ≤ '+fnum(t.hi)+u}
function words(e){var p=e.groups.map(function(g){var w=g.map(termWords);return w.length>1?'('+w.join(' or ')+')':w[0]});if(e.k>1)p.push('clusters ≥ '+e.k+' cells');return p.join(' · ')}
function needsExt(e){for(var i=0;i<e.groups.length;i++)for(var j=0;j<e.groups[i].length;j++){var fd=FIELDS[e.groups[i][j].f];if(fd&&fd.plane==='ext')return true}return false}
/* ── evaluator (= neFiltTermFn / neFiltEval) ── */
function bucketOf(E,v){var k=0;while(k+1<E.length&&v>=E[k+1])k++;return k}
function rawFn(fd,c){if(fd.dtype==='u16')return function(k){return c[2*k]|c[2*k+1]<<8};if(fd.dtype==='i8')return function(k){var v=c[k];return v>127?v-256:v};return function(k){return c[k]}}
function bounds(fd,t){var lo=-2147483648,hi=2147483647;if(fd.kind==='bucket'){if(!isNaN(t.lo))lo=bucketOf(fd.edges,t.lo);if(!isNaN(t.hi))hi=bucketOf(fd.edges,t.hi);return[lo,hi]}var sc=fd.scale||1,off=fd.off||0;if(!isNaN(t.lo))lo=Math.ceil(Math.round((t.lo-off)/sc*1e6)/1e6);if(!isNaN(t.hi))hi=Math.floor(Math.round((t.hi-off)/sc*1e6)/1e6);return[lo,hi]}
function termFn(bin,t){var fd=FIELDS[t.f];if(!fd)return function(){return false};var c=bin.cols[fd.id];if(!c){return t.unk?function(){return true}:function(){return false}}var nd=fd.nodata,raw=rawFn(fd,c);if(t.unk)return function(k){return raw(k)===nd};
 if(fd.kind==='flag')return function(k){var v=raw(k);return v!==nd&&v!==0};
 if(fd.kind==='set'){var mask=0;t.set.forEach(function(m){var i=member(fd,m);if(i>=0&&i<31)mask|=1<<i});return function(k){var v=raw(k);return v!==nd&&v>=0&&v<31&&(mask&(1<<v))!==0}}
 if(fd.kind==='bits'){var bm=0;t.set.forEach(function(m){var i=member(fd,m);if(i>=0)bm|=1<<i});return function(k){var v=raw(k);return v!==nd&&(v&bm)!==0}}
 if(fd.kind==='sites'){if(t.flag)return function(k){return raw(k)!==nd};var sm=0;(bin.hdr.n2k||[]).forEach(function(code,i){if(t.set.indexOf(code)>=0&&i<31)sm|=1<<i});if(!sm)return function(){return false};return function(k){var v=raw(k);return v!==nd&&v<31&&(sm&(1<<v))!==0}}
 var b=bounds(fd,t),lo=b[0],hi=b[1];if(fd.halo)return function(k){var v=raw(k);if(v===nd)return false;if(v&128)return hi===2147483647&&lo<=64;return v>=lo&&v<=hi};
 var zn=fd.id==='loss'||fd.id==='snar'||fd.id==='snar2';return function(k){var v=raw(k);if(v===nd||(zn&&v===0))return false;return v>=lo&&v<=hi}}
function evaluate(bin,e){var n=bin.n,out=new Uint8Array(n),k;if(!e.groups.length){out.fill(1)}else{var fns=e.groups.map(function(g){return g.map(function(t){return termFn(bin,t)})});for(k=0;k<n;k++){var ok=true;for(var gi=0;gi<fns.length&&ok;gi++){var any=false,g=fns[gi];for(var ti=0;ti<g.length;ti++)if(g[ti](k)){any=true;break}if(!any)ok=false}out[k]=ok?1:0}}
 if(e.k>1)clusterFilter(bin,out,e.k);return out}
function lattice(bin){if(bin.lat)return bin.lat;var m=new Map();for(var k=0;k<bin.n;k++)m.set(key(bin.ij[2*k],bin.ij[2*k+1]),k);return bin.lat=m}
function key(i,j){return((i&65535)<<16|(j&65535))>>>0}
function clusterFilter(bin,m,kmin){var lat=lattice(bin),seen=new Uint8Array(bin.n),stack=[],comp=[];for(var k=0;k<bin.n;k++){if(!m[k]||seen[k])continue;stack.length=0;comp.length=0;stack.push(k);seen[k]=1;while(stack.length){var c=stack.pop();comp.push(c);var ci=bin.ij[2*c],cj=bin.ij[2*c+1];for(var d=0;d<6;d++){var q=lat.get(key(ci+NBR[d][0],cj+NBR[d][1]));if(q!=null&&m[q]&&!seen[q]){seen[q]=1;stack.push(q)}}}if(comp.length<kmin)for(var x=0;x<comp.length;x++)m[comp[x]]=0}}
/* ── bins ── */
function decode(buf){var d=new Uint8Array(buf),dv=new DataView(buf);if(d.length<15||String.fromCharCode(d[0],d[1],d[2],d[3])!=='NFX1')throw new Error('not NFX1');var ncol=dv.getUint16(5,true),n=dv.getUint32(7,true),hl=dv.getUint32(11,true),hdr=JSON.parse(new TextDecoder().decode(d.subarray(15,15+hl))),o=15+hl,nrow=dv.getUint16(o,true),ij=new Int32Array(2*n),p=0;o+=2;
 for(var r=0;r<nrow;r++){var j=dv.getInt16(o,true),nr=dv.getUint16(o+2,true);o+=4;for(var u=0;u<nr;u++){var i0=dv.getInt16(o,true),ln=dv.getUint16(o+2,true);o+=4;for(var q=0;q<ln;q++){ij[p++]=i0+q;ij[p++]=j}}}
 if(p!==2*n)throw new Error('runs');var cols={};hdr.cols.forEach(function(c){var sz=(c.dt==='u16'?2:1)*n;cols[c.id]=d.subarray(o,o+sz);o+=sz});return{hdr:hdr,n:n,ij:ij,cols:cols}}
/* which hex edge (v0 → v0+1) faces lattice neighbour d: the edge whose midpoint direction is closest to the neighbour's centre offset */
function edgeMap(bin){if(bin.E)return bin.E;var a=bin.hdr.a,hex=bin.hdr.hex,E=new Int8Array(6);if(hex.length<6)return bin.E=E;for(var d=0;d<6;d++){var dx=a[1]*NBR[d][0]+a[2]*NBR[d][1],dy=a[(a.length>=12?7:4)]*NBR[d][0]+a[(a.length>=12?8:5)]*NBR[d][1],best=-2,bv=0;for(var v=0;v<6;v++){var w=(v+1)%6,mx=(hex[v][0]+hex[w][0])/2,my=(hex[v][1]+hex[w][1])/2,dot=(mx*dx+my*dy)/Math.sqrt((mx*mx+my*my)*(dx*dx+dy*dy)+1e-12);if(dot>best){best=dot;bv=v}}E[d]=bv}return bin.E=E}
function pos(bin,k){var i=bin.ij[2*k],j=bin.ij[2*k+1],a=bin.hdr.a;if(a.length>=12)return[a[0]+a[1]*i+a[2]*j+a[3]*i*i+a[4]*i*j+a[5]*j*j,a[6]+a[7]*i+a[8]*j+a[9]*i*i+a[10]*i*j+a[11]*j*j];return[a[0]+a[1]*i+a[2]*j,a[3]+a[4]*i+a[5]*j]}
function fetchBin(k,i,j,plane){return fetch(BASE+'/api/v1/ne/glyph/fidx?i='+i+'&j='+j+'&p='+plane).then(function(r){if(r.status===404)return null;if(!r.ok)throw new Error('fidx '+r.status);return r.arrayBuffer().then(function(b){var bin=decode(b);bin.state=r.headers.get('X-NE-Fidx-State')||bin.hdr.state;return bin})})}
function binOf(b){var B=BINS[b];if(!B){var ij=b.split('/');B=BINS[b]={k:b,i:+ij[0],j:+ij[1],core:null,ext:null,m:null,err:null,busy:0,mseq:-1};ORDER.push(b)}else{ORDER.splice(ORDER.indexOf(b),1);ORDER.push(b)}return B}
function loaded(B){return !!B.core&&(!EXT||!!B.ext||!!B.empty||!!B.core.hasExt)}
/* fetch what the bin still lacks (core, then ext when the expression needs it); on arrival re-evaluate and repaint the tiles that touch it */
function ensure(b){var B=binOf(b);if(B.err)return;var need=null;if(!B.core)need='core';else if(EXT&&!loaded(B))need='ext';if(!need){if(!B.busy)ready(B);return}if(B.busy)return;B.busy=1;
 fetchBin(b,B.i,B.j,need).then(function(bin){B.busy=0;if(!BINS[b])return;if(!bin){B.core={hdr:{a:[0,0,0,0,0,0],hex:[],n2k:[]},n:0,ij:new Int32Array(0),cols:{},state:''};B.empty=true}else if(need==='core')B.core=bin;else{B.ext=bin;if(bin.n===B.core.n)for(var c in bin.cols)B.core.cols[c]=bin.cols[c];B.core.hasExt=true}B.mseq=-1;ensure(b)},function(e){B.busy=0;B.err=String(e&&e.message||e);post({t:'bin',b:b,n:0,match:0,err:B.err});for(var tk in WANT)if(WANT[tk].bins.indexOf(b)>=0)post({t:'tile',k:tk,url:null,err:B.err})})}
/* the bin is complete for this expression: evaluate once per expression (ESEQ), report counts, repaint the tiles whose bins are all here */
function ready(B){if(!EXPR)return;if(B.mseq!==ESEQ){var bin=B.core,n=bin.n,m=B.m=evaluate(bin,EXPR),match=0;for(var k=0;k<n;k++)match+=m[k];B.match=match;B.mseq=ESEQ;post({t:'bin',b:B.k,n:n,match:match,state:bin.state,ext:!!bin.hasExt,empty:!!B.empty});for(var t in TILES)if(TILES[t].bins.indexOf(B.k)>=0)TILES[t].dirty=true}
 for(var tk in WANT){var w=WANT[tk];if(w.bins.indexOf(B.k)<0)continue;var all=true;for(var q=0;q<w.bins.length;q++){var Q=BINS[w.bins[q]];if(!Q||!loaded(Q)||Q.mseq!==ESEQ){all=false;break}}if(!all)continue;var T=TILES[tk];if(!T)T=TILES[tk]={k:tk,bins:w.bins,box:w,px:0,dirty:true,pseq:0};if(T.dirty||T.px!==PX){T.box=w;paintTile(T)}}}
function evict(){var used={};for(var tk in WANT)WANT[tk].bins.forEach(function(b){used[b]=1});for(var t in TILES)if(!WANT[t])delete TILES[t];while(ORDER.length>KEEP){var k=null;for(var q=0;q<ORDER.length;q++)if(!used[ORDER[q]]){k=ORDER[q];break}if(k==null)break;ORDER.splice(ORDER.indexOf(k),1);delete BINS[k]}}
/* ── painter: one PNG per view tile, at the view's pixel density ────────────────────────────────────────────────────────────────
   veil = translucent page colour over every non-matching cell (hex polygons, slightly grown so neighbours fuse into one sheet);
   accent hairline on the outer boundary of matching clusters (edges whose neighbour does not match). Tiles are the LOD tiles of the
   glyph itself, so the <image> lands on exactly the same frame box as the data tile beneath — no seam, no drift at any zoom. */
/* scanline fill of one convex hexagon (pixel centres inside the polygon; the polygon is grown by pad px so neighbours fuse and 2-px cells still show) */
function fillHex(buf,W,H,xs,ys){var ymin=Infinity,ymax=-Infinity,v;for(v=0;v<6;v++){if(ys[v]<ymin)ymin=ys[v];if(ys[v]>ymax)ymax=ys[v]}var r0=Math.max(0,Math.ceil(ymin-0.5)),r1=Math.min(H-1,Math.floor(ymax-0.5));if(r1<r0){r0=r1=Math.round((ymin+ymax)/2-0.5);if(r0<0||r0>=H)return}
 for(var r=r0;r<=r1;r++){var yc=r+0.5,xl=Infinity,xr=-Infinity;for(v=0;v<6;v++){var w=(v+1)%6,ya=ys[v],yb=ys[w];if(ya===yb)continue;if((yc<ya)===(yc<yb))continue;var x=xs[v]+(yc-ya)*(xs[w]-xs[v])/(yb-ya);if(x<xl)xl=x;if(x>xr)xr=x}if(xl===Infinity){xl=xr=(xs[0]+xs[3])/2}var c0=Math.max(0,Math.ceil(xl-0.5)),c1=Math.min(W-1,Math.floor(xr-0.5));if(c1<c0){c0=c1=Math.round((xl+xr)/2-0.5);if(c0<0||c0>=W)continue}var o=(r*W+c0)*4+3;for(var c=c0;c<=c1;c++,o+=4)buf[o]=255}}
/* 1-px DDA line in the accent colour, alpha .9 */
function lineTo(buf,W,H,xa,ya,xb,yb,r,g,b){var dx=xb-xa,dy=yb-ya,n=Math.max(1,Math.ceil(Math.max(Math.abs(dx),Math.abs(dy))));for(var i=0;i<=n;i++){var x=Math.floor(xa+dx*i/n),y=Math.floor(ya+dy*i/n);if(x<0||y<0||x>=W||y>=H)continue;var o=(y*W+x)*4;buf[o]=r;buf[o+1]=g;buf[o+2]=b;buf[o+3]=230}}
function paintTile(T){var box=T.box;T.px=PX;T.dirty=false;var any=false,allIn=true,bins=T.bins.map(function(b){var B=BINS[b];if(B&&B.core&&B.core.n){any=true;if(B.match!==B.core.n)allIn=false}return B});if(!any||EXPR.empty||allIn){post({t:'tile',k:T.k,url:null});return}
 /* rasterised in JS into one ImageData — canvas path calls (moveTo/lineTo per hexagon vertex) are far too slow for a few hundred thousand cells on weak devices and software rasterisers; a scanline fill of each hexagon is a few ms */
 /* the PNG covers the tile box grown by R on every side (the <image> is placed the same way, fltView): a cell whose centre sits just inside the edge is painted whole, and two tiles' sheets overlap instead of leaving a dotted seam */
 var R=0.2,bx=box.x-R,by=box.y-R,bw=box.w+2*R,bh=box.h+2*R,W=Math.max(16,Math.min(2048,Math.round(bw*PX))),H=Math.max(16,Math.min(2048,Math.round(bh*PX))),sx=W/bw,sy=H/bh,x0=bx,x1=bx+bw,y0=by,y1=by+bh,bi,k,p,v,buf=new Uint8ClampedArray(W*H*4),vr=VEIL[0],vg=VEIL[1],vb=VEIL[2];
 for(k=0;k<W*H*4;k+=4){buf[k]=vr;buf[k+1]=vg;buf[k+2]=vb}
 var pad=0.6,xs=new Float64Array(6),ys=new Float64Array(6);
 for(bi=0;bi<bins.length;bi++){var B=bins[bi];if(!B||!B.core||!B.core.n)continue;var bin=B.core,hex=bin.hdr.hex,m=B.m,n=bin.n;if(hex.length<6)continue;var hx=new Float64Array(6),hy=new Float64Array(6);for(v=0;v<6;v++){hx[v]=hex[v][0]*sx;hx[v]+=hx[v]>0?pad:-pad;hy[v]=hex[v][1]*sy;hy[v]+=hy[v]>0?pad:-pad}
  for(k=0;k<n;k++){if(m[k])continue;p=pos(bin,k);if(p[0]<x0||p[0]>x1||p[1]<y0||p[1]>y1)continue;var cx=(p[0]-bx)*sx,cy=(p[1]-by)*sy;for(v=0;v<6;v++){xs[v]=cx+hx[v];ys[v]=cy+hy[v]}fillHex(buf,W,H,xs,ys)}}
 /* accent hairline on the outer boundary of matching clusters */
 var ar=ACC[0],ag=ACC[1],ab=ACC[2];
 for(bi=0;bi<bins.length;bi++){var B2=bins[bi];if(!B2||!B2.core||!B2.core.n||!B2.match)continue;var bin2=B2.core,hex2=bin2.hdr.hex,m2=B2.m,lat=lattice(bin2),E=edgeMap(bin2);if(hex2.length<6)continue;for(k=0;k<bin2.n;k++){if(!m2[k])continue;p=pos(bin2,k);if(p[0]<x0||p[0]>x1||p[1]<y0||p[1]>y1)continue;var cx2=(p[0]-bx)*sx,cy2=(p[1]-by)*sy,ci=bin2.ij[2*k],cj=bin2.ij[2*k+1];for(var d=0;d<6;d++){var q=lat.get(key(ci+NBR[d][0],cj+NBR[d][1]));if(q!=null&&m2[q])continue;var v0=E[d],v1=(v0+1)%6;lineTo(buf,W,H,cx2+hex2[v0][0]*sx,cy2+hex2[v0][1]*sy,cx2+hex2[v1][0]*sx,cy2+hex2[v1][1]*sy,ar,ag,ab)}}}
 var cv=new OffscreenCanvas(W,H),g=cv.getContext('2d');g.putImageData(new ImageData(buf,W,H),0,0);
 var seq=++SEQ;T.pseq=seq;cv.convertToBlob({type:'image/png'}).then(function(blob){if(T.pseq!==seq||!WANT[T.k])return;post({t:'tile',k:T.k,url:URL.createObjectURL(blob)})})}
/* ── the cell under a point ── */
function at(x,y){var best=null,bd=1e9;for(var k0 in BINS){var B=BINS[k0],bin=B.core;if(!bin||!B.m||!bin.n)continue;for(var k=0;k<bin.n;k++){var p=pos(bin,k),dd=(p[0]-x)*(p[0]-x)+(p[1]-y)*(p[1]-y);if(dd<bd){bd=dd;best={B:B,k:k}}}}
 if(!best||bd>0.03)return null;var bin=best.B.core,rows=[],seen={};EXPR.groups.forEach(function(g){g.forEach(function(t){if(seen[t.f])return;seen[t.f]=1;var fd=FIELDS[t.f];if(!fd)return;var c=bin.cols[fd.id];if(!c){rows.push([fd.label,'—']);return}var v=rawFn(fd,c)(best.k),s;if(v===fd.nodata)s='unknown';else if(fd.kind==='set')s=fd.labels&&fd.labels[v]!=null?fd.labels[v]:fd.members[v]==null?String(v):(fd.members[v]||'none');else if(fd.kind==='flag')s=v?'yes':'no';else if(fd.kind==='bits'){var b=[];fd.members.forEach(function(mm,i){if(v&(1<<i))b.push(mm)});s=b.length?b.join(' '):'none'}else if(fd.kind==='sites')s=(bin.hdr.n2k||[])[v]||String(v);else if(fd.kind==='bucket'){var E=fd.edges;s=v+1<E.length?E[v]+'–'+E[v+1]+'\u202f'+fd.unit:'≥ '+E[v]+'\u202f'+fd.unit}else if(fd.halo)s=(v&128)?'≥ '+Math.max(64,v&127)+' (leaves the bin)':String(v);else{if((fd.id==='loss'||fd.id==='snar'||fd.id==='snar2')&&v===0)s='none';else{var pv=v*(fd.scale||1)+(fd.off||0);s=fnum(pv)+(fd.unit?'\u202f'+fd.unit:'')}}rows.push([fd.label,s])})});return{match:!!best.B.m[best.k],rows:rows}}
/* ── messages ── */
function post(m){self.postMessage(m)}
function repaintAll(){var bins={};for(var tk in WANT)WANT[tk].bins.forEach(function(b){bins[b]=1});for(var b in bins)ensure(b)}
self.onmessage=function(ev){var m=ev.data;
 if(m.t==='init'){BASE=m.base||'';if(m.veil)VEIL=m.veil;if(m.acc)ACC=m.acc;catalogue().then(function(c){post({t:'cat',cat:c})},function(e){post({t:'err',err:'fields: '+(e&&e.message||e)})});return}
 if(m.t==='theme'){if(m.veil)VEIL=m.veil;if(m.acc)ACC=m.acc;for(var t in TILES)TILES[t].dirty=true;repaintAll();return}
 catalogue().then(function(){
  if(m.t==='expr'){EXPR=parse(m.f);EXPR.empty=!EXPR.groups.length&&!EXPR.k;EXT=needsExt(EXPR);ESEQ++;post({t:'expr',f:m.f,canon:canonical(EXPR),words:words(EXPR),groups:EXPR.groups.map(function(g){return{canon:g.map(termStr).join('|'),words:g.map(termWords).join(' or ')}}).concat(EXPR.k>1?[{canon:'k>='+EXPR.k,words:'clusters \u2265 '+EXPR.k+' cells'}]:[]),ignored:EXPR.ignored,ext:EXT,empty:EXPR.empty});repaintAll();return}
  if(m.t==='want'){WANT={};if(m.px)PX=m.px;m.tiles.forEach(function(w){WANT[w.k]=w;var T=TILES[w.k];if(T&&T.bins.join()!==w.bins.join()){T.bins=w.bins;T.dirty=true}});evict();repaintAll();return}
  if(m.t==='at'){post({t:'at',id:m.id,hit:EXPR?at(m.x,m.y):null});return}
 },function(e){post({t:'err',err:String(e&&e.message||e)})})};
