package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGlyphDiagSyntax — each diagnostics file parses on its own (they are
// statement lists spliced into the glyph script).
func TestGlyphDiagSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, f := range []string{"ne_glyph_kx.js", "ne_glyph_trace.js", "ne_glyph_deep.js"} {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", f, err, out)
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "`") {
			t.Errorf("%s: a backtick would end the Go raw string it is spliced into", f)
		}
	}
}

// TestGlyphRunMachine — kxRunStep, the ?run state machine, under node: for
// every possible single culprit the bisect must name it; the two dead ends
// (no repro, not a switch) must be told apart; an unknown outcome repeats.
func TestGlyphRunMachine(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := os.ReadFile("ne_glyph_kx.js")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(src), "var KXRUN=")
	if i < 0 {
		t.Fatal("KXRUN not found in ne_glyph_kx.js")
	}
	harness := string(src)[i:] + `
function play(dies){var z=kxRunStep(sc,null),steps=0,deaths=0;while(z.ph!=='done'&&steps<60){var d=dies(z.cur);if(d)deaths++;z=kxRunStep(sc,z,d?'died':'ok');steps++}return{z:z,steps:steps,deaths:deaths}}
var fail=0,sc,N;['touch','mit'].forEach(function(SN){sc=KXRUN[SN];N=sc.bis.length;var W=sc.inv?'cure ':'culprit ';
for(var c=0;c<N;c++){(function(c){var name=sc.bis[c];
 var r=play(function(cur){return cur.fs===1&&cur.view==='near'&&cur.on.indexOf(name)<0});
 if(r.z.verdict!==W+name+' (alone it cures)'){fail++;console.log('culprit '+name+': '+r.z.verdict)}
 if(r.steps>sc.pre.length+6||r.deaths>6){fail++;console.log('culprit '+name+': '+r.steps+' steps, '+r.deaths+' deaths')}})(c)}
var r=play(function(){return false});if(!/^no repro/.test(r.z.verdict)){fail++;console.log('no repro: '+r.z.verdict)}
r=play(function(cur){return cur.fs===1&&cur.view==='near'});if(!(sc.inv?/^not cured/:/^not a switch/).test(r.z.verdict)){fail++;console.log('not a switch: '+r.z.verdict)}});
sc=KXRUN.touch;
/* inv: a step's list = the mitigations kept, the page gets the undo switch of every other one */
var ms=KXRUN.mit,k=kxRunSet(ms,{on:[]}).join(',');if(k!=='ulive'||kxRunSet(ms,{on:ms.bis}).length||kxRunSet(KXRUN.touch,{on:['cap']}).join()!=='cap'){fail++;console.log('kxRunSet: '+k)}
/* two switches needed together: the bisect names the later one, the confirm step says it is not alone */
r=play(function(cur){return cur.fs===1&&cur.view==='near'&&!(cur.on.indexOf('cap')>=0&&cur.on.indexOf('pt')>=0)});if(!/^culprit pt \(not alone/.test(r.z.verdict)){fail++;console.log('pair: '+r.z.verdict)}
var z=kxRunStep(sc,null),id=z.cur.id;z=kxRunStep(sc,z,'');if(z.cur.id!==id||z.log.length){fail++;console.log('unknown outcome must repeat the step')}
console.log(fail?'FAIL':'OK');process.exit(fail?1:0)`
	p := filepath.Join(t.TempDir(), "run.js")
	if err := os.WriteFile(p, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, p).CombinedOutput(); err != nil {
		t.Fatalf("run machine: %v\n%s", err, out)
	}
}
