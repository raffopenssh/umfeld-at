package main

// ne_glyph_outbox.go — the tree outbox: every tree report (species, measure, state, note, flag, add,
// undo, withdraw) is written to the device first and sent in the background, in order, until the
// relay answers (docs/ne-glyph.md §Weak signal).
//
// neGlyphOutboxCoreJS is pure (TestGlyphOutbox runs it under node with a fake clock and a fake
// network): a FIFO with one request in flight, persisted through io.save on every change.
//
//	put(it, {done, park})  it = {b: the POST body, k: the tree's key, lon, lat}; gets id (= X-Idem), t, n
//	an answer that is final (anything but 429 / ≥ 500)  → off the queue, done(x, it) — or io.late(it, x)
//	                                                      when the page that sent it is gone (reload)
//	no answer / 429 / 5xx → the item stays, is PARKED (park(it), io.park(it): the card stops waiting),
//	                        and is sent again after Retry-After or 2 s · 2^n (≤ 60 s); kick() = now
//	                        (the browser's online event, the tab coming back)
//	OB_SOFT ms without an answer parks the item while the request is still out (a slow link)
//	offline (io.online() false) parks without a request
//	an item ≥ OB_TRIES answered refusals (429/5xx) is delivered as that refusal; one older than
//	OB_AGE is dropped (io.drop) — never a queue that blocks for ever behind one poisoned item
//	discard(k) removes the parked, not-in-flight items of one tree (the card's "discard")
//
// The same id goes out on every attempt: the relay answers a repeat with the first answer
// (api_tree_report_idem.go), so a lost answer never becomes a second write.
const neGlyphOutboxCoreJS = `
var OB_MAX=300,OB_SOFT=3000,OB_TRIES=10,OB_AGE=7*864e5;
function outbox(io){var q=[],cur=null,tries=0,wait=0,T=null,ctx={};
 try{q=(io.load()||[]).filter(function(it){return it&&it.id&&it.b})}catch(e){q=[]}
 function save(){try{io.save(q)}catch(e){}}
 function call(it,name,x){var c=ctx[it.id];if(c&&c[name]){try{c[name](x,it)}catch(e){if(io.err)io.err(e)}return true}return false}
 function park(it){if(it.p)return;it.p=1;save();call(it,'park');if(io.park)io.park(it)}
 function arm(ms){if(T!=null)io.cancel(T);T=io.later(function(){T=null;pump()},ms)}
 function again(ra){cur=null;tries++;var ms=ra>0?Math.min(300,ra)*1000:Math.min(60000,2000*Math.pow(2,tries-1));wait=io.now()+ms;arm(ms)}
 function finish(it,x){cur=null;tries=0;wait=0;var i=q.indexOf(it);if(i>=0)q.splice(i,1);save();var had=call(it,'done',x);delete ctx[it.id];if(!had&&io.late)io.late(it,x);pump()}
 function pump(){if(cur||!q.length)return;var now=io.now();
  while(q.length&&now-q[0].t>OB_AGE){var old=q.shift();delete ctx[old.id];save();if(io.drop)io.drop(old)}
  if(!q.length)return;
  if(!io.online()){q.slice().forEach(park);return}
  if(now<wait){arm(wait-now);return}
  var it=cur=q[0],soft=io.later(function(){if(cur===it)park(it)},OB_SOFT);
  io.send(it).then(function(x){io.cancel(soft);if(cur!==it)return;if(x.s===429||x.s>=500){it.n=(it.n||0)+1;if(it.n>=OB_TRIES){finish(it,x);return}park(it);save();again(x.ra);return}finish(it,x)},function(){io.cancel(soft);if(cur!==it)return;park(it);again(0)})}
 return{put:function(it,c){it.id=it.id||io.uuid();it.t=io.now();it.n=0;q.push(it);while(q.length>OB_MAX){var o=q.shift();delete ctx[o.id];if(io.drop)io.drop(o)}if(c)ctx[it.id]=c;save();pump();return it},
  pump:pump,kick:function(){wait=0;tries=0;if(T!=null){io.cancel(T);T=null}pump()},
  discard:function(k){var n=0;q=q.filter(function(it){if(it.k===k&&it!==cur&&it.p){delete ctx[it.id];n++;return false}return true});if(n)save();return n},
  forKey:function(k){return q.filter(function(it){return it.k===k})},list:function(){return q},cur:function(){return cur},wait:function(){return wait}}}
`

// neGlyphOutboxJS — the outbox inside the glyph IIFE: localStorage `neg-ob`, the relay as the
// network, and what the reader sees of it — a `saved` row in the tree card (obRow) with `discard`,
// a centre dot on the map for every waiting report (obMarks, a dotted ring for a waiting new tree),
// one hint when a report is parked and one when the queue has drained.
const neGlyphOutboxJS = neGlyphOutboxCoreJS + `
/* ── tree outbox (ne_glyph_outbox.go) ── */
var OB={hint:0,sent:0,sT:0};
/* an issued key in the export menu (ne.key, localStorage) rides along to OUR relay only: the report is then counted for that key's account (accounts.go); no key = anonymous as ever */
function obHdr(it){var h={'Content-Type':'application/json','X-Idem':it.id},k=typeof xKey==='function'?xKey():'';if(k&&/^[\x21-\x7e]{8,128}$/.test(k))h['X-Export-Key']=k;return h}
function obSend(it){return fetch(TRP,{method:'POST',headers:obHdr(it),body:JSON.stringify(it.b)}).then(function(r){return r.json().catch(function(){return{}}).then(function(j){return{s:r.status,j:j||{},ra:+r.headers.get('Retry-After')||0}})})}
function obParked(it){apSched();var now=Date.now();if(now-OB.hint<8000)return;OB.hint=now;var n=OBX.list().length;showHint('saved on this device \u00b7 '+(n>1?n+' reports wait':'1 report waits')+(navigator.onLine===false?' for a signal':' \u00b7 sending in the background'),3600)}
function obFirst(b){return b&&b.reports?b.reports[0]||{}:b||{}}
/* a report sent by a page that is gone (reload, the browser killed the tab): no card to answer — the reader's marks are kept, the tile is read again, one line says so */
function obLate(it,x){var b=obFirst(it.b),ok=x.s===200||x.s===201,t=x.j&&(x.j.tree||x.j.results&&x.j.results[0]&&x.j.results[0].tree);if(ok){var fl=b.claim&&b.claim!=='tree'&&b.claim!=='missing';if(b.retract){if(fl)delete TR.mine['f'+it.k]}else if(fl)TR.mine['f'+it.k]=b.claim;else TR.mine[t&&(t.client_id||t.tree_id)||it.k]=1;trSave();OB.sent++}else if(x.s!==404&&x.s!==409&&x.s!==403)showHint('a saved tree report was not accepted \u00b7 '+trErr(x.j,x.s),4200);
 var e=it.lon!=null?trTileAt(it.lon,it.lat):null;if(e)trFreshSoon(e,1500,true);apSched();clearTimeout(OB.sT);OB.sT=setTimeout(function(){if(OB.sent&&!OBX.list().length){showHint(OB.sent+(OB.sent>1?' saved tree reports sent':' saved tree report sent'),3200);OB.sent=0}},900)}
function obDropped(it){showHint('a saved tree report was too old to send \u00b7 dropped',3600);apSched()}
var OBX=outbox({load:function(){return JSON.parse(localStorage.getItem('neg-ob')||'[]')},save:function(q){try{localStorage.setItem('neg-ob',JSON.stringify(q))}catch(e){}},uuid:trUUID,now:function(){return Date.now()},later:function(f,ms){return setTimeout(f,ms)},cancel:function(t){clearTimeout(t)},online:function(){return navigator.onLine!==false},send:obSend,park:obParked,late:obLate,drop:obDropped,err:function(e){telErr(e&&(e.stack||e.message)||e,'outbox')}});
function obWord(b){b=obFirst(b);if(b.retract==='last')return'undo';if(b.retract)return b.claim&&b.claim!=='tree'&&b.claim!=='missing'?'flag taken back':'withdraw';if(b.claim==='missing')return'new tree';if(b.species)return trName(b.species);if(b.h_m!=null)return'height '+b.h_m+'\u202fm';if(b.dbh_cm!=null)return'dbh '+b.dbh_cm+'\u202fcm';if(b.state!==undefined)return b.state===null?'state taken back':b.date?'date '+b.date:(ST[b.state]||b.state);if(b.note!==undefined&&(!b.claim||b.claim==='tree'))return b.note===null?'note taken back':'note';return CL[b.claim]||b.claim||'report'}
/* the card's row for what waits on this tree: said in the reader's words, with the way out */
function obRow(a){var L=OBX.forKey(trLL(a.lon,a.lat)).filter(function(it){return it.p});if(!L.length)return'';var c=OBX.cur(),fly=c&&L.indexOf(c)>=0;return'<div class="rw off ob"><span class="lb">saved</span><span class="vl"><span class="mut">'+L.map(function(it){return esc(obWord(it.b))}).join(' \u00b7 ')+' \u2014 on this device, '+(navigator.onLine===false?'sent when the signal is back':'sending in the background')+'</span>'+(L.length>1||!fly?' '+trB('obx',null,'discard','',null,'do not send '+(L.length>1?'these':'this')):'')+'</span></div>'}
function obDiscard(a){var n=OBX.discard(trLL(a.lon,a.lat));if(n){var ui=a.tr||{};ui.busy=false;ui.done='';ui.err='';apSched();showHint(n>1?n+' saved reports discarded':'saved report discarded',2200)}trRe(a)}
/* on the map: a centre dot (= yours) where a report waits; a tree that waits to be added gets its dotted ring too */
function obMarks(Q,C,zpx,cw,ch){var L=OBX.list(),seen={};for(var i=0;i<L.length;i++){var it=L[i];if(!it.p||it.lon==null||seen[it.k])continue;seen[it.k]=1;var X=(px(it.lon)-vb.x)*zpx,Y=(py(it.lat)-vb.y)*zpx;if(X<-9||X>cw+9||Y<-9||Y>ch+9)continue;if(obFirst(it.b).claim==='missing')Q.push(X,Y,7);C.push(X,Y,7)}}
window.addEventListener('online',function(){OBX.kick()});document.addEventListener('visibilitychange',function(){if(!document.hidden)OBX.kick()});setTimeout(function(){OBX.pump()},2500);
`
