// api_legal_pages.go — Impressum, Datenschutzerklärung, Lizenzen: the pages
// an Austrian website must carry (§ 5 ECG, § 25 MedienG, Art. 13 DSGVO) once
// it is publicly reachable. Public tier, tokenless, no data access.
//
// Operator name and street are stored ROT13 and decoded
// in the browser; the e-mail is never in the DOM until a human clicks
// "E-Mail anzeigen" (spam harvesters). The page is still complete for a human. Everything here must describe what the service ACTUALLY does — keep
// it in step with api.go (request log: no IP), api_ne_public.go (IP only in an
// in-memory rate-limit bucket), api_feedback.go (free-text feedback is stored
// and published), ne_glyph.go (localStorage for view prefs, no cookies), and
// the landing page (no third-party fonts/CSS/JS — the only cross-origin
// traffic is the glyph's live data fetches to {OBS_NAME}, §3/§4
// below). TestNoExternalAssets (api_no_external_assets_test.go) enforces
// both the no-external-asset rule and the wording that describes it.
package main

import (
	"net/http"
)

const legalStand = "Oktober 2026"

// legalPageCSS — same mono look as the docs, kept small.
const legalPageCSS = `body{font:15px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;max-width:52rem;margin:2rem auto;padding:0 1rem;color:#222;background:#fff}
h1{font-size:1.5rem;margin:.2rem 0 1rem}h2{font-size:1.05rem;margin:1.6rem 0 .4rem}
a{color:#0645ad}ul{padding-left:1.3rem}.nav{color:#666;font-size:.9rem}.muted{color:#666;font-size:.9rem}
code{background:#f3f3f3;padding:0 .2rem}button.obf-email{font:inherit;color:#0645ad;background:none;border:1px solid #0645ad;border-radius:3px;padding:0 .4rem;cursor:pointer}`

// legalObfJS — decodes .obf-name / .obf-street (ROT13) at load. The e-mail is
// NOT in the HTML or the DOM at all: it is kept as a reversed, per-position XOR'd
// byte array and only assembled into the page when a human clicks the button
// (harvesters that run JS and scrape the loaded DOM still see nothing).
const legalObfJS = `<script>(function(){var r=function(s){return s.replace(/[a-z]/gi,function(c){var k=c.charCodeAt(0),b=k<=90?65:97;return String.fromCharCode((k-b+13)%26+b)})};
document.querySelectorAll('.obf-name').forEach(function(n){n.textContent=r('Enssnry Uvpxvfpu')});
document.querySelectorAll('.obf-street').forEach(function(n){n.textContent=r('Tbyqfpuyntfgen')+String.fromCharCode(223)+r('r 23/18')});
var d=[106,98,124,4,89,110,108,114,77,117,125,127,126,93,93,100,126,115,66,90,108,38,119,73,70,110,102,124,67,93,107,104,126,76,83,102,127],k=[7,13,31,42,53];
document.querySelectorAll('.obf-email').forEach(function(b){b.addEventListener('click',function(ev){ev.preventDefault();var o='';for(var i=0;i<d.length;i++)o+=String.fromCharCode(d[i]^k[i%k.length]);
var e=o.split('').reverse().join(''),a=document.createElement('a');a.href=['ma','il','to:'].join('')+e;a.textContent=e;b.replaceWith(a)})});})();</script>`

const legalFooter = `<p class="nav"><a href="/">Startseite</a> · <a href="/impressum">Impressum</a> · <a href="/datenschutz">Datenschutz</a> · <a href="/lizenzen">Lizenzen</a></p>`

func legalPage(title, body string) string {
	return `<!DOCTYPE html><html lang="de"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="robots" content="noindex,follow"><title>` + title + ` | umfeld-at</title><style>` + legalPageCSS + `</style></head><body>` +
		`<p class="nav"><a href="/">← umfeld-at</a></p>` +
		body + legalFooter + legalObfJS + `</body></html>`
}

var impressumHTML = legalPage("Impressum", `<h1>Impressum</h1>
<p>Angaben gemäß § 25 MedienG und § 5 ECG für <strong>umfeld-at</strong> ({PUBLIC_HOST}).</p>
<p><strong>Medieninhaber, Herausgeber und Diensteanbieter:</strong><br><span class="obf-name">—</span><br><span class="obf-street">—</span><br>1150 Wien<br>Österreich</p>
<p><strong>Kontakt:</strong> E-Mail: <button type="button" class="obf-email">E-Mail anzeigen</button> · Rückmeldungen zur API auch über <a href="/api/v1/feedback?status=open">/api/v1/feedback</a></p>
<h2>Grundlegende Richtung</h2>
<p>umfeld-at ist ein nicht-kommerzielles Open-Data-Projekt. Es stellt für Österreich Kontextinformationen zu einem Punkt oder Gebiet bereit
(Gemeinde, Schutzgebiete, OSM-Umgebung, Ortsnamen, Bodenpreis-Schätzungen, Rechtsnormen je Katastralgemeinde) und ein H3-Zellen-Produkt
zur gewidmeten und beobachteten Flächennutzung samt deren Veränderung über die Zeit (<a href="/api/v1/docs/ne.md">NE cells</a>).
Es dient der Information der Öffentlichkeit und der Demonstration von Datenverarbeitungsmethoden.</p>
<h2>Keine Grundstücksdaten</h2>
<p>Dieser Dienst speichert und liefert <strong>keine Katasterdaten</strong>: keine Grundstücke, Grundstücksnummern, Einlagezahlen, Gebäudeumrisse
oder Eigentümerdaten. Alle Produkte sind nach Punkt, Rechteck, Verwaltungscode oder H3-Zelle (≈ 300 m²) geschlüsselt und enthalten
ausschließlich Statistiken, aus denen keine Objekte rekonstruierbar sind. Wer Katastergeometrie benötigt, setzt sie selbst aus den
offenen BEV-Vektorkacheln zusammen (<a href="https://github.com/raffopenssh/vtcseamless">vtcseamless / bevdirect</a>, MIT).</p>
<h2>Haftungsausschluss</h2>
<p>Die Inhalte wurden mit größtmöglicher Sorgfalt erstellt; für Richtigkeit, Vollständigkeit und Aktualität wird keine Gewähr übernommen.
Bodenpreise sind <em>Modellschätzungen</em>, keine amtlichen Bewertungen. Flächennutzungs- und Veränderungswerte sind statistische
Ableitungen aus offenen Daten und ersetzen keine Auskunft aus Kataster, Grundbuch oder Flächenwidmungsplan. Die Nutzung erfolgt auf eigene Gefahr.
Für Inhalte externer Links sind ausschließlich deren Betreiber verantwortlich. Der Dienst befindet sich im Stadium „alpha“; Verfügbarkeit und
Schnittstellen können sich ändern.</p>
<p>Dieser Dienst wird nicht vom BEV, von Statistik Austria, der AMA, dem BML, der EEA, UNEP-WCMC, OpenStreetMap oder dem Bundeskanzleramt
betrieben und ist mit diesen nicht verbunden; eine Billigung durch diese Stellen wird nicht behauptet.</p>
<h2>Urheberrecht, Open Source &amp; Lizenzen</h2>
<p>Der Quellcode ist <a href="https://opensource.org/licenses/MIT">MIT</a>-lizenziert (Repository derzeit nicht öffentlich);
die Katasterzusammensetzung <a href="https://github.com/raffopenssh/vtcseamless">vtcseamless / bevdirect</a> und die
Referenzimplementierung der Zellstatistik (<code>ne_cells</code>, in <a href="https://github.com/raffopenssh/vtcseamless-py">vtcseamless-py</a>)
sind öffentlich. Eigene Texte, Grafiken und abgeleitete
Daten (NE-Zellen, Veränderungssignal, Modellwerte) stehen unter <a href="https://creativecommons.org/licenses/by/4.0/">CC BY 4.0</a>
(Namensnennung: „umfeld-at“) – soweit die jeweilige Quelle nichts Strengeres verlangt (WDPA und GADM: nur nicht-kommerziell;
OSM: ODbL). Alle verwendeten Datenquellen mit ihren Lizenzen und Pflichten: <a href="/lizenzen">Lizenzen</a>
(maschinenlesbar <a href="/api/v1/license?format=json">/api/v1/license?format=json</a>).</p>
<p class="muted">Stand: `+legalStand+`</p>`)

var datenschutzHTML = legalPage("Datenschutzerklärung", `<h1>Datenschutzerklärung</h1>
<h2>1. Verantwortlicher</h2>
<p><span class="obf-name">—</span><br><span class="obf-street">—</span><br>1150 Wien, Österreich<br>E-Mail: <button type="button" class="obf-email">E-Mail anzeigen</button></p>
<h2>2. Was dieser Dienst verarbeitet</h2>
<h3>2.1 Zugriffe (Server-Log)</h3>
<p>Bei jedem Aufruf wird ein Protokolleintrag mit Methode, aufgerufener Adresse samt Parametern (z.&nbsp;B. Koordinaten oder Suchbegriff),
Antwortstatus, Antwortdauer, Antwortgröße und der Browser-Familie (z.&nbsp;B. „Firefox“, „curl“) geschrieben.
<strong>Die IP-Adresse wird nicht protokolliert.</strong> Das gilt auch für Suchbegriffe der Adress- und Ortssuche in der Karte
(<code>/api/v1/addresses/search?q=…</code>): sie stehen ohne IP im Protokoll und werden sonst nirgends gespeichert. Die Protokolle werden im Systemjournal des Servers gehalten und automatisch verdrängt
(in der Regel nach wenigen Tagen). Rechtsgrundlage: Art. 6 Abs. 1 lit. f DSGVO (Betrieb, Fehleranalyse, Missbrauchsabwehr).</p>
<h3>2.2 Ratenbegrenzung</h3>
<p>Zur Missbrauchsabwehr wird die IP-Adresse des Aufrufers kurzzeitig <em>nur im Arbeitsspeicher</em> einem Zählwerk zugeordnet und nach
wenigen Minuten Inaktivität verworfen. Sie wird nicht gespeichert, nicht protokolliert und nicht weitergegeben.
Rechtsgrundlage: Art. 6 Abs. 1 lit. f DSGVO.</p>
<h3>2.3 Feedback</h3>
<p>Über <code>/api/v1/feedback</code> können Nutzer:innen und automatisierte Agenten freiwillig Rückmeldungen senden (Kategorie, Titel, Text,
optional Name des sendenden Programms). Diese Einträge werden dauerhaft gespeichert und sind <strong>öffentlich abrufbar</strong>.
Bitte tragen Sie dort keine personenbezogenen Daten ein. Rechtsgrundlage: Art. 6 Abs. 1 lit. a DSGVO (Einwilligung durch Absenden);
Löschung auf Anfrage per E-Mail.</p>
<h3>2.4 Peer-Beiträge</h3>
<p>Partnerdienste (<code>{OBS_NAME}</code>, <code>{MAP_NAME}</code>, <code>{SUB_NAME}</code>) liefern mit Zugangsschlüsseln
aggregierte Zellstatistiken an. Diese enthalten keine personenbezogenen Daten; gespeichert wird der Name des Partnerdienstes, nicht die Person.</p>
<h3>2.5 Was nicht verarbeitet wird</h3>
<p>Keine Registrierung, keine Benutzerkonten, keine Eigentümer- oder Grundstücksdaten, keine Standortabfrage des Geräts (GPS).
Koordinaten, die Sie in der Karte oder per API abfragen, werden beantwortet und erscheinen nur im Zugriffsprotokoll nach 2.1.</p>
<h2>3. Cookies und lokale Speicherung</h2>
<p>Diese Website verwendet <strong>keine Cookies</strong> und keine Tracking- oder Analysedienste. Die interaktive Karte merkt sich
Anzeige-Einstellungen (ein-/ausgeblendete Ebenen, Vollauflösung, die letzten zehn Suchen) im <code>localStorage</code> Ihres Browsers;
diese Werte verlassen Ihr Gerät nicht und können Sie jederzeit über die Browser-Einstellungen löschen. Geteilte Ansichten stehen als
Parameter in der Adresszeile (<code>#q=…</code>) und werden nicht serverseitig gespeichert.</p>
<p>Wenn Sie in der Karte einen Baum melden (Schaltfläche in der Baumkarte), erzeugt der Browser einmalig eine zufällige Kennung
(<code>localStorage</code>-Schlüssel <code>neg-rep</code>) und sendet sie als <code>reporter</code> mit der Meldung an {OBS_NAME}
(siehe 4), damit dort Meldungen derselben Quelle zusammengefasst werden können. Die Kennung enthält keine Geräte- oder Personendaten,
wird ausschließlich beim Melden übertragen und kann wie jeder <code>localStorage</code>-Eintrag gelöscht werden.</p>
<h2>4. Drittanbieter</h2>
<p>Die Seiten laden <strong>keine Ressourcen von Drittanbietern</strong>: keine externen Schriften (nur die Systemschriften Ihres Geräts),
keine externen Stylesheets, Skripte oder Bilder; Kartenkacheln Dritter nur auf Ihre Wahl (Luftbild, unten). Die Flächennutzungs- und Förderdaten der Karte liefert ausschließlich dieser Server.</p>
<p><strong>Ausnahme – Partnerdienst {OBS_NAME}:</strong> Die interaktive Karte ruft die <em>beobachtete</em> Landschaft (LiDAR/Satellit:
Bodenbedeckung, Baumkronen, Gebäudestrukturen) live vom Partnerdienst <code>{OBS_NAME}</code> (<code>{OBS_WEB}</code>) ab – beim Berühren einer Zelle,
beim Auswählen einer Katastralgemeinde und bei eingeschalteter Baumebene. Dabei erhält dieser Dienst Ihre IP-Adresse und den angefragten
Kartenausschnitt (keine Cookies, keine Zugangsdaten; <code>credentials: omit</code>); beim Melden eines Baums zusätzlich die Kennung nach 3.
{OBS_NAME} wird vom selben Betreiber auf derselben Hosting-Plattform (5.) geführt; für ihn gilt dessen eigene Datenschutzerklärung.
Die Förderdaten ({SUB_NAME}) holt hingegen <em>unser Server</em> ab; Ihr Browser kontaktiert diesen Dienst nicht.</p>
<p><strong>Ausnahme – Luftbild (basemap.at):</strong> Nur wenn Sie in der Karte die Foto-Ebene einschalten (Erdkugel-Symbol, <code>;ortho</code> im Link),
lädt Ihr Browser Orthofoto-Kacheln direkt von <code>mapsneu.wien.gv.at</code> (basemap.at, betrieben von der Stadt Wien für geoland.at,
Lizenz CC BY 4.0). Dabei erhält dieser Dienst Ihre IP-Adresse und die Kachelkoordinaten des Kartenausschnitts (keine Cookies, keine
Zugangsdaten; <code>credentials: omit</code>). Solange die Ebene aus ist, wird basemap.at nicht kontaktiert; unser Server leitet die Kacheln
weder weiter noch speichert er sie. Es gilt die Datenschutzerklärung von basemap.at / Stadt Wien.</p>
<p>Verlinkte Seiten (github.com, exe.dev, creativecommons.org, Datenquellen unter <a href="/lizenzen">Lizenzen</a>) werden erst bei Klick
aufgerufen und unterliegen ihren eigenen Datenschutzerklärungen.</p>
<h2>5. Hosting</h2>
<p>Der Dienst wird bei exe.dev (Bold Software, Inc., USA) gehostet; die Server befinden sich in den USA. Der Hosting-Anbieter betreibt den
HTTPS-Zugang (Proxy) und kann dabei nach eigenen Richtlinien Verbindungsdaten verarbeiten. Die Datenübermittlung in die USA erfolgt auf Grundlage
von Art. 49 Abs. 1 lit. a DSGVO (Einwilligung durch Nutzung) bzw. Art. 49 Abs. 1 lit. b DSGVO (Erfüllung des Nutzungsvertrags).
Weitere Informationen: <a href="https://exe.dev/privacy">exe.dev Privacy Notice</a>.</p>
<h2>6. Ihre Rechte</h2>
<ul><li>Auskunft (Art. 15 DSGVO)</li><li>Berichtigung (Art. 16)</li><li>Löschung (Art. 17)</li><li>Einschränkung der Verarbeitung (Art. 18)</li>
<li>Datenübertragbarkeit (Art. 20)</li><li>Widerspruch (Art. 21)</li></ul>
<p>Zur Ausübung dieser Rechte genügt eine E-Mail an die oben genannte Adresse. Da keine IP-Adressen und keine Konten gespeichert werden,
können Zugriffe in der Regel keiner Person zugeordnet werden; betroffen sind praktisch nur Feedback-Einträge.</p>
<h2>7. Beschwerderecht</h2>
<p>Österreichische Datenschutzbehörde, Barichgasse 40–42, 1030 Wien, <a href="https://www.dsb.gv.at/">www.dsb.gv.at</a>.</p>
<h2>8. Open Source</h2>
<p>Der Quellcode ist MIT-lizenziert (Repository derzeit nicht öffentlich); Datenquellen und Lizenzen unter <a href="/lizenzen">Lizenzen</a>, Betreiberangaben im
<a href="/impressum">Impressum</a>.</p>
<p class="muted">Stand: `+legalStand+`</p>`)

// handleLegalPages — /impressum, /datenschutz (+ English aliases) and
// /lizenzen → the existing licence page.
func handleLegalPages(mux *http.ServeMux) {
	page := func(html string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_, _ = w.Write([]byte(peerText(html)))
		}
	}
	mux.HandleFunc("/impressum", page(impressumHTML))
	mux.HandleFunc("/imprint", page(impressumHTML))
	mux.HandleFunc("/datenschutz", page(datenschutzHTML))
	mux.HandleFunc("/privacy", page(datenschutzHTML))
	lic := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/license?format=html", http.StatusFound)
	}
	mux.HandleFunc("/lizenzen", lic)
	mux.HandleFunc("/licenses", lic)
	mux.HandleFunc("/license", lic)
}

// legalPaths — the public allow-list entries for the pages above.
var legalPaths = []string{"/impressum", "/imprint", "/datenschutz", "/privacy", "/lizenzen", "/licenses", "/license"}

func init() {
	for _, p := range legalPaths {
		publicExactPaths[p] = true
	}
}
