# Accounts — who can be invoiced later

Code + design: header comment of `accounts.go`. Internal: keys, account ids and
budgets never appear in public docs or answers.

Everybody stays anonymous. An **account** exists only for a holder of a key we
issued:

| Key | Env list | Account id |
|---|---|---|
| export key (links, `X-Export-Key`, the key field of the export menu) | `EXPORT_KEYS=key:name:rps:burst:daily` | `name` |
| mail key (`subscribe <key>`) | `TREE_MAIL_KEYS=key:name[:…][:acct=<id>]` | `acct=` if given, else `mail:<name>` |

One holder with both → the mail keys carry `acct=<export key name>`; everything
lands on one account. The **id is the name and is permanent**: renaming a key is
a new account. Rotate a key by adding a second `EXPORT_KEYS` entry with the
same name, then removing the old one — same account, same pseudonym.

## Issue a key

    k=xk_$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c28)     # export key (sits in URLs: lower case, no punctuation)
    # /etc/umfeld-at.env:  EXPORT_KEYS=…,$k:<id>:10:60:100000
    #                      TREE_MAIL_KEYS=…,<mail key>:<id>-1:@x.example:addr=1:acct=<id>
    scripts/deploy.sh

`ACCT_SECRET` (env, ≥ 32 random chars, set once by `scripts/install.sh`'s
template) keys the pseudonyms. **Never change it** — a new secret is a new
pseudonym for every account and orphans every link at the peer. Without it
nothing is linked. Back it up with the env file.

## How a person "signs in" — no login, no cookie, no UI change

A key sits in the key field of the export menu (`localStorage` `ne.key`, as
before). It gets there by typing — or by opening an **activation link**
`<public URL>/#key=<key>` from a mail: the key is in the URL *fragment*, which
no server, proxy or log ever sees; the page stores it, removes it from the
address bar before anything reads the hash (`xTake`) and says so once. Emptying
the field signs out. From then on the map's tree reports carry the key to
**our relay** (`X-Export-Key`), which forwards only the pseudonym.

Which key a person holds:

| Key | Works for | Give it to |
|---|---|---|
| account key (`EXPORT_KEYS`) | exports + reports | the holder's machines, projects, sheets — not people |
| **member key** `<id>.<n>.<mac>` | exports + reports, as the account | a person. Derived (`HMAC(ACCT_SECRET, "member:<id>:<n>")`), nothing stored, the number is never written or sent anywhere. `GET $P/api/v1/accounts/<id>?members=N` lists links 1…N (same ones every time). Switch one off: `ACCT_REVOKED=<id>.<n>,…`; all die with the account's `EXPORT_KEYS` entry |
| **mail key** (already handed out) | mail edits + reports from the map; **never** exports | whoever has it: type it into the same field or open `/#key=<mail key>`. The page keeps a key this short (≤ 12 letters/digits) out of every export link and project file |

So the account's master key never needs to be mailed around, and people who
already hold a mail key need nothing new.

## What the peer gets

`X-Reporter-Account: hex(HMAC-SHA256(ACCT_SECRET, "acct:"+id))[:32]` on the
relayed `POST /api/v1/flags`, next to the unchanged `reporter`. Never a key, a
name or an address. The first linked report links that reporter id's whole
history at the peer — no backfill. `account_warning` in the answer is logged
(≤ 1 line / 10 min) and counted; the report is stored either way.

## Billing run

    P=$UMFELD_PRIVATE_PREFIX; H="Authorization: Bearer $UMFELD_TOKEN"
    curl -s -H "$H" "localhost:8000$P/api/v1/accounts?pretty=1"                      # every account, ledger per month
    curl -s -H "$H" "localhost:8000$P/api/v1/accounts/<id>?since=<unix>&until=<unix>&pretty=1"   # + the peer's counts
    curl -s -X DELETE -H "$H" "localhost:8000$P/api/v1/accounts/<id>"                # account closed: the peer forgets the link

- `ledger` — ours, per month from `data/accounts/usage.json` (UTC days, ≤ 800
  days): `exports` (+ `kinds`: table · tiles · archive · project · features ·
  layers), `bytes`, `reports` (relayed POSTs; a batch counts once), `limited`
  (429s, not in `exports`). Written ≤ once a minute and at shutdown. Anonymous
  callers are never written.
- `peer` — `{linked, reporters, reports, tree_versions, species, measures,
  added, retracted, trees, flags, first_ts, last_ts}`, counts only, fetched on
  demand and cached 1 h per account and window (`peer_fetched`). Never per
  page view.
- Closing an account: take the keys out of the env file and deploy **first**,
  then DELETE (a report under a live key links again).

`data/accounts/` is billing state: keep it, back it up, never commit it.

## Triage

    curl -s -H "$H" localhost:8000$P/api/v1/debug/inflight | jq .caches.accounts

A body `account` field sent by a browser is dropped by the relay (the peer
would read it): an account is only ever what the relay derives from a key.

`configured:false` → `ACCT_SECRET` missing. `reports_not_linked` > 0 +
`last_warning` → the peer does not accept accounts from our reporter key
(verified holders only) — ask the peer; reports are not affected.

A key in a URL (`key=`) is replaced by `key=-` in the request log and the
in-flight list (`acctURL`).
