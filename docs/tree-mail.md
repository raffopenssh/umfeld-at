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

Add `<key>:<name>` to the list, `scripts/deploy.sh` (or restart). Removing a key
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
