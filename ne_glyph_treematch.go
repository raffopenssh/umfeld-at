package main

// ne_glyph_treematch.go — the species search of the tree card (docs/ne-glyph.md row TREE REPORTS).
//
// Pure functions over the observed-layer peer's dictionary rows
// [taxon, de, en, class, rank, codes[], alt[]] (served in rank order), so TestGlyphTreeMatch runs
// them under node. The peer resolves the same inputs on a write (its resolver is the contract);
// this is what the reader sees while typing:
//
//	exact name / code / other name
//	→ inside a shown name (from 3 letters; 2 letters: its start), or "P. abies", "PiAb", "PicAbi"
//	  (genus prefix + epithet prefix)
//	→ a code starting with it → inside another name → one or two typos.
//
// Within one step the caller's order decides (trees named in view, the reader's own, rank).
// Normalisation = the peer's: lower-case, ß → ss, no diacritics, ä/ö/ü also as ae/oe/ue (both forms
// are indexed), "×" and " x " dropped, hyphens, dots and spaces removed.
const neGlyphTreeMatchJS = `
function trKey(s,ae){s=String(s||'').toLowerCase().replace(/\u00df/g,'ss').replace(/\u00d7/g,' ').replace(/ x /g,' ');if(ae)s=s.replace(/\u00e4/g,'ae').replace(/\u00f6/g,'oe').replace(/\u00fc/g,'ue');if(s.normalize)s=s.normalize('NFD').replace(/[\u0300-\u036f]/g,'');var c='',w=[0],gap=false;for(var i=0;i<s.length;i++){var ch=s.charAt(i);if(/[a-z0-9]/.test(ch)){if(gap&&c.length)w.push(c.length);gap=false;c+=ch}else gap=true}return{c:c,w:w}}
/* rows → the index: every name in both umlaut forms with its field (0 de · 1 en · 2 taxon · 3 code · 4 other name), genus + epithet of a species */
function trIndex(taxa){return taxa.map(function(t){var N=[],seen={},add=function(s,f){for(var ae=0;ae<2;ae++){var k=trKey(s,ae);if(!k.c||seen[f+k.c])continue;seen[f+k.c]=1;k.f=f;k.s=s;N.push(k)}};add(t[1],0);add(t[2],1);add(t[0],2);(t[5]||[]).forEach(function(s){add(s,3)});(t[6]||[]).forEach(function(s){add(s,4)});var p=String(t[0]).replace(/ x /,' ').split(' '),r={n:N,g:'',e:'',tx:t[0]};if(p.length>1){r.g=trKey(p[0]).c;r.e=trKey(p[p.length-1]).c}return r})}
function trLev(a,b,max){var n=a.length,m=b.length;if(Math.abs(n-m)>max)return max+1;var p=[],i,j;for(j=0;j<=m;j++)p[j]=j;for(i=1;i<=n;i++){var d=[i],lo=i;for(j=1;j<=m;j++){d[j]=Math.min(p[j]+1,d[j-1]+1,p[j-1]+(a.charCodeAt(i-1)===b.charCodeAt(j-1)?0:1));if(d[j]<lo)lo=d[j]}if(lo>max)return max+1;p=d}return p[m]}
/* where q sits in a name: at a word's start, else inside ONE word ("nuss" is not in "Pinus sylvestris") */
function trAt(n,c){for(var at=n.c.indexOf(c);at>=0;at=n.c.indexOf(c,at+1)){if(n.w.indexOf(at)>=0)return at;var ok=true;for(var y=1;y<n.w.length;y++)if(n.w[y]>at&&n.w[y]<at+c.length){ok=false;break}if(ok)return at}return-1}
/* → [{i: row, via: the name that matched when it is not the shown one}], best first, ≤ lim. pri(i) orders within a step (lower first), then rank. An empty query lists species by pri alone. */
function trSearch(IX,q,pri,lim){lim=lim||6;pri=pri||function(){return 0};var raw=String(q||'').toLowerCase().replace(/\s+(spp?|spec)\.?\s*$/,''),k=trKey(raw),c=k.c,S=[],i;
 if(!c){for(i=0;i<IX.length;i++){var p0=pri(i);if(IX[i].g||p0<0)S.push([0,p0,i,null])}}
 else{var two=k.w.length===2?[c.slice(0,k.w[1]),c.slice(k.w[1])]:null;
  for(i=0;i<IX.length;i++){var r=IX[i],b=9,via=null,de=false;
   for(var x=0;x<r.n.length;x++){var n=r.n[x],sc=9,at=trAt(n,c);if(at>=0){if(n.c.length===c.length)sc=0;else if(n.f===3){if(at===0)sc=1.6}else if(at===0||c.length>=3&&(n.f!==2||n.w.indexOf(at)>=0))sc=n.f===4?2:1}if(sc<9&&n.f===0)de=true;if(sc<b){b=sc;via=n.f?n.s:null}}
   if(r.g&&b>1){var hit=false;if(two)hit=two[1].length>=2&&r.g.lastIndexOf(two[0],0)===0&&r.e.lastIndexOf(two[1],0)===0;else if(c.length>=4)for(var s=1;s<=c.length-2&&!hit;s++)hit=r.g.lastIndexOf(c.slice(0,s),0)===0&&r.e.lastIndexOf(c.slice(s),0)===0;if(hit){b=1;via=r.tx;de=false}}
   if(b<9)S.push([b,pri(i),i,de?null:via])}
  if(!S.length&&c.length>=4){var mx=c.length>=7?2:1;for(i=0;i<IX.length;i++){var best=mx+1,bv=null;IX[i].n.forEach(function(n){if(n.f===3)return;for(var L=c.length-1;L<=c.length+1;L++){var dd=trLev(c,n.c.slice(0,L),mx);if(dd<best){best=dd;bv=n.f?n.s:null}}});if(best<=mx)S.push([5+best,pri(i),i,bv])}}}
 S.sort(function(a,b){return a[0]-b[0]||a[1]-b[1]||a[2]-b[2]});return S.slice(0,lim).map(function(e){return{i:e[2],via:e[3]}})}
`
