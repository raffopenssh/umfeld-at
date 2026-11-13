/* ne_glyph_trace.js — the glyph's field recorder and run driver (docs/ne-glyph.md §Field diagnostics).
   ?trace  records a session on a device we cannot hold: layout samples, every input event, stalls, frame gaps,
           an env row and a state snapshot; three ways out (timed parts, watchdog Worker, carry-over of a dead session).
   ?run=X  (implies ?trace) the page drives scenario X itself (KXRUN in ne_glyph_kx.js): per step it sets view and
           switches, plays a synthetic gesture, asks for real fingers, and posts the outcome; a page that dies is
           reopened on the same link and carries on with the next step.
   Spliced into neGlyphScript; reads the page's state, changes it only through fsApply/fsLeave/applyHash/KX.run. */
if(TRACE){(function(){
var T0=Date.now(),SID=Math.random().toString(36).slice(2,8),FB='/api/v1/feedback?trace=1',LSK='neg-trace',
 rows=[],wq=[],cnt=0,sentCnt=0,part=0,n=0,last='',rafT=0,wd=null,RUN=null,GPU='';
var tNow=function(){return Math.round((Date.now()-T0)/10)/100};
var P=function(x){rows.push(x);wq.push(x);cnt++;if(rows.length>500)rows.shift()};
var R=function(e){if(!e)return'-';var r=e.getBoundingClientRect();return Math.round(r.top)+':'+Math.round(r.bottom)+':'+Math.round(r.width)};
var card=function(){return tip.className.replace('glyph-tip','').trim()};
try{GPU=localStorage.getItem('neg-gpu')||''}catch(e){}

/* ── env: what the device and its browser are, once per record ── */
var env=function(){var mm=function(q){try{return matchMedia(q).matches?1:0}catch(e){return-1}},vm=document.querySelector('meta[name=viewport]'),ov='';
 try{ov=getComputedStyle(document.documentElement).overflowY+'/'+getComputedStyle(document.body).overflowY}catch(e){}
 return{sid:SID,lite:LITE?1:0,wv:WV?1:0,nolive:NOLIVE?1:0,kx:KX.list(),brands:UAB,dpr:window.devicePixelRatio,scr:screen.width+'x'+screen.height,inner:innerWidth+'x'+innerHeight,
  tp:navigator.maxTouchPoints||0,mem:navigator.deviceMemory||0,hc:navigator.hardwareConcurrency||0,hover:mm('(hover:hover)'),fine:mm('(pointer:fine)'),rm:mm('(prefers-reduced-motion:reduce)'),
  standalone:mm('(display-mode:standalone)'),vp:vm?vm.content:'',ovf:ov,gpu:GPU,q:location.search.slice(0,120)}};
/* ── state: where the page is right now; saved with every heartbeat, so a dead session says where it died ── */
var inp={k:'',t:0,map:0},fsWas=false,fsT=T0;
var state=function(){var now=Date.now();if(fsOn!==fsWas){fsWas=fsOn;fsT=now}
 return{fs:fsOn?(fsNative?'native':'css'):'',fs_age:Math.round((now-fsT)/100)/10,z:vb?Math.round(W/vb.w):0,hi:hi&&hi.on?1:0,left:hi&&hi.on?hi.left:-1,ptrs:order.length,pinch:pinch?1:0,
  card:card(),vis:document.visibilityState,inp:inp.k,in_age:inp.t?Math.round((now-inp.t)/100)/10:-1,map_downs:inp.map,kx:KX.list(),run:RUN?RUN.where():''}};

/* ── layout sampler: every 120 ms, kept when it changed ── */
var samp=function(){var imgs=svg.querySelectorAll('image'),vis=0,rlc=0;for(var i=0;i<imgs.length;i++){var im=imgs[i];if(im.style.display==='none'||!im.getAttribute('href'))continue;vis++;if(im.closest&&im.closest('.ov-rel'))rlc++}
 var vv=window.visualViewport,fl=fig.querySelector('.glyph-filt'),hd=tip.querySelector('.hd');
 var x=[R(svg),R(tip),card(),R(tools),R(fl),fig.querySelectorAll('.gf-c').length+'/'+[].map.call((fl||{children:[]}).children,function(c){return c.className.split(' ')[0].replace('gf-','')+Math.round(c.getBoundingClientRect().height)+(c.classList.contains('gf-n')?'"'+c.textContent.replace(/\s+/g,' ').slice(0,24)+'"':'')}).join('+'),
  vis,rlc,imgs.length,vb?Math.round(vb.w)+'/'+Math.round(vb.h):'-',hi&&hi.on?hi.left:-1,typeof rl==='object'&&rl?rl.inflight+rl.q.length:-1,fsOn?1:0,innerWidth+'x'+innerHeight,vv?Math.round(vv.height):-1,window.devicePixelRatio,document.visibilityState.charAt(0),
  hd&&hd.textContent?String(hd.textContent).replace(/\s+/g,' ').slice(0,60):''].join(' | ');
 n++;if(x!==last){last=x;P(tNow()+' '+x)}};

/* ── records ── one builder for every way out; ?trace=1 = the recorder's own write budget (api_feedback.go) */
var fresh=function(){var k=Math.min(rows.length,cnt-sentCnt);sentCnt=cnt;return rows.slice(rows.length-k)};
var post=function(title,ctx,out,beacon){ctx.rows=out;ctx.samples=n;if(!ctx.sid)ctx.sid=SID;ctx.ua=navigator.userAgent;ctx.mem=navigator.deviceMemory||0;ctx.hc=navigator.hardwareConcurrency||0;if(!ctx.fetch)ctx.fetch={};
 var body={agent:'glyph-trace',category:'other',severity:'low',title:(title+' '+(navigator.userAgent||'')).slice(0,100),description:location.hash.slice(0,500),endpoint:location.pathname,context:ctx};
 while(JSON.stringify(body).length>60000&&out.length>20)out.splice(0,Math.ceil(out.length/4));
 var s=JSON.stringify(body);
 try{if(beacon&&navigator.sendBeacon&&navigator.sendBeacon(FB,new Blob([s],{type:'application/json'})))return}catch(e){}
 try{_fetch(FB,{method:'POST',headers:{'Content-Type':'application/json'},body:s,keepalive:s.length<60000}).catch(function(){})}catch(e){}};
/* parts: the first after 45 s, then every 30 s whatever is new, 12 at most, and when the tab is hidden; a run posts per step instead */
var iv=0,sv=0;
var send=function(why){var k=Math.min(rows.length,cnt-sentCnt);if(part>=12||(part&&!k))return;part++;if(part>=12||Date.now()-T0>36e4){clearInterval(iv);clearInterval(sv)}
 post('trace '+(part>1?'+'+(part-1):''),{part:part,dbg:dbgL.slice(-150),err:tel.err.slice(0,20),fetch:tel.n,env:env(),st:state()},fresh(),true)};

/* ── carry-over + heartbeat ── once a second ALWAYS (alive = the main thread's last tick, raf = the last animation
   frame: alive running on while raf stands still = compositor/GPU hung; both stopping together = the whole renderer
   or the host app) and AT ONCE after every input row and every trMark. A hidden tab clears it; what is left at the
   next load is the session that died → sent as "trace prev" with the state it died in. */
var save=function(){try{localStorage.setItem(LSK,JSON.stringify({t:Date.now(),sid:SID,alive:tNow(),raf:rafT,h:location.hash,rows:rows.slice(-160),dbg:dbgL.slice(-40),err:tel.err.slice(0,10),st:state(),env:env()}))}catch(e){}};
trMark=function(x){P(tNow()+' '+x);save()};
try{var pv=localStorage.getItem(LSK);if(pv){localStorage.removeItem(LSK);var pj=JSON.parse(pv);if(pj&&pj.rows&&pj.rows.length&&Date.now()-pj.t<864e5){
 var pb={agent:'glyph-trace',category:'other',severity:'low',title:('trace prev '+(navigator.userAgent||'')).slice(0,100),description:String(pj.h||'').slice(0,500),endpoint:location.pathname,
  context:{part:'prev',sid:pj.sid||'',next_sid:SID,alive_s:pj.alive||0,last_raf_s:pj.raf||0,died_s_ago:Math.round((Date.now()-pj.t)/1000),st:pj.st||null,env:pj.env||null,rows:pj.rows,dbg:pj.dbg||[],err:pj.err||[],fetch:{},ua:navigator.userAgent,mem:navigator.deviceMemory||0,hc:navigator.hardwareConcurrency||0,samples:pj.rows.length}};
 _fetch(FB,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(pb),keepalive:true}).catch(function(){})}}}catch(e){}

/* ── watchdog Worker ── gets every row and a heartbeat; main thread silent ≥ 3 s with the tab in front → it posts
   the record itself ("trace hang"). 'wd ready' in the rows = the Worker runs on this device; ?hangtest proves the path. */
try{var wsrc="var rows=[],dbg=[],last=Date.now(),n=0,meta={},hung=0;postMessage('ready');onmessage=function(e){var d=e.data;if(hung){rows.push(d.t+' main thread back after '+(Date.now()-hung)+'ms');hung=0}last=Date.now();if(d.m)meta=d.m;if(d.r&&d.r.length){rows=rows.concat(d.r);if(rows.length>300)rows=rows.slice(-300)}if(d.d)dbg=d.d};setInterval(function(){var gap=Date.now()-last;if(hung||gap<3000||n>=3||meta.hidden||!meta.o)return;hung=last;n++;var out=rows.slice();out.push('HANG: main thread silent for '+gap+'ms');var body={agent:'glyph-trace',category:'other',severity:'low',title:('trace hang '+String(meta.ua||'')).slice(0,100),description:String(meta.hash||'').slice(0,500),endpoint:'/',context:{part:'hang',sid:meta.sid,silent_ms:gap,st:meta.st,env:meta.env,rows:out,dbg:dbg,err:[],fetch:{},ua:meta.ua,mem:meta.mem,hc:meta.hc,samples:out.length}};while(JSON.stringify(body).length>60000&&out.length>20)out.splice(0,Math.ceil(out.length/4));fetch(meta.o+'"+FB+"',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).catch(function(){})},1000)";
 wd=new Worker(URL.createObjectURL(new Blob([wsrc],{type:'text/javascript'})));wd.onmessage=function(){P(tNow()+' wd ready')};wd.onerror=function(){P(tNow()+' wd error');wd=null}}catch(e){wd=null;P('0 wd none')}
var meta=function(){return{o:location.origin,sid:SID,hash:location.hash,ua:navigator.userAgent,mem:navigator.deviceMemory||0,hc:navigator.hardwareConcurrency||0,hidden:document.visibilityState==='hidden',st:state(),env:env()}};
var bT=0,bFs=0,scN=0,hN=0;
var beat=function(now){var m=null,d=null;if(now-bT>=1000){bT=now;m=meta();d=dbgL.slice(-60);
  if(fsN-bFs>3)P(tNow()+' fsFit x'+(fsN-bFs)+'/s');bFs=fsN;
  if(scN){P(tNow()+' scroll x'+scN+' y='+Math.round(window.scrollY));scN=0}
  if(hN){P(tNow()+' history x'+hN+' '+location.hash.slice(-48));hN=0}
  if(RUN)RUN.tick(now);save()}
 if(wd){try{wd.postMessage({t:tNow(),r:wq,m:m,d:d})}catch(e){}}wq=[]};
var lastT=Date.now();
/* a sampler that fires late = the main thread was busy: the gap is the stall */
var samp2=function(){var now=Date.now(),gap=now-lastT;lastT=now;if(gap>400)P(tNow()+' stall '+gap+'ms');var c0=cnt;samp();beat(now);if(cnt!==c0&&order.length)save()};
try{if(window.PerformanceObserver){new PerformanceObserver(function(l){l.getEntries().forEach(function(en){if(en.duration>=100)P(Math.round((performance.timeOrigin+en.startTime-T0)/10)/100+' longtask '+Math.round(en.duration)+'ms')})}).observe({entryTypes:['longtask']})}}catch(e){}

/* ── input: every event of a tap or gesture with what was hit; saved at once (the row before a freeze must survive).
   synth = dispatched by a script (the run's auto pass), not by the device. move1 = a pointer's first move. ── */
var mv={};
var nmEl=function(el){return el&&el.tagName?el.tagName.toLowerCase()+(el.className&&typeof el.className==='string'?'.'+el.className.split(' ').slice(0,2).join('.'):''):'?'};
['pointerdown','pointerup','pointercancel','click','contextmenu','gotpointercapture','lostpointercapture'].forEach(function(t){document.addEventListener(t,function(e){
 var id=e.pointerId,x=tNow()+' '+t+' '+nmEl(e.target)+' @'+Math.round(e.clientX)+','+Math.round(e.clientY)+(id!==undefined?' id='+id+(e.pointerType?' '+e.pointerType:''):'')+(e.isTrusted?'':' synth');
 if(t==='pointerdown'){mv[id]=0;if(e.isTrusted&&main.contains(e.target))inp.map++;x+=' card='+card()}
 else if(t==='pointerup'||t==='pointercancel'){x+=' moves='+(mv[id]||0);delete mv[id]}
 else if(t==='click')x+=' card='+card()+(e.defaultPrevented?' prevented':'');
 if(e.isTrusted){inp.k=t+' '+nmEl(e.target);inp.t=Date.now()}
 P(x);save()},{capture:true,passive:true})});
document.addEventListener('pointermove',function(e){var id=e.pointerId;if(mv[id]===undefined)return;if(mv[id]++===0){P(tNow()+' move1 id='+id+' @'+Math.round(e.clientX)+','+Math.round(e.clientY)+(e.isTrusted?'':' synth'));if(e.isTrusted)save()}},{capture:true,passive:true});
['touchstart','touchend','touchcancel'].forEach(function(t){document.addEventListener(t,function(e){inp.k=t+' n='+e.touches.length;inp.t=Date.now();P(tNow()+' '+t+' n='+e.touches.length+' '+nmEl(e.target)+(e.cancelable?'':' passive'));save()},{capture:true,passive:true})});
window.addEventListener('scroll',function(){scN++},{passive:true});
try{['replaceState','pushState'].forEach(function(k){var f=history[k];history[k]=function(){hN++;return f.apply(history,arguments)}})}catch(e){}
['fullscreenchange','webkitfullscreenchange'].forEach(function(t){document.addEventListener(t,function(){trMark(t+' el='+(document.fullscreenElement||document.webkitFullscreenElement?1:0)+' fsOn='+(fsOn?1:0)+' '+innerWidth+'x'+innerHeight)})});
window.addEventListener('resize',function(){P(tNow()+' resize '+innerWidth+'x'+innerHeight+(window.visualViewport?' vv='+Math.round(visualViewport.height):''))});
window.addEventListener('popstate',function(){P(tNow()+' popstate '+location.hash.slice(-40))});
document.addEventListener('visibilitychange',function(){var h=document.visibilityState==='hidden';P(tNow()+' visibility '+document.visibilityState);
 if(wd)try{wd.postMessage({t:tNow(),m:meta()})}catch(e){}
 if(RUN)RUN.vis(h);
 if(h){send();try{localStorage.removeItem(LSK)}catch(e){}}});

/* ── frames: a gap between two animation frames while the tab is in front = nothing was painted that long ── */
var fT=performance.now();
var fr=function(){var t=performance.now();if(t-fT>300&&document.visibilityState==='visible')P(tNow()+' frame '+Math.round(t-fT)+'ms z='+(vb?(W/vb.w).toFixed(0):'-')+' imgs='+svg.querySelectorAll('image').length+(pinch?' pinch':'')+(typeof hi==='object'&&hi&&hi.on?' left='+hi.left:''));fT=t;rafT=tNow();if(Date.now()-T0<36e4||RUN)requestAnimationFrame(fr)};
fr();

/* ── synthetic gesture: a drag, then a pinch out and back, as pointer events on the map — the page's own gesture
   code and the paint it causes, without the device's input pipeline. A freeze here is script or paint; a freeze
   only under real fingers is input handling (browser or host app). ── */
var synth=function(done){var b=svg.getBoundingClientRect(),cx=b.left+b.width/2,cy=b.top+b.height/2,i=0;
 var pe=function(t,id,x,y,prim){main.dispatchEvent(new PointerEvent(t,{pointerId:id,pointerType:'touch',isPrimary:prim,clientX:x,clientY:y,bubbles:true,cancelable:true,buttons:t==='pointerup'?0:1}))};
 pe('pointerdown',9101,cx,cy,true);
 var f=function(){i++;var d;
  if(i<=20)pe('pointermove',9101,cx-i*3,cy+i*2,true);
  else if(i===21){pe('pointerup',9101,cx-60,cy+40,true);pe('pointerdown',9102,cx-30,cy,true);pe('pointerdown',9103,cx+30,cy,false)}
  else if(i<=50){d=30+(i-21)*3;pe('pointermove',9102,cx-d,cy,true);pe('pointermove',9103,cx+d,cy,false)}
  else if(i<=75){d=117-(i-50)*3;pe('pointermove',9102,cx-d,cy,true);pe('pointermove',9103,cx+d,cy,false)}
  else{pe('pointerup',9102,cx-42,cy,true);pe('pointerup',9103,cx+42,cy,false);if(done)setTimeout(done,900);return}
  requestAnimationFrame(f)};
 requestAnimationFrame(f)};

/* ── run driver ── */
var rm=/[?&]run=([a-z]+)/.exec(location.search);
if(rm&&KXRUN[rm[1]]){RUN=(function(){
 var sc=KXRUN[rm[1]],RK='neg-run',rid=rm[1]+':'+((/[?&]r=([^&#]*)/.exec(location.search)||[])[1]||''),z=null,g=null,act={},TOUCH=(navigator.maxTouchPoints||0)>0,
  ban=document.createElement('div');
 ban.style.cssText='position:fixed;top:0;left:0;right:0;z-index:200;font:13px/1.35 system-ui,sans-serif;background:#111;color:#fff;padding:calc(5px + env(safe-area-inset-top)) 10px 5px;text-align:center;pointer-events:none';
 document.body.appendChild(ban);
 var say=function(s,c){ban.textContent=s;ban.style.background=c||'#111'};
 var store=function(){try{localStorage.setItem(RK,JSON.stringify({rid:rid,z:z}))}catch(e){}};
 var report=function(kind){post('trace run '+kind+' '+rid,{part:'run',run:{rid:rid,kind:kind,inv:sc.inv?1:0,ph:z.ph,lo:z.lo,hi:z.hi,cur:z.cur?z.cur.id:'',sub:z.sub||'',res:z.res,log:z.log,verdict:z.verdict},dbg:dbgL.slice(-60),err:tel.err.slice(0,20),fetch:tel.n,env:env(),st:state()},fresh(),false)};
 /* view(c): the step's switches, view and full screen. A step that undoes the WebView full-screen mitigation (ufs) needs
    the Fullscreen API, and that needs a tap: the banner becomes the button (sub 'tap'). again = after the auto pass:
    a native full screen is kept (a second tap would be needed), the CSS one is left and re-entered as before. */
 var goHash=function(h){try{history.replaceState(null,'',location.pathname+location.search+h)}catch(e){}applyHash()};
 var view=function(c,again){var set=kxRunSet(sc,c),keep=again&&fsOn&&fsNative;if(fsOn&&!keep)fsLeave();
  if(!again){KX.run(set);if(sc.inv){if(typeof apCv!=='undefined'&&apCv){apCv.remove();apCv=null}goHash(sc.far)}}
  window.scrollTo(0,0);goHash(sc[c.view]);
  if(c.fs&&!fsOn){if(KX.on('ufs')&&!again){z.sub='tap';store();trMark('run tap: waiting for the full-screen tap');ban.style.pointerEvents='auto';say('step '+(z.log.length+1)+' · TAP THIS BAR to go on','#8a4b00');
    ban.onclick=function(){ban.onclick=null;ban.style.pointerEvents='none';fsToggle();say('step '+(z.log.length+1)+' · loading …')}}
   else fsApply(true)}
  if(!c.fs&&fsOn)fsLeave()};
 var settled=function(c){if(c.fs&&!fsOn)return false;return c.view==='near'?!!(hi&&hi.on&&hi.left===0):true};
 var finish=function(){trMark('run done: '+z.verdict);var m=/^culprit (\w+)/.exec(z.verdict);KX.run(m?[m[1]]:[]);say('diagnostic finished — thank you. You can close this page.','#0a6b3d');report('done')};
 var touch=function(){z.sub='touch';store();g={downs:0,pinch:0,ms:0,t0:0,idle:0};trMark('run touch: waiting for fingers');
  say('step '+(z.log.length+1)+' · now drag the map AND pinch it a few times'+(TOUCH?'':' (mouse: drag 3 times)'))};
 var auto=function(c){z.sub='auto';store();trMark('run auto: synthetic drag + pinch');say('step '+(z.log.length+1)+' · automatic part, hands off …');
  synth(function(){trMark('run auto done');view(c,true);setTimeout(touch,1500)})};
 var begin=function(){var c=z.cur;if(!c){finish();return}
  z.open=1;z.away=document.visibilityState==='hidden'?1:0;z.sub='setup';g=null;store();
  trMark('run step '+c.id+' fs='+c.fs+' view='+c.view+(sc.inv?' kept=':' on=')+(c.on.join(',')||'-')+(sc.inv?' kx='+(kxRunSet(sc,c).join(',')||'-'):''));say('step '+(z.log.length+1)+' · loading …');view(c);
  var t0=Date.now(),w=setInterval(function(){if(c.fs&&!fsOn){t0=Date.now();return}if(settled(c)||Date.now()-t0>9000){clearInterval(w);trMark('run settled '+(Date.now()-t0)+'ms hi='+(hi&&hi.on?1:0));setTimeout(function(){auto(c)},600)}},300)};
 var end=function(out){g=null;z.open=0;trMark('run step '+z.cur.id+' → '+out);z=kxRunStep(sc,z,out);z.sub='';store();report('step');say('step done ✓','#0a6b3d');setTimeout(begin,1200)};
 /* real gestures: trusted pointer events on the map; enough = 3 touches, one of them with two fingers, 2.5 s of contact */
 main.addEventListener('pointerdown',function(e){if(!g||!e.isTrusted)return;act[e.pointerId]=1;var k=Object.keys(act).length;g.downs++;if(k>=2)g.pinch=1;if(k===1)g.t0=Date.now();g.idle=0},true);
 ['pointerup','pointercancel'].forEach(function(t){window.addEventListener(t,function(e){if(!e.isTrusted||!act[e.pointerId])return;delete act[e.pointerId];if(g&&!Object.keys(act).length){g.ms+=Date.now()-g.t0;g.idle=Date.now()}},true)});
 try{var j=JSON.parse(localStorage.getItem(RK)||'null');if(j&&j.rid===rid)z=j.z}catch(e){}
 if(z&&z.cur&&z.open){var out=z.away?'':'died';P('0 run resume: step '+z.cur.id+' sub='+(z.sub||'')+' was open → '+(out||'unknown (the page was in the background), repeating'));z.open=0;z=kxRunStep(sc,z,out);z.sub='';store();setTimeout(function(){report('resume')},1500)}
 else if(!z){z=kxRunStep(sc,null);P('0 run start '+rid)}
 setTimeout(begin,2500);
 return{where:function(){return z.cur?z.cur.id+'/'+(z.sub||''):z.ph},
  vis:function(h){if(z.open){z.away=h?1:0;store()}},
  tick:function(now){if(!g||z.sub!=='touch')return;var ok=g.downs>=3&&(g.pinch||!TOUCH)&&g.ms>=(TOUCH?2500:600);
   if(Object.keys(act).length)return;
   if(ok&&g.idle&&now-g.idle>1500){end('ok');return}
   say('step '+(z.log.length+1)+' · drag '+(g.downs>=3?'✓':g.downs+'/3')+(TOUCH?' · pinch '+(g.pinch?'✓':'…')+' · '+(g.ms>=2500?'✓':'keep going'):''))},
  state:function(){return z}}})()}

iv=setInterval(samp2,120);samp();
P('0 env '+JSON.stringify(env()));
P('0 paint lite='+(LITE?1:0)+' wv='+(WV?1:0)+' nolive='+(NOLIVE?1:0)+' kx='+KX.list()+' brands='+UAB);
if(!RUN)setTimeout(function(){send();sv=setInterval(send,30000)},45000);
/* the GPU's name, once per device (a WebGL context is made and dropped; marked first, in case that itself is the trouble) */
if(!GPU)setTimeout(function(){try{trMark('gpu probe');var c=document.createElement('canvas'),gl=c.getContext('webgl'),s='none';if(gl){var x=gl.getExtension('WEBGL_debug_renderer_info');s=String(x?gl.getParameter(x.UNMASKED_RENDERER_WEBGL):gl.getParameter(gl.RENDERER)).slice(0,90);var l=gl.getExtension('WEBGL_lose_context');if(l)l.loseContext()}GPU=s;localStorage.setItem('neg-gpu',s);trMark('gpu '+s)}catch(e){GPU='err'}},RUN?400:3000);
/* ?hangtest: block the main thread for 6 s, 8 s in — a "trace hang" record must arrive, or the watchdog does not work on this device */
if(/[?&]hangtest\b/.test(location.search))setTimeout(function(){trMark('hangtest: blocking 6 s');var t=Date.now();while(Date.now()-t<6000){}trMark('hangtest: back')},8000);
window.neGlyphTrace={rows:rows,send:send,sid:SID,env:env,state:state,synth:synth,run:function(){return RUN?RUN.state():null}}})()}
