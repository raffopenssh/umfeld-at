/* ne_glyph_kx.js — KX, the one switch registry of the glyph's diagnostics (docs/ne-glyph.md §Field diagnostics).
   A switch turns ONE moving part of the page off. CSS switches are a class kx-<name> on the figure, behaviour
   switches are read where the behaviour lives (KX.on('cap') next to setPointerCapture). Three drivers share it:
   ?kx=a,b (preset by URL), the ?deep panel (tick by hand) and ?run=<scenario> (the page bisects them itself).
   The run's state machine (kxRunStep) is pure — node tests it (TestGlyphRunMachine). Spliced into neGlyphScript
   right after fig; nothing here may touch the page beyond the figure's classes. */
var KX=(function(){
 var S={},R={},st=null,
 K=[['wv','wave circle off'],['tpa','picked ring: no animation'],['tp','picked ring off'],['hov','hover ring off'],['cur','cursor ring off'],['flt','no CSS filters in the map'],['nss','no non-scaling-stroke'],['mk','flag marks off'],['ap','tree canvas off'],['apop','tree marks: no fade'],['tr','no transitions in the map'],['an','no animations at all'],['wc','no will-change'],['ts','height scale off'],['pk','peek / notes off'],['pt','point marker + ping off'],['dock','card off'],['red','magenta behind the map (is the bar a hole?)'],['clip','seam: tiles unclipped (old)'],['pe0','seam: tiles clipped exactly'],['peq','seam: 1/4 px overlap'],['pe4','seam: 4 px overlap'],['add','seam: tiles added up (force)'],['noadd','seam: tiles not added up'],['hash','host: URL never rewritten (no replaceState)'],['push','host: no history entry on full screen'],['lock','host: page pinned without overflow:hidden'],['cap','input: no pointer capture'],['ta','input: touch-action pan-y in full screen'],['pd','input: touch listeners never preventDefault'],['hit','input: map content takes no hit test'],['ulive','undo: live transform during a gesture (NOLIVE off)'],['ufs','undo: Fullscreen API inside a WebView'],['u1','undo lite 1: tiles blended + supersampled'],['u2','undo lite 2: blur, filters, promoted crown canvas'],['u4','undo lite 4: tile pictures clipped + faded'],['u5','undo lite 5: animations in the map']],
 css='figure.kx-wv svg.glyph g.tp .wv{display:none!important}figure.kx-tpa svg.glyph g.tp,figure.kx-tpa svg.glyph g.tp *{animation:none!important}figure.kx-tp svg.glyph g.tp{display:none!important}figure.kx-hov svg.glyph .hov{display:none!important}figure.kx-cur svg.glyph .cur{display:none!important}figure.kx-flt svg.glyph,figure.kx-flt svg.glyph *{filter:none!important}figure.kx-nss svg.glyph *{vector-effect:none!important}figure.kx-mk svg.glyph .hi .ov-tree .ap use{display:none!important}figure.kx-ap canvas.glyph-ap{display:none!important}figure.kx-apop svg.glyph .hi .ov-tree .ap{transition:none!important;opacity:1!important}figure.kx-tr svg.glyph,figure.kx-tr svg.glyph *,figure.kx-tr canvas{transition:none!important}figure.kx-an,figure.kx-an *{animation:none!important}figure.kx-wc svg.glyph,figure.kx-wc canvas.glyph-ap{will-change:auto!important}figure.kx-ts .glyph-ts{display:none!important}figure.kx-pt svg.glyph g.pt,figure.kx-pt svg.glyph .ping{display:none!important}figure.kx-dock .glyph-tip{display:none!important}figure.kx-red .glyph-main{background:#f0f!important}figure.kx-pk .glyph-peek,figure.kx-pk .gt-n,figure.kx-pk .glyph-hint{display:none!important}figure.kx-clip svg.glyph .hi image{clip-path:none!important}figure.kx-pe0 svg.glyph{--pe:0px!important}figure.kx-peq svg.glyph{--pe:calc(.125px*var(--kxu,0))!important}figure.kx-pe4 svg.glyph{--pe:calc(4px*var(--kxu,0))!important}figure.kx-ta .glyph-main.js.zoomed{touch-action:pan-y!important}figure.kx-hit svg.glyph,figure.kx-hit svg.glyph *{pointer-events:none!important}';
 var paint=function(){
  var any=false;K.forEach(function(k){var v=!!(S[k[0]]||R[k[0]]);if(v)any=true;fig.classList.toggle('kx-'+k[0],v)});
  if((any||st)&&!st){st=document.createElement('style');st.textContent=css;document.head.appendChild(st)}
 };
 var m=/[?&]kx=([^&#]*)/.exec(location.search);
 (m?decodeURIComponent(m[1]).split(/[,.+ ]/):[]).forEach(function(k){if(k)S[k]=1});
 paint();
 return{K:K,
  on:function(k){return!!(S[k]||R[k])},
  set:function(k,v){if(v)S[k]=1;else delete S[k];paint()},
  /* the run's own set, replaced as a whole at every step (the URL's and the panel's switches stay) */
  run:function(list){R={};(list||[]).forEach(function(k){R[k]=1});paint()},
  css:function(){if(!st){st=document.createElement('style');st.textContent=css;document.head.appendChild(st)}},
  list:function(){var o=[];K.forEach(function(k){if(S[k[0]]||R[k[0]])o.push(k[0])});return o.join(',')||'-'}};
})();
/* ── run scenarios ── a scenario is a preamble of fixed steps and a list of switches to bisect.
   Every step: the page sets the view and the switches itself, plays a synthetic gesture (sub 'auto': script + paint,
   no input pipeline), then asks for real fingers (sub 'touch'). A step ends 'ok' (gestures done, page alive) or
   'died' (the page was reopened while the step was open and it had not been sent to the background).
   pre: [id, full screen, view] — bis: switches, host- and input-facing first. */
var KXRUN={touch:{far:'#v=obs',near:'#pt=14.34043,48.36932;v=obs;full;trees;at=4205,853,770',
  pre:[['inline-far',0,'far',0],['inline-near',0,'near',0],['fs-far',1,'far',0],['fs-near',1,'near',0],['fs-near-all',1,'near',1]],
  ref:'fs-near',all:'fs-near-all',
  bis:['hash','push','lock','cap','ta','pd','hit','ap','pt','mk','ts','tr','an','flt','wc','nss','dock']},
 /* mit — which of the mitigations in the build is the cure? inv: a step's list = the mitigations KEPT, the page runs with
    the undo switch of every other one (KX.run(bis minus list)). fs-near-kept = the build as shipped, fs-near-raw = every
    mitigation undone (the build that froze). raw survives → none is needed; bisect → the one that cures. */
 mit:{inv:1,far:'#v=obs',near:'#pt=14.34043,48.36932;v=obs;full;trees;at=4205,853,770',
  pre:[['fs-near-kept',1,'near',1],['fs-near-raw',1,'near',0]],ref:'fs-near-raw',all:'fs-near-kept',
  bis:['ulive','ufs','u4','u5','u2','u1']}};
/* the switches a step turns on: its list, or (inv) every switch of the scenario NOT on its list */
var kxRunSet=function(sc,c){return sc.inv?sc.bis.filter(function(k){return c.on.indexOf(k)<0}):c.on};
/* kxRunStep(sc, z, out) → z: the state after the open step ended with out ('ok' | 'died' | '' = unknown, repeat it).
   z = {ph:'pre'|'bis'|'confirm'|'done', i, lo, hi, cur:{id,fs,view,on:[…]}|null, res:{id:out}, log:[[id,on,out]], verdict}.
   Bisect: a step with the first n switches on; ok → the culprit is among them (hi=n), died → it is not (lo=n). */
var kxRunStep=function(sc,z,out){
 var N=sc.bis.length,pre=function(i){var p=sc.pre[i];return{id:p[0],fs:p[1],view:p[2],on:p[3]?sc.bis.slice():[]}},
  mid=function(){var n=Math.ceil((z.lo+z.hi)/2);return{id:'bisect-'+n,fs:1,view:'near',on:sc.bis.slice(0,n),n:n}};
 if(!z)return{ph:'pre',i:0,lo:0,hi:N,cur:pre(0),res:{},log:[],verdict:''};
 if(z.ph==='done'||!z.cur||!out)return z;
 z.log.push([z.cur.id,z.cur.on.join(','),out+(z.sub&&out==='died'?'@'+z.sub:'')]);
 if(z.ph==='pre'){z.res[z.cur.id]=out;z.i++;
  if(z.i<sc.pre.length)z.cur=pre(z.i);
  else if(z.res[sc.ref]==='ok'){z.ph='done';z.cur=null;z.verdict='no repro: '+sc.ref+' survived'+(sc.inv?' — no mitigation is needed':'')}
  else if(z.res[sc.all]!=='ok'){z.ph='done';z.cur=null;z.verdict=sc.inv?'not cured: '+sc.all+' died with every mitigation kept':'not a switch: '+sc.all+' died with every switch on'}
  else{z.ph='bis';z.lo=0;z.hi=N;z.cur=mid()}
 }else if(z.ph==='bis'){if(out==='ok')z.hi=z.cur.n;else z.lo=z.cur.n;
  if(z.hi-z.lo<=1){z.ph='confirm';z.cur={id:'confirm-'+sc.bis[z.lo],fs:1,view:'near',on:[sc.bis[z.lo]]}}else z.cur=mid()
 }else if(z.ph==='confirm'){z.ph='done';z.verdict=(sc.inv?'cure ':'culprit ')+sc.bis[z.lo]+(out==='ok'?' (alone it cures)':' (not alone: the step with only this switch died)');z.cur=null}
 return z};
