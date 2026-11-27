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

## The tool: `scripts/acct`

Everything below is one CLI (python, stdlib only; `scripts/acct` alone prints the
list). It edits the env file atomically (0600, no backup copy — a copy would
keep keys we just removed) and asks the running service on the private tier for
the rest; it re-runs itself under sudo when it has to. Key edits go live with
`--deploy` (= `scripts/deploy.sh`, a restart that refuses no connection).
**There is nothing for a key holder on the public side**: statements and sheets
are made here and sent.

    scripts/acct new avl --mail 6 --domain @a-v-l.at --deploy   # account + export key (shown ONCE) + 6 personal mail keys
    scripts/acct links avl 6                                    # activation links for six people
    scripts/acct list                                           # every account, this month
    scripts/acct statement avl --month last                     # the usage statement to send (text; --by day)
    scripts/acct csv --month 2026-11 > nov.csv                  # the ledger as a sheet, all accounts (or one: csv avl)
    scripts/acct show avl --month last                          # JSON: ledger + the peer's counts
    scripts/acct keys avl                                       # entries by hash prefix / name
    scripts/acct rotate avl --deploy                            # second export key, same account; then rm-key <id> <prefix>
    scripts/acct revoke avl 3 --deploy                          # one person's member key off (unrevoke)
    scripts/acct mail-key avl 2 --deploy                        # two more personal mail keys
    scripts/acct noacct demo --deploy                           # a shared key: budget only, no account
    scripts/acct close avl --yes --deploy && scripts/acct unlink avl --yes   # account closed, link erased at the peer
    scripts/acct doctor                                         # secret, file mode, plain keys, stale copies, service in step, peer linking

`--json` on `new`, `links`, `list`, `keys`, `rotate`, `mail-key` for scripting.
Tests: `tests_py/test_acct_cli.py` (env edits), `accounts_test.go` (service).

## Keys at rest

`EXPORT_KEYS` holds **hashes**: `sha256:<hex>:name:rps:burst:daily[:noacct]`. The
key is shown once by `acct new` / `rotate` and exists only at its holder; a
lost key is replaced (`rotate`), not recovered. A plain `key:name:…` entry still
loads (`acct hash-env` converts; `doctor` flags it and any old copy of the env
file that still has one). Member keys need no entry at all. **Mail keys stay
plain**: they are 40 bits typed on a phone — a hash of that is searched in
minutes, so it would only pretend — and the reporter id of their past edits is
derived from the key itself.

`noacct` marks an issued key that is **no account** (the shared demo key): it
keeps its budget and is otherwise anonymous — no ledger row, no pseudonym, no
member keys, not in any list.

`ACCT_SECRET` (env, ≥ 32 random chars; `scripts/install.sh` and `acct new`
generate it when missing) keys the pseudonyms. **Never change it** — a new
secret is a new pseudonym for every account and orphans every link at the
peer. Without it nothing is linked. Back it up with the env file.

An account id is 2–24 chars of `a-z 0-9 _ -` and permanent.

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
| **member key** `<id>.<n>.<mac>` | exports + reports, as the account | a person. Derived (`HMAC(ACCT_SECRET, "member:<id>:<n>")`), nothing stored, the number is never written or sent anywhere. `acct links <id> N` lists links 1…N (same ones every time). Switch one off: `acct revoke <id> <n>` (`ACCT_REVOKED`); all die with the account's `EXPORT_KEYS` entry |
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

    scripts/acct csv --month last > ledger.csv        # period,account,exports,table,tiles,archive,project,features,layers,other,bytes,reports,limited
    scripts/acct statement <id> --month last          # per holder: totals, products, the peer's tree counts, per-day table

Behind it (private tier): `GET $P/api/v1/accounts[/{id}]` with `since=&until=`
(unix), `format=csv|txt`, `by=day`, `members=N`; `DELETE …/{id}`.

- `ledger` — ours, per month from `data/accounts/usage.json` (UTC days, ≤ 800
  days): `exports` (+ `kinds`: table · tiles · archive · project · features ·
  layers), `bytes`, `reports` (relayed POSTs; a batch counts once), `limited`
  (429s, not in `exports`). Written ≤ once a minute and at shutdown. Anonymous
  callers are never written.
- `peer` — `{linked, reporters, reports, tree_versions, species, measures,
  added, retracted, trees, flags, first_ts, last_ts}`, counts only, fetched on
  demand and cached 1 h per account and window (`peer_fetched`). Never per
  page view.
- Closing an account: `acct close` + deploy **first**, then `acct unlink` (a
  report under a live key links again — the tool refuses the other order). Take
  the last statement before the unlink: the peer's counts go with it.
- Days are UTC.

`data/accounts/` is billing state: keep it, back it up, never commit it.

## Triage

    scripts/acct doctor

`ACCT_SECRET` missing → nothing is linked. *the peer links our reports … not* →
the peer does not accept accounts from our reporter key (verified holders only)
— ask the peer; reports are not affected. Raw counters: `debug/inflight` →
`accounts`.

A body `account` field sent by a browser is dropped by the relay (the peer
would read it): an account is only ever what the relay derives from a key.

`configured:false` → `ACCT_SECRET` missing. `reports_not_linked` > 0 +
`last_warning` → the peer does not accept accounts from our reporter key
(verified holders only) — ask the peer; reports are not affected.

A key in a URL (`key=`) is replaced by `key=-` in the request log and the
in-flight list (`acctURL`).
