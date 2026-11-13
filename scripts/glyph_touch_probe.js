#!/usr/bin/env node
// glyph_touch_probe.js — drive the landing glyph with REAL touch input (docs/ne-glyph.md §Field diagnostics).
//
// Events dispatched from page script (PointerEvent in an eval) reach only the page's handlers. This probe sends
// touches through Chromium's input pipeline (CDP Input.dispatchTouchEvent on an emulated phone): gesture
// detection, touch-action, non-passive listeners, touch adjustment, hit tests, pointer capture all take part.
// It is the desktop half of a phone bug hunt — what dies here is Blink; what dies only on the phone is the
// device's GPU, its WebView or the host app.
//
//   node scripts/glyph_touch_probe.js [url] [mode]
//     url   default http://localhost:8000/?trace#pt=14.34043,48.36932;v=obs;full;trees;screen;at=4205,853,770
//     mode  all (default) = hold · pan · pinch out · pinch in · tap, the page must answer after each
//           run  = the url carries ?run=<scenario>: gestures are made whenever the page asks for fingers,
//                  until it reports "finished"; prints the run's log (an end-to-end test of the run driver)
//   env CHROME (default /headless-shell/headless-shell), PORT (9333)
// No dependencies: Node ≥ 18, a 60-line WebSocket client below. Exit 0 = every step answered, no JS error.
const {spawn}=require('child_process'),net=require('net'),crypto=require('crypto'),os=require('os'),fs=require('fs'),path=require('path');
const url=process.argv[2]||'http://localhost:8000/?trace#pt=14.34043,48.36932;v=obs;full;trees;screen;at=4205,853,770',mode=process.argv[3]||'all';
const PORT=+(process.env.PORT||9333),CHROME=process.env.CHROME||'/headless-shell/headless-shell';
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

(async()=>{
 const prof=fs.mkdtempSync(path.join(os.tmpdir(),'glyph-probe-'));
 const ch=spawn(CHROME,['--remote-debugging-port='+PORT,'--no-sandbox','--user-data-dir='+prof,'--touch-events=enabled','about:blank'],{stdio:'ignore'});
 let code=1;const bye=()=>{try{ch.kill()}catch(e){}try{fs.rmSync(prof,{recursive:true,force:true})}catch(e){}process.exit(code)};
 try{
  let t;for(let i=0;i<60&&!t;i++){try{t=(await(await fetch('http://127.0.0.1:'+PORT+'/json')).json()).find(x=>x.type==='page')}catch(e){}if(!t)await sleep(250)}
  if(!t)throw new Error('no browser on port '+PORT);
  const ws=await wsConnect(t.webSocketDebuggerUrl);let id=0;const pend={};
  ws.onmessage=m=>{const d=JSON.parse(m);if(d.id&&pend[d.id]){pend[d.id](d);delete pend[d.id]}};
  const send=(method,params={},to=8000)=>new Promise(res=>{const i=++id;pend[i]=res;ws.send(JSON.stringify({id:i,method,params}));setTimeout(()=>{if(pend[i]){delete pend[i];res({timeout:method})}},to)});
  const ev=async(e,to)=>{const r=await send('Runtime.evaluate',{expression:e,returnByValue:true,awaitPromise:true},to||8000);return r.timeout?'TIMEOUT':(r.result&&r.result.result?r.result.result.value:undefined)};
  await send('Emulation.setDeviceMetricsOverride',{width:384,height:721,deviceScaleFactor:2.8125,mobile:true});
  await send('Emulation.setTouchEmulationEnabled',{enabled:true,maxTouchPoints:5});
  await send('Emulation.setUserAgentOverride',{userAgent:'Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Mobile Safari/537.36',userAgentMetadata:{brands:[{brand:'Chromium',version:'156'},{brand:'DuckDuckGo',version:'156'}],platform:'Android',platformVersion:'10',architecture:'',model:'K',mobile:true}});
  await send('Page.enable');await send('Page.navigate',{url});
  for(let i=0;i<40;i++){await sleep(500);if(await ev("!!(window.neGlyph&&document.querySelector('svg.glyph'))")===true)break}
  const T=(type,pts)=>send('Input.dispatchTouchEvent',{type,touchPoints:pts.map((p,i)=>({x:p[0],y:p[1],id:i,radiusX:22,radiusY:22,force:1}))},6000);
  const box=async()=>JSON.parse(await ev("(function(){var b=document.querySelector('svg.glyph').getBoundingClientRect(),m=document.querySelector('.glyph-main').getBoundingClientRect();return JSON.stringify({x:m.left+m.width*.5,y:Math.max(m.top+40,Math.min(innerHeight-60,m.top+m.height*.4))})})()"));
  let bad=0;const alive=async l=>{const v=await ev("new Promise(r=>requestAnimationFrame(()=>r('alive z='+(window.neGlyph.state().z||'')+' '+location.hash.slice(-60))))",6000);console.log((l+'        ').slice(0,10),v);if(!/^alive/.test(String(v)))bad++};
  const pan=async r=>{await T('touchStart',[[r.x,r.y]]);for(let i=1;i<=24;i++){await T('touchMove',[[r.x-i*3,r.y+i*3]]);await sleep(16)}await T('touchEnd',[])};
  const pinch=async(r,a,b)=>{await T('touchStart',[[r.x-a,r.y]]);await T('touchStart',[[r.x-a,r.y],[r.x+a,r.y]]);for(let i=1;i<=30;i++){const d=a+(b-a)*i/30;await T('touchMove',[[r.x-d,r.y],[r.x+d,r.y+i/2]]);await sleep(16)}await T('touchEnd',[])};
  if(mode==='run'){
   let lastSay='';for(let i=0;i<400;i++){const s=String(await ev("(function(){var z=window.neGlyphTrace&&window.neGlyphTrace.run();return z?(z.ph+'|'+(z.cur?z.cur.id:'')+'|'+(z.sub||'')+'|'+z.verdict):'?'})()"));
    if(s!==lastSay){console.log(s);lastSay=s}
    if(/^done/.test(s))break;
    if(/\|touch\|/.test(s)){const r=await box();await pan(r);await sleep(300);await pinch(r,30,110);await sleep(300);await pinch(r,110,40);await sleep(300);await pan(r);await sleep(2400)}else await sleep(500)}
   console.log('log',await ev("JSON.stringify(window.neGlyphTrace.run().log)"));
   if(!/^done/.test(lastSay))bad++;
  }else{
   await sleep(6000);console.log('state',await ev("JSON.stringify({fig:document.querySelector('figure.glyph').className,imgs:document.querySelectorAll('svg.glyph .hi image').length})"));
   const r=await box();
   await T('touchStart',[[r.x,r.y]]);await alive('down');await sleep(1200);await alive('hold');await T('touchEnd',[]);await alive('up');
   await pan(r);await sleep(400);await alive('pan');
   await pinch(r,30,120);await sleep(500);await alive('pinch out');
   await pinch(r,120,40);await sleep(500);await alive('pinch in');
   await T('touchStart',[[r.x,r.y]]);await sleep(60);await T('touchEnd',[]);await sleep(900);await alive('tap');
  }
  const err=await ev("JSON.stringify(window.neGlyph.log().errors)");console.log('errors',err);if(err!=='[]')bad++;
  code=bad?1:0;console.log(bad?'FAIL':'PASS');
 }catch(e){console.error(String(e&&e.stack||e))}
 bye()})();
