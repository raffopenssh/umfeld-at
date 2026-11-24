#!/usr/bin/env node
// glyph_net_probe.js — the landing glyph on a weak, lost or flapping network (docs/ne-glyph.md §Weak signal).
//
// An emulated phone (384×721 @2.8, CPU ×4) with the position watched and followed walks ~60 m at the deepest
// zoom with trees + photo on, taps a tree every third step and a chip in its card — once per network:
//   base     as the machine has it
//   offline  no network at all
//   slow     2.5 s latency, 3 kB/s; a tree report is answered after 9 s
//   flap     1.3 s offline / 1.3 s EDGE, alternating
// then the network comes back for 12 s. Tree POSTs are INTERCEPTED (CDP Fetch) and answered here — nothing
// reaches the relay or the peer. Per mode: longest main-thread gap, long tasks, requests, reports queued at the
// end and after the 12 s, distinct X-Idem ids. Exit 0 = no gap > 1 s, no stuck input, outbox empty, no JS error.
//
//   node scripts/glyph_net_probe.js [url] [modes]      env CHROME, PORT (9334), CPU (4), STEPS (24)
const {spawn}=require('child_process'),net=require('net'),crypto=require('crypto'),os=require('os'),fs=require('fs'),path=require('path');
const PORT=+(process.env.PORT||9334),CHROME=process.env.CHROME||'/headless-shell/headless-shell';
const sleep=ms=>new Promise(r=>setTimeout(r,ms));

// minimal RFC 6455 client: text frames, masking, fragmentation of incoming messages
function wsConnect(u){return new Promise((res,rej)=>{const m=/^ws:\/\/([^:/]+):(\d+)(\/.*)$/.exec(u),s=net.connect(+m[2],m[1]),key=crypto.randomBytes(16).toString('base64');
 let buf=Buffer.alloc(0),open=false,parts=[];const ws={onmessage:null,send(t){const p=Buffer.from(t),n=p.length,mask=crypto.randomBytes(4);let h;
   if(n<126)h=Buffer.from([0x81,0x80|n]);else if(n<65536){h=Buffer.alloc(4);h[0]=0x81;h[1]=0x80|126;h.writeUInt16BE(n,2)}else{h=Buffer.alloc(10);h[0]=0x81;h[1]=0x80|127;h.writeBigUInt64BE(BigInt(n),2)}
   const q=Buffer.alloc(n);for(let i=0;i<n;i++)q[i]=p[i]^mask[i&3];s.write(Buffer.concat([h,mask,q]))},close(){s.destroy()}};
 s.on('connect',()=>s.write('GET '+m[3]+' HTTP/1.1\r\nHost: '+m[1]+':'+m[2]+'\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: '+key+'\r\nSec-WebSocket-Version: 13\r\n\r\n'));
 s.on('error',rej);
 s.on('data',d=>{buf=Buffer.concat([buf,d]);
  if(!open){const i=buf.indexOf('\r\n\r\n');if(i<0)return;if(!/^HTTP\/1\.1 101/.test(buf.toString('latin1',0,i))){rej(new Error('no upgrade'));return}buf=buf.subarray(i+4);open=true;res(ws)}
  for(;;){if(buf.length<2)return;let n=buf[1]&127,o=2;if(n===126){if(buf.length<4)return;n=buf.readUInt16BE(2);o=4}else if(n===127){if(buf.length<10)return;n=Number(buf.readBigUInt64BE(2));o=10}
   if(buf.length<o+n)return;const fin=buf[0]&0x80,op=buf[0]&15,pl=buf.subarray(o,o+n);buf=buf.subarray(o+n);
   if(op===8){s.destroy();return}if(op===9||op===10)continue;parts.push(pl);if(fin){const t=Buffer.concat(parts).toString();parts=[];if(ws.onmessage)ws.onmessage(t)}}})})}

const url=process.argv[2]||'http://localhost:8000/#full;trees;ortho=1;relief;at=4270,1279,4000',modes=(process.argv[3]||'base,offline,slow,flap').split(',');
const CPU=+(process.env.CPU||4);
(async()=>{
 const prof=fs.mkdtempSync(path.join(os.tmpdir(),'glyph-net-'));
 const ch=spawn(CHROME,['--remote-debugging-port='+PORT,'--no-sandbox','--user-data-dir='+prof,'--touch-events=enabled','about:blank'],{stdio:'ignore'});
 let code=1;const bye=()=>{try{ch.kill()}catch(e){}try{fs.rmSync(prof,{recursive:true,force:true})}catch(e){}process.exit(code)};
 try{
  let t;for(let i=0;i<60&&!t;i++){try{t=(await(await fetch('http://127.0.0.1:'+PORT+'/json')).json()).find(x=>x.type==='page')}catch(e){}if(!t)await sleep(250)}
  const ws=await wsConnect(t.webSocketDebuggerUrl);let id=0;const pend={};let reqs=0;
  let netMode='base';const posts=[];
  ws.onmessage=m=>{const d=JSON.parse(m);if(d.id&&pend[d.id]){pend[d.id](d);delete pend[d.id]}else if(d.method==='Network.requestWillBeSent')reqs++;
   else if(d.method==='Fetch.requestPaused'){const p=d.params,rq=p.request;let b={};try{b=JSON.parse(rq.postData||'{}')}catch(e){}const idem=rq.headers['X-Idem']||rq.headers['x-idem']||'';posts.push({mode:netMode,idem,b});
    const f=b.reports?b.reports[0]:b,flag=f.claim&&f.claim!=='tree'&&f.claim!=='missing';
    const body=JSON.stringify(flag?{tally:{claims:{[f.claim]:1},n:1}}:{tree:{taxon:f.species||null,h_m:f.h_m||null,lon:f.lon,lat:f.lat,tree_id:f.tree_id||undefined,client_id:f.client_id||undefined,n:1,v:1}});
    const ok=()=>send('Fetch.fulfillRequest',{requestId:p.requestId,responseCode:201,responseHeaders:[{name:'Content-Type',value:'application/json'}],body:Buffer.from(body).toString('base64')});
    if(netMode==='offline'||netMode==='off')send('Fetch.failRequest',{requestId:p.requestId,errorReason:'InternetDisconnected'});else if(netMode==='slow')setTimeout(ok,9000);else if(netMode==='edge')setTimeout(ok,900);else ok()}};
  const send=(method,params={},to=20000)=>new Promise(res=>{const i=++id;pend[i]=res;ws.send(JSON.stringify({id:i,method,params}));setTimeout(()=>{if(pend[i]){delete pend[i];res({timeout:method})}},to)});
  const ev=async(e,to)=>{const r=await send('Runtime.evaluate',{expression:e,returnByValue:true,awaitPromise:true},to||20000);return r.timeout?'TIMEOUT':(r.result&&r.result.result?r.result.result.value:JSON.stringify(r).slice(0,300))};
  await send('Emulation.setDeviceMetricsOverride',{width:384,height:721,deviceScaleFactor:2.8125,mobile:true});
  await send('Emulation.setTouchEmulationEnabled',{enabled:true,maxTouchPoints:5});
  await send('Emulation.setUserAgentOverride',{userAgent:'Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Mobile Safari/537.36',userAgentMetadata:{brands:[{brand:'Chromium',version:'156'},{brand:'DuckDuckGo',version:'156'}],platform:'Android',platformVersion:'10',architecture:'',model:'K',mobile:true}});
  await send('Browser.grantPermissions',{permissions:['geolocation']});
  await send('Emulation.setGeolocationOverride',{latitude:48.039185,longitude:14.415233,accuracy:8});
  await send('Page.enable');await send('Network.enable');await send('Fetch.enable',{patterns:[{urlPattern:'*/api/v1/trees/report*',requestStage:'Request'}]});await send('Page.navigate',{url});
  for(let i=0;i<40;i++){await sleep(500);if(await ev("!!(window.neGlyph&&document.querySelector('svg.glyph'))")===true)break}
  await sleep(+process.env.SETTLE||7000);
  console.log('locate',await ev("(function(){var b=document.querySelector('.gv-loc');if(!b)return'no button';b.click();return 'clicked'})()"));await sleep(4000);console.log('state',await ev("JSON.stringify(window.neGlyph.locate?window.neGlyph.locate():null).slice(0,300)+' '+location.hash"));
  await ev(`(function(){var M=window.__m={gap:0,last:performance.now(),lt:0,ltn:0,ltmax:0,raf:0};setInterval(function(){var n=performance.now(),g=n-M.last;if(g>M.gap)M.gap=g;M.last=n},50);(function f(){M.raf++;requestAnimationFrame(f)})();try{new PerformanceObserver(function(l){l.getEntries().forEach(function(e){M.lt+=e.duration;M.ltn++;if(e.duration>M.ltmax)M.ltmax=e.duration})}).observe({entryTypes:['longtask']})}catch(e){}})()`);
  await send('Emulation.setCPUThrottlingRate',{rate:CPU});
  const T=(type,pts)=>send('Input.dispatchTouchEvent',{type,touchPoints:pts.map((p,i)=>({x:p[0],y:p[1],id:i,radiusX:22,radiusY:22,force:1}))},6000);
  const NET={base:{offline:false,latency:0,downloadThroughput:-1,uploadThroughput:-1},offline:{offline:true,latency:0,downloadThroughput:0,uploadThroughput:0},slow:{offline:false,latency:2500,downloadThroughput:3000,uploadThroughput:1500},edge:{offline:false,latency:800,downloadThroughput:30000,uploadThroughput:15000}};
  const setNet=n=>{netMode=n;return send('Network.emulateNetworkConditions',NET[n])};
  const pan=async(dx,dy)=>{const x=190,y=330;await T('touchStart',[[x,y]]);for(let i=1;i<=12;i++){await T('touchMove',[[x+dx*i/12,y+dy*i/12]]);await sleep(16)}await T('touchEnd',[])};
  const tap=async(x,y)=>{await T('touchStart',[[x,y]]);await sleep(50);await T('touchEnd',[])};
  const pinch=async(a,b)=>{const x=190,y=330;await T('touchStart',[[x-a,y]]);await T('touchStart',[[x-a,y],[x+a,y]]);for(let i=1;i<=20;i++){const d=a+(b-a)*i/20;await T('touchMove',[[x-d,y],[x+d,y]]);await sleep(16)}await T('touchEnd',[])};
  let bad=0;
  for(const mode of modes){
   await setNet('base');await sleep(1500);
   await ev("(function(){var M=window.__m;M.gap=0;M.lt=0;M.ltn=0;M.ltmax=0;M.raf=0;M.last=performance.now()})()");reqs=0;
   const t0=Date.now();let fl=null;
   if(mode==='flap'){let off=false;fl=setInterval(()=>{off=!off;setNet(off?'offline':'edge')},1300)}else await setNet(mode);
   let dead=0,lat=48.039185,lon=14.415233,taps=0,chips=0;
   for(let r=0;r<(+process.env.STEPS||24);r++){lat+=0.000018;lon+=0.000022;await send('Emulation.setGeolocationOverride',{latitude:lat,longitude:lon,accuracy:6});await sleep(900);
    if(r%3===1){const p=await ev("(function(){var g=window.neGlyph,m=document.querySelector('.glyph-main').getBoundingClientRect(),A=g.apices?g.apices():[],best=null;for(var i=0;i<A.length;i++){var a=A[i];var x=a.x,y=a.y;if(x>m.left+25&&x<m.right-25&&y>m.top+25&&y<m.bottom-25){best=[x,y];if(Math.random()<.2)break}}return JSON.stringify(best)})()");
     if(p&&p!=='null'&&p!=='TIMEOUT'){const q=JSON.parse(p);const x=await Promise.race([tap(q[0],q[1]),sleep(7000).then(()=>'T')]);if(x==='T')dead++;taps++;await sleep(500);
      const c=await ev("(function(){var b=document.querySelector('.glyph-tip .tr button[data-a=flag],.glyph-tip .tr button[data-a=sp]');if(!b)return'';var r=b.getBoundingClientRect();return JSON.stringify([r.left+r.width/2,r.top+r.height/2,b.dataset.a+':'+b.dataset.v])})()");
      if(c&&c[0]==='['){const q2=JSON.parse(c);await tap(q2[0],q2[1]);chips++}}}}
   console.log('  taps',taps,'chips',chips,'card:',String(await ev("(document.querySelector('.glyph-tip .tr .ft,.glyph-tip .tr .er,.glyph-tip .tr')||{textContent:''}).textContent.replace(/\\s+/g,' ').slice(-160)")));
   await sleep(4000);if(fl)clearInterval(fl);const pN=posts.length,qBefore=await ev("JSON.stringify((JSON.parse(localStorage.getItem('neg-ob')||'[]')).length)");await setNet('base');await sleep(12000);
   console.log('  outbox: queued at end of mode',qBefore,'after 12 s online',await ev("JSON.stringify((JSON.parse(localStorage.getItem('neg-ob')||'[]')).length)"),'posts in mode',posts.filter(p=>p.mode===mode||mode==='flap').length,'drained after',posts.length-pN,'distinct ids',new Set(posts.map(p=>p.idem)).size,'of',posts.length,'hint:',await ev("document.querySelector('.glyph-hint').textContent"));
   const s=await ev("(function(){var M=window.__m,g=window.neGlyph,L=g.log?g.log():{};return JSON.stringify({gap:Math.round(M.gap),lt:Math.round(M.lt),ltn:M.ltn,ltmax:Math.round(M.ltmax),raf:M.raf,err:(L.err||[]).slice(-3),net:g.net&&g.net(),hash:location.hash.slice(0,90)})})()",15000);
   console.log(mode.padEnd(8),'wall',((Date.now()-t0)/1000).toFixed(1)+'s','reqs',reqs,'stuckInput',dead,s);
   let J={};try{J=JSON.parse(s)}catch(e){}const left=+(await ev("JSON.stringify((JSON.parse(localStorage.getItem('neg-ob')||'[]')).length)"));if(s==='TIMEOUT'||dead||!(J.gap<=1000)||(J.err||[]).length||left){bad++;console.log('  FAIL',mode)}
  }
  code=bad?1:0;
 }catch(e){console.error(e)}bye()})();
