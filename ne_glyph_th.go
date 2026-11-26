// ne_glyph_th.go — the trees coloured by HEIGHT (share token ;trees=h): a hold on the trees
// icon (380 ms; right-click; key h) swaps the quiet green ramp for one that is read for
// structure; shift-click does the same — docs/ne-glyph.md §Tree heights.
//
//	crowns (LOD 0/1)    the same rings, each in the colour of its own height
//	stand sheet         a hexagon in the colour of its TOP height (9 of 10 tree tops stand below,
//	                    neStandsQ), brighter = denser; once a hexagon is ≥ TH_CZ (8.5) px its core shows
//	                    the LOWER storey (a quarter of the tops stand below): one colour =
//	                    even-aged, a dark core = layered
//	ground              dimmed by one rect under the trees (no CSS filter: a phone repaints the
//	                    full-resolution sheet for one)
//
// The ramp is viridis lifted at its low end (a 3 m tree must still read on the dimmed ground),
// luminance rising with height, 3–45 m in TH_N steps. The legend keeps its shape — the swatches,
// the band bar and the height scale only change ink; what the core means is said once, in a hint.
package main

import "fmt"

// neGlyphThRamp — the ramp's stops (3 m … 45 m +), shared with the QGIS project (ne_mvt_qgs.go).
var neGlyphThRamp = [8][3]uint8{{0x5b, 0x5b, 0xb5}, {0x4a, 0x7f, 0xc0}, {0x2f, 0x9f, 0xb4}, {0x22, 0xb3, 0x9a}, {0x3c, 0xc4, 0x7a}, {0x7f, 0xd3, 0x4e}, {0xc0, 0xdf, 0x25}, {0xfd, 0xe7, 0x25}}

// neGlyphThRampL — the same reading on a LIGHT page: the ground is veiled with the page colour, so ink
// must get darker with height (olive 3 m → teal → blue → deep violet 45 m +). "Stands out more = taller"
// in both modes; the page picks by prefers-color-scheme, exports use the dark ramp over a dimmed ground.
var neGlyphThRampL = [8][3]uint8{{0xb9, 0xb0, 0x1c}, {0x86, 0xb8, 0x32}, {0x46, 0xad, 0x62}, {0x1f, 0x9a, 0x86}, {0x26, 0x80, 0x8e}, {0x31, 0x64, 0x8e}, {0x41, 0x44, 0x87}, {0x44, 0x01, 0x54}}

const neGlyphThMinM, neGlyphThMaxM = 3.0, 45.0

// neGlyphThRGB — the ramp at a height in metres.
func neGlyphThRGB(m float64) [3]uint8 { return neGlyphThRampAt(&neGlyphThRamp, m) }

func neGlyphThRampAt(ramp *[8][3]uint8, m float64) [3]uint8 {
	t := (m - neGlyphThMinM) / (neGlyphThMaxM - neGlyphThMinM)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	t *= float64(len(ramp) - 1)
	i := int(t)
	if i > len(ramp)-2 {
		i = len(ramp) - 2
	}
	f := t - float64(i)
	var o [3]uint8
	for k := 0; k < 3; k++ {
		a, b := float64(ramp[i][k]), float64(ramp[i+1][k])
		o[k] = uint8(a + (b-a)*f + .5)
	}
	return o
}

func neGlyphThHex(c [3]uint8) string { return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2]) }

// neGlyphThStops — the ramp as a JS array literal / a CSS gradient list.
func neGlyphThStops(q string) string { return neGlyphThStopsOf(&neGlyphThRamp, q) }

func neGlyphThStopsOf(ramp *[8][3]uint8, q string) string {
	s := ""
	for i, c := range ramp {
		if i > 0 {
			s += ","
		}
		s += q + neGlyphThHex(c) + q
	}
	return s
}

// neGlyphThVars — the ramp as CSS custom properties: --thg (gradient stops), --th0…4 (the legend's five bands).
func neGlyphThVars(ramp *[8][3]uint8) string {
	s := "--thg:" + neGlyphThStopsOf(ramp, "")
	for i, m := range []float64{4, 10, 20, 30, 40} {
		s += fmt.Sprintf(";--th%d:%s", i, neGlyphThHex(neGlyphThRampAt(ramp, m)))
	}
	return s
}

// neGlyphThCSS — the button's states, the legend swatches (the five bands at 4 · 10 · 20 · 30 · 40 m), the ground, the cores.
var neGlyphThCSS = `
.glyph-view .gv-tree{-webkit-touch-callout:none;-webkit-user-select:none;user-select:none;touch-action:manipulation}
/* heights on: the underline is the ramp */.glyph-view .gv-tree.th[aria-pressed=true]::after{border-bottom:0;height:3px;border-radius:2px;opacity:1;background:linear-gradient(90deg,var(--thg))}
/* a hold in progress: the ramp runs in under the icon */.glyph-view .gv-tree.hold::after{content:"";position:absolute;left:.2em;right:.2em;bottom:-.05em;border-bottom:0;height:3px;border-radius:2px;opacity:1;background:linear-gradient(90deg,var(--thg));transform-origin:0 50%;animation:thHold .38s linear both}
@media(max-width:640px),(hover:none){.glyph-view .gv-tree.hold::after{left:.45em;right:.45em;bottom:.15em}}
@keyframes thHold{from{transform:scaleX(0)}to{transform:scaleX(1)}}
.glyph-view .gv-tree.thp svg{animation:thPop .32s ease-out}@keyframes thPop{40%{transform:scale(1.22)}}
@media(prefers-reduced-motion:reduce){.glyph-view .gv-tree.hold::after,.glyph-view .gv-tree.thp svg{animation:none}}
figure.glyph{` + neGlyphThVars(&neGlyphThRampL) + `}figure.glyph.th-dk{` + neGlyphThVars(&neGlyphThRamp) + `}
.glyph-tip .g-th0,.glyph-peek .g-th0{fill:var(--th0)}.glyph-tip .g-th1,.glyph-peek .g-th1{fill:var(--th1)}.glyph-tip .g-th2,.glyph-peek .g-th2{fill:var(--th2)}.glyph-tip .g-th3,.glyph-peek .g-th3{fill:var(--th3)}.glyph-tip .g-th4,.glyph-peek .g-th4{fill:var(--th4)}
/* the ground steps back INTO THE PAGE (light or dark), inside Austria only */svg.glyph .thg{fill:var(--bg,#fafaf7);fill-opacity:.8;stroke:none;pointer-events:none}
/* over the photo: a thin dark veil; the sheet turns into dots sized by density (troPaint, PH) — the photo shows between them */svg.glyph.v-ortho .hi .thg{fill:#0b0d0c;fill-opacity:.22}
/* a hint has the bottom edge to itself: the trees chip steps out while it speaks (and the hint is opaque and on top where :has is missing) */.glyph-hint{z-index:6;max-width:calc(100% - 16px);box-sizing:border-box}.glyph-main:has(.glyph-hint.on) .glyph-trn{opacity:0!important;transition:opacity .2s}
svg.glyph .tro .co{display:none}svg.glyph .tro.cz .co{display:inline}
.glyph-hint svg.thk{width:1.05em;height:1.05em;vertical-align:-.18em;margin:0 .25em 0 .1em}
`

// neGlyphThJS — runs inside the glyph IIFE, before the stand sheet's block (troSync reads th).
var neGlyphThJS = `
/* ── tree heights in colour (ne_glyph_th.go) ── */
var TH_N=16,TH_CZ=8.5/* px across from which a hexagon shows its core */,TH_MQ=window.matchMedia?matchMedia('(prefers-color-scheme:dark)'):null,TH_S,TH_LUT,th={on:false,sc:null,held:0,hT:0,kn:0};
function thLut(){TH_S=thDk()?[` + neGlyphThStops("'") + `]:[` + neGlyphThStopsOf(&neGlyphThRampL, "'") + `];var o=[],i,k;for(i=0;i<TH_N;i++){var t=i/(TH_N-1)*(TH_S.length-1),j=Math.min(TH_S.length-2,Math.floor(t)),f=t-j,c='#';for(k=1;k<7;k+=2){var u=parseInt(TH_S[j].substr(k,2),16),v=parseInt(TH_S[j+1].substr(k,2),16);c+=('0'+Math.round(u+(v-u)*f).toString(16)).slice(-2)}o.push(c)}TH_LUT=o}
function thInit(){th.dk=thDk();thLut();fig.classList.toggle('th-dk',th.dk)}
function thPhoto(){thSkin();if(thOn()&&typeof troSync==='function'){troSync();apSched()}}
function thSay(){return'tree heights \u00b7 '+(thDk()?'blue 3\u202fm \u2192 yellow 45\u202fm':'olive 3\u202fm \u2192 violet 45\u202fm')+' \u00b7 '+(coarse?'tap and hold':'hold')+' the tree again for the plain crowns'}
/* the device flips light ⇄ dark: new inks everywhere */if(TH_MQ){var thMq=function(){thSkin()};if(TH_MQ.addEventListener)TH_MQ.addEventListener('change',thMq);else if(TH_MQ.addListener)TH_MQ.addListener(thMq)}
function thGd(){if(!th.gd&&cells.length){var gd=[],i;for(i=0;i<cells.length;i++)gd.push(faceD(i));th.gd=gd.join('')}return th.gd||''}
function thOn(){return !!(th&&th.on)}
function thIdx(m){var i=Math.round((m-3)/42*(TH_N-1));return i<0?0:i>=TH_N?TH_N-1:i}
function thHint(h,ms){hint.innerHTML=h;hint.hidden=false;hint.classList.add('on');clearTimeout(hintT);hintT=setTimeout(function(){hint.classList.remove('on')},ms||3200)}
function thHex(a,b){return'<svg class="thk" viewBox="0 0 20 20" aria-hidden="true"><path d="M10 1l7.8 4.5v9L10 19l-7.8-4.5v-9z" fill="'+TH_LUT[thIdx(a)]+'"/><path d="M10 5.5l3.9 2.25v4.5L10 14.5l-3.9-2.25v-4.5z" fill="'+TH_LUT[thIdx(b)]+'"/></svg>'}
/* what the core says — once per page, a few times per device, when the cores first show */
function thKey(){if(th.kn)return;th.kn=1;var n=0;try{n=+localStorage.getItem('neg-thk')||0;localStorage.setItem('neg-thk',n+1)}catch(e){}if(n>=4)return;thHint('rim = the tall trees \u00b7 core = the small ones \u00b7'+thHex(30,28)+'even-aged'+thHex(34,7)+'layered',5200)}
/* the ground steps back: ONE path (the res-7 faces = Austria, nothing outside it) over the land-use fills and UNDER the roads — photo and roads stay there to orient by; the stand sheet joins it there (troPlace) */
function thPlace(){thSkin();var rdl=thOn()&&hi.tree&&!!proj&&hi.on&&hi.g&&hi.l&&hi.l.rd?hi.l.rd[0].parentNode:null;if(!rdl){if(th.sc&&th.sc.parentNode)th.sc.parentNode.removeChild(th.sc);return}if(!th.sc){if(!thGd())return;th.sc=document.createElementNS('http://www.w3.org/2000/svg','path');th.sc.setAttribute('class','thg');th.sc.setAttribute('d',thGd())}var at=typeof tro==='object'&&tro.g&&tro.g.nextSibling===rdl?tro.g:rdl;if(th.sc.nextSibling!==at)hi.g.insertBefore(th.sc,at)}
/* bright inks on a dark page — and over the photo, whatever the page (a dark violet crown is lost on a forest canopy) */
function thDk(){return !!(TH_MQ&&TH_MQ.matches||typeof ot==='object'&&ot.on&&hi.on)}
function thSkin(){var dk=thDk();if(dk===th.dk)return;th.dk=dk;thLut();fig.classList.toggle('th-dk',dk);if(!thOn())return;setTimeout(function(){if(typeof tro==='object')tro.pk='';if(typeof troSync==='function')troSync();apSched();if(typeof tsRender==='function')tsRender();hiLegend()},0)}
function thBtn(){var b=gv.querySelector('.gv-tree');if(!b)return;b.classList.toggle('th',th.on);if(!b.classList.contains('off'))b.title=(hi.tree?'take the trees off the map':'trees \u00b7 far out where they stand \u00b7 close up every tree as a ring of its crown')+' \u00b7 hold for '+(th.on?'the plain crowns':'the heights in colour')+' ( t \u00b7 h )'}
function setTreeH(on,quiet){on=!!on;if(on===th.on){thBtn();return}th.on=on;svg.classList.toggle('v-th',on);thBtn();dbgLog('tree heights '+(on?'on':'off'));if(typeof tro==='object'){tro.pk='';tro.bx=null}if(typeof troSync==='function')troSync();thPlace();apSched();if(typeof tsRender==='function')tsRender();if(quiet)return;hiLegend();setHash();if(on&&hi.tree)showHint(thSay(),3600)}
/* hold (touch, mouse), shift-click, right-click, key h: off → trees on in colour · on → the colours flip */
function thFlip(){var b=gv.querySelector('.gv-tree');if(b){b.classList.remove('thp');void b.offsetWidth;b.classList.add('thp')}try{if(navigator.vibrate)navigator.vibrate(12)}catch(e){}if(!hi.tree){setTreeH(true,true);setTree(true);showHint(thSay(),3600)}else setTreeH(!th.on)}
function thHeld(){var h=Date.now()-th.held<900&&!th.used;th.used=1;return h}
/* a first plain tap on the trees: say once (twice per device) that the icon has a second layer */
function thTease(){if(th.on||th.tz)return;th.tz=1;var n=0;try{n=+localStorage.getItem('neg-thh')||0;localStorage.setItem('neg-thh',n+1)}catch(e){}if(n>=2)return;setTimeout(function(){if(hi.tree&&!th.on)showHint(coarse?'tap and hold the tree \u00b7 heights in colour':'hold, right-click or shift-click the tree ( h ) \u00b7 heights in colour',3800)},1400)}
(function(){var b=gv.querySelector('.gv-tree');if(!b)return;var end=function(){clearTimeout(th.hT);th.hT=0;b.classList.remove('hold')};
 b.addEventListener('pointerdown',function(e){if(e.button>0)return;end();b.classList.add('hold');th.hT=setTimeout(function(){end();th.held=Date.now();th.used=0;thFlip()},380)});
 ['pointerup','pointercancel','pointerleave'].forEach(function(t){b.addEventListener(t,end)});
 /* shift-click = the hold, for a mouse (runs before the bar's own click handler) */b.addEventListener('click',function(e){if(!e.shiftKey||Date.now()-th.held<900&&!th.used)return;e.stopPropagation();e.preventDefault();end();thFlip()});
 b.addEventListener('contextmenu',function(e){e.preventDefault();/* a touch hold fires this too, after the timer: one flip */if(Date.now()-th.held<1200)return;end();th.held=Date.now();th.used=0;thFlip()})})();
thInit();
`
