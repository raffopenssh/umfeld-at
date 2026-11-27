# Trees by email — ops

Public description: `docs/qgis-layer.md` §Trees by email. Design, bounds and the
order of checks: header comments of `tree_mail.go` and `tree_mail_parse.go`.

## Keys

`TREE_MAIL_KEYS=key:name[:edits per day],…` in the env file (0600), a list of
its own — never the export keys, which sit in project files. A key is typed on a
phone into a subject line: 8 characters from `abcdefghjkmnpqrstuvwxyz23456789`
(40 bits; case, hyphens and spaces are ignored on entry; guessing is capped at 5
wrong keys per address and 1 000 in total per day, answered with silence).

    k=$(LC_ALL=C tr -dc 'abcdefghjkmnpqrstuvwxyz23456789' </dev/urandom | head -c8); echo "${k:0:4}-${k:4}"

Add `<key>:<name>` to the list, `scripts/deploy.sh` (or restart). Optional fields after the
name, in any order: `300` (edits a day), `@x.example` (only addresses of that mail domain may
subscribe — compared at subscribe time, never stored), `addr=1` (addresses the key binds at a
time, default and ceiling 5), `acct=<id>` (the account the key's edits are counted for — the
name of the holder's export key; without it the key is an account of its own, `docs/accounts.md`).
The holder of a mail key can also put it into the map (key field of the export menu, or
`/#key=<key>`): tree reports from that browser are then counted for the same account. It opens no export. A **personal key** is `key:name:@x.example:addr=1`: one person, one
reporter id, one address.

A right key that cannot bind the sender is **answered** (the sender holds the key; silence would
read as a broken service), a wrong key never is:

| Case | Answer | counter |
|---|---|---|
| every address of the key is taken | *already in use by another address* — your key: `stop` from the old address; somebody else's: ask for one of your own | `subscribe_full` |
| the key's mail domain is another | *issued for the addresses of one organisation* (the domain is not named) | `subscribe_other_domain` |
| this address already holds this key | *already subscribed* + the help | `subscribe_again` |

Of an address only the keyed hash is kept (binding, the day's counters) — no domain, no local part. Removing a key
drops its addresses at their next mail. Keys and names never appear in public
docs, answers or logs (logs carry the name only).

`TREE_MAIL_ADDRESS` (default `trees@<public host>`) is the envelope recipient
the loop answers for; other mail to the machine is moved to `Maildir/cur`
untouched. `TREE_MAIL_DIR` (default `~/Maildir`).

## Attribution

Every edit goes out under the deployment's reporter key with
`source: umfeld-at/tree-mail` and `reporter: m<12 hex>` = HMAC(key, name): one
person's edits fold, can be withdrawn (`undo`) or discounted at the peer without
touching anybody else's, and the id says nothing about key, name or address.

## Triage

    curl -s -H "Authorization: Bearer $UMFELD_TOKEN" localhost:8000$P/api/v1/debug/inflight | jq .caches.tree_mail
    journalctl -u umfeld-at | grep tree-mail

`mails` counts what happened by word: `subscribed`, `mails_read`, `replies`,
`dropped_unknown_sender`, `subscribe_wrong_key`, `dropped_unverified` (the
gateway would not answer: From not DKIM-aligned — nothing was relayed),
`dropped_automatic`, `other_address`, `edits_written|known|refused`.
**A correct `subscribe` got no answer** → `dropped_unverified` went up and
`last_gateway_refusal` (also `journalctl … | grep 'gateway refused'`) quotes the
platform's reason. `reply policy is "known"` = the machine may not answer
correspondents at all: `ssh exe.dev share receive-email <vm> --reply-policy=all`
(owner's call; nothing in the service can widen it). The mail is gone by then —
the sender mails `subscribe <key>` again.

**A mail was silently dropped** (`dropped_unknown_sender`) → the log line
`dropped, sender not subscribed; subject shape: words=N prefix=none|reply|forward
first=<command>|other|none` says why it was not a subscribe: a prefix, a first
word that is not `subscribe`, or an empty subject. No subject text is logged.

Daily edits: 1 500 per key (unless the key says otherwise), 1 500 per address,
1 500 for the whole path (the peer's cap for our reporter key is shared with
the map — raise the total only together with the peer).

State: `data/tree_mail/state.json` (address hashes → key name, day counters);
`data/tree_mail/queue/` is empty unless the gateway or the peer was busy.

Species: names go to the peer as written; its resolver knows German, English,
Latin, codes and abbreviations and answers an unknown one with suggestions,
which the second receipt quotes. No LLM in the path.

## Same tree · notes (2026-11-20)

- **`same tree`** (`gleicher baum`, `zwiesel`, `doppelt`, …; `tmSaid.flag = "crown"`): `tmDiffRows` runs
  every row through `tmDiff`, then binds each marked row to the tallest unmarked row of the file
  within `(reach_a + reach_b) × 1.25 + 1 m` (reach = `crown_d / 2`, else `0.2 × h`, ≤ 25 m). The flag
  sits on the LOWER top: `{lon,lat,claim:"spurious",observed{h_m},expected{absent:true,hint:"crown",
  of:{lon,lat}}}` — the same body the tree card sends (`trSameSend`). `of` carries no id: the
  export's `t<lon>_<lat>` is ours, the peer's ids are another grid. Undo = `retract:true` like any flag.
- **Notes** (`tmNote`): reader's text = `note` column/field + lines typed into the description beside
  the table (`tmRow.free`); unread fields (`tmRow.extra`, minus `tmIgnore`) ride along only on a row
  that reports something. `tmPersonal` drops e-mail / phone-like values (notes are public at the
  peer); length ≤ the peer's `limits.note_chars` (`tmNoteLimit`, 280 when it names none, ceiling `treeNoteCeil` 4500). The peer stores notes since 2026-11-20 (its `#tree-note`): a note alone is a
  report (`tmNoteAlone`). Length = `limits.note_chars` of `trees/taxa/dict` when advertised
  (`treeNoteCap`, asked hourly; `debug/inflight` → `tree_report.peer_notes`), else 280; the peer cuts longer ones.
- The help mail (`tmHelpText`) is three steps + one table; keep it glanceable on a phone.

## Position frames (2026-11-21)

The observed-layer peer may correct the tree positions of a KG after an export was made (a constant
shift; its `llm.txt#position-frames`). Code + overview: `tree_frames.go`.

- An export row carries `frame` (`p0` = never moved). `tmDiff` hands it back as `"frame"` on every
  report of a row with an id; a drawn point (no id) never gets one — real-world positions do not move.
- A file without the field (older export, an app that dropped it): `tmFrames` asks the peer once
  (`POST /trees/frame`, the file's apex points). No KG listed → nothing is sent. Else, per row:
  the LiDAR height decides where given — position AND `h` equal to an apex of today's copy
  (`neOGCPiece`, ≤ 16 pieces per file) = that apex's frame; position + the KG's `moved_m`
  (EPSG:3035, `laeaFwd`/`laeaInv`) on an apex of that height, and none at the row's own position =
  the copy before (the peer's `stale` token); both fit → the next rule, then today's; else the peer's answer for the KG when
  the KG boxes round the point agree and the peer is confident; else no frame (the peer infers).
- Answers: `frame.moved_m` / `frame.ambiguous` are counted (`edits_moved_frame`,
  `edits_frame_unclear`, `frame_by_height`, `frame_by_kg` on `debug/inflight` → `tree_mail`) and
  logged, and said in ONE extra mail after the receipt (the file is a copy that ages: "export again
  when convenient"; unclear rows listed, ≤ 5). The live map never says any of it.
- Our own copies: the day-keyed tree caches (`treep/`, `tree/`, `treej/`) carry `treeFrameGen` in
  the key; it is bumped when the peer's `frames_rev` changes (asked ≤ every 10 min while trees are
  served, within 30 s of a `moved_m` answer). `debug/inflight` → `tree_frames`.
