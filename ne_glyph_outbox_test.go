package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGlyphOutbox — the tree outbox state machine (neGlyphOutboxCoreJS) under node with a fake
// clock and a fake network: order kept, nothing lost across a reload, one request at a time,
// the same id on every attempt, back-off, parking, discard, poison and age limits.
func TestGlyphOutbox(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	p := filepath.Join(t.TempDir(), "outbox.js")
	if err := os.WriteFile(p, []byte(neGlyphOutboxCoreJS+"\n"+glyphOutboxHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, p).CombinedOutput(); err != nil {
		t.Fatalf("outbox: %v\n%s", err, out)
	}
}

const glyphOutboxHarness = `
var assert=require('assert');
/* a world: manual clock + timers, a store that survives "reloads", a network the test answers by hand */
function world(){var W={now:1e6,timers:[],tid:0,store:[],online:true,sent:[],parked:[],late:[],dropped:[],uid:0};
 W.io={load:function(){return JSON.parse(JSON.stringify(W.store))},save:function(q){W.store=JSON.parse(JSON.stringify(q))},uuid:function(){return'id-'+(++W.uid)},now:function(){return W.now},
  later:function(f,ms){var t={id:++W.tid,at:W.now+ms,f:f};W.timers.push(t);return t.id},cancel:function(id){W.timers=W.timers.filter(function(t){return t.id!==id})},online:function(){return W.online},
  send:function(it){return new Promise(function(res,rej){W.sent.push({it:it,id:it.id,res:res,rej:rej})})},park:function(it){W.parked.push(it.id)},late:function(it,x){W.late.push([it.id,x.s])},drop:function(it){W.dropped.push(it.id)},err:function(e){throw e}};
 W.adv=async function(ms){var end=W.now+ms;for(;;){var due=W.timers.filter(function(t){return t.at<=end}).sort(function(a,b){return a.at-b.at})[0];if(!due)break;W.now=due.at;W.timers=W.timers.filter(function(t){return t!==due});due.f();await tick()}W.now=end;await tick()};
 W.open=function(){W.sent=[];W.timers=[];return W.box=outbox(W.io)};return W}
function tick(){return new Promise(function(r){setImmediate(r)})}
(async function(){
 var W=world(),B=W.open(),done=[],cb=function(n){return{done:function(x){done.push([n,x.s])},park:function(){done.push([n,'park'])}}};
 /* order, one at a time, same id on the wire, removed from the store only on a final answer */
 B.put({b:{species:'a'},k:'k1'},cb('a'));B.put({b:{h_m:3},k:'k1'},cb('b'));B.put({b:{retract:'last'},k:'k2'},cb('c'));
 assert.strictEqual(W.sent.length,1,'one request in flight');assert.strictEqual(W.store.length,3,'all three persisted before any answer');
 W.sent[0].res({s:201,j:{}});await tick();assert.deepStrictEqual(done,[['a',201]]);assert.strictEqual(W.sent.length,2);assert.strictEqual(W.sent[1].it.b.h_m,3,'FIFO');assert.strictEqual(W.store.length,2);
 /* a transport failure: parked, kept, sent again with the SAME id after the back-off — not before */
 W.sent[1].rej(new TypeError('net'));await tick();assert.deepStrictEqual(done[1],['b','park']);assert.strictEqual(W.store.length,2);assert.strictEqual(W.sent.length,2);
 await W.adv(1900);assert.strictEqual(W.sent.length,2,'no retry before 2 s');await W.adv(200);assert.strictEqual(W.sent.length,3);assert.strictEqual(W.sent[2].id,W.sent[1].id,'same idempotency id');
 /* 503 with Retry-After: honoured; a second failure doubles the plain back-off */
 W.sent[2].res({s:503,j:{},ra:7});await tick();await W.adv(6900);assert.strictEqual(W.sent.length,3);await W.adv(200);assert.strictEqual(W.sent.length,4);
 W.sent[3].rej(new TypeError('net'));await tick();await W.adv(7900);assert.strictEqual(W.sent.length,4,'third failure waits 8 s');
 /* kick (online event) sends now */
 B.kick();await tick();assert.strictEqual(W.sent.length,5);
 /* a 4xx is an answer: delivered, queue moves on */
 W.sent[4].res({s:400,j:{error:'x'}});await tick();assert.deepStrictEqual(done[done.length-1],['b',400]);assert.strictEqual(W.sent.length,6);assert.strictEqual(W.sent[5].it.b.retract,'last');
 /* reload while the undo is in flight: the new page finds it, sends it under the same id, answers go to late() */
 var idc=W.sent[5].id;W.open();assert.strictEqual(W.box.list().length,1);W.box.pump();assert.strictEqual(W.sent.length,1);assert.strictEqual(W.sent[0].id,idc,'the id survives the reload');
 W.sent[0].res({s:201,j:{}});await tick();assert.deepStrictEqual(W.late,[[idc,201]]);assert.strictEqual(W.store.length,0);
 /* offline: nothing is sent, everything parks at once; kick with a network sends in order */
 W=world();B=W.open();done=[];W.online=false;B.put({b:{species:'x'},k:'k1',lon:1,lat:2},cb('x'));B.put({b:{species:'y'},k:'k9'},cb('y'));
 assert.strictEqual(W.sent.length,0);assert.deepStrictEqual(done,[['x','park'],['y','park']]);assert.strictEqual(W.parked.length,2);assert.strictEqual(B.forKey('k1').length,1);
 /* discard takes the parked reports of one tree only */
 assert.strictEqual(B.discard('k9'),1);assert.strictEqual(W.store.length,1);
 W.online=true;B.kick();await tick();assert.strictEqual(W.sent.length,1);assert.strictEqual(B.discard('k1'),0,'the one in flight cannot be discarded');W.sent[0].res({s:200,j:{}});await tick();assert.deepStrictEqual(done[2],['x',200]);
 /* a slow link: parked after OB_SOFT while the request is still out; the answer still lands */
 W=world();B=W.open();done=[];B.put({b:{species:'s'},k:'k'},cb('s'));await W.adv(OB_SOFT+10);assert.deepStrictEqual(done,[['s','park']]);assert.strictEqual(W.sent.length,1,'no second request while the first is out');
 W.sent[0].res({s:201,j:{}});await tick();assert.deepStrictEqual(done[1],['s',201]);
 /* poison: OB_TRIES refusals deliver the refusal and free the queue; age: an old item is dropped */
 W=world();B=W.open();done=[];B.put({b:{species:'p'},k:'k'},cb('p'));B.put({b:{species:'q'},k:'k'},cb('q'));
 for(var i=0;i<OB_TRIES;i++){W.sent[W.sent.length-1].res({s:502,j:{}});await tick();await W.adv(61000)}
 assert.deepStrictEqual(done.filter(function(d){return d[0]==='p'&&d[1]===502}).length,1,'refusal delivered once');assert.strictEqual(W.sent[W.sent.length-1].it.b.species,'q','queue moved on');
 W=world();W.store=[{id:'old',t:W.now-OB_AGE-1,b:{species:'o'},k:'k'},{id:'new',t:W.now,b:{species:'n'},k:'k'}];B=W.open();B.pump();assert.deepStrictEqual(W.dropped,['old']);assert.strictEqual(W.sent[0].id,'new');
 /* a broken store never breaks the page */
 W=world();W.io.load=function(){throw new Error('quota')};B=W.open();B.put({b:{species:'z'},k:'k'});assert.strictEqual(W.sent.length,1);
})().catch(function(e){console.error(e);process.exit(1)});
`
