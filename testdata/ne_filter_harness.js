// Node harness for TestNEFilterJSParity: loads the Worker source with stubs and
// answers one JSON job on stdin: {fields:{...}, exprs:[...], bin:<base64 NFX1>}
// → {exprs:[{canon,words,ignored,ext}], masks:[<base64 Uint8Array>]}.
'use strict';
const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const job = JSON.parse(fs.readFileSync(0, 'utf8'));
const ctx = { self: {}, console, URL, Map, Set, Math, JSON, Promise, TextDecoder, Int8Array, Uint8Array, Int32Array, Uint16Array, Float64Array, DataView, ArrayBuffer, Number, String, Array, Object, isNaN, parseFloat, Error };
ctx.fetch = () => Promise.reject(new Error('no fetch in harness'));
ctx.self.postMessage = () => {};
vm.createContext(ctx);
vm.runInContext(src + '\n;this.__h={parse,canonical,words,needsExt,evaluate,decode,FIELDS,WORDS,setCat:function(j){CAT=j;FIELDS={};j.fields.forEach(function(f){FIELDS[f.id]=f});WORDS=j.words||{}}};', ctx, { filename: 'ne_glyph_filter.js' });
const h = ctx.__h;
h.setCat(job.fields);
const out = { exprs: [], masks: [] };
let bin = null;
if (job.bin) {
  const buf = Buffer.from(job.bin, 'base64');
  const ab = buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength);
  bin = h.decode(ab);
  out.n = bin.n;
  out.state = bin.hdr.state;
}
for (const f of job.exprs) {
  const e = h.parse(f);
  out.exprs.push({ canon: h.canonical(e), words: h.words(e), ignored: e.ignored, ext: h.needsExt(e) });
  if (bin) out.masks.push(Buffer.from(h.evaluate(bin, e)).toString('base64'));
}
process.stdout.write(JSON.stringify(out));
