"""scripts/acct — the env-file half (no service needed): what it writes must be
what export_gate.go / tree_mail.go read, and a key is only ever kept as its hash."""
import hashlib, json, os, re, stat, subprocess, sys

ACCT = os.path.join(os.path.dirname(__file__), "..", "scripts", "acct")


def run(env, *args, ok=True):
    r = subprocess.run([sys.executable, ACCT, "--env", str(env)] + list(args), capture_output=True, text=True, env=dict(os.environ, ACCT_NO_SUDO="1"))
    assert (r.returncode == 0) == ok, (args, r.stdout, r.stderr)
    return r.stdout


def val(env, name):
    m = re.search(r"^%s=(.*)$" % name, env.read_text(), re.M)
    return m.group(1) if m else None


def test_lifecycle(tmp_path):
    env = tmp_path / "e.env"
    env.write_text("UMFELD_TOKEN=t\n# a comment\nEXPORT_KEYS=plainkey123456:demo:5:30:5000\nTREE_MAIL_KEYS=abcd2345:solo\nOTHER=x,y\n")
    os.chmod(env, 0o600)

    new = json.loads(run(env, "new", "org", "--mail", "2", "--domain", "@x.example", "--json"))
    key = new["export_key"]
    assert key.startswith("xk_") and len(key) == 31 and len(new["mail_keys"]) == 2
    text = env.read_text()
    assert key not in text, "the export key itself was written"
    assert "sha256:%s:org:10:60:100000" % hashlib.sha256(key.encode()).hexdigest() in val(env, "EXPORT_KEYS")
    mk = new["mail_keys"][0]["key"].replace("-", "")
    assert "%s:org-1:@x.example:addr=1:acct=org" % mk in val(env, "TREE_MAIL_KEYS")
    assert len(val(env, "ACCT_SECRET")) >= 32 and "# a comment" in text and "OTHER=x,y" in text
    assert stat.S_IMODE(os.stat(env).st_mode) == 0o600
    secret = val(env, "ACCT_SECRET")

    run(env, "new", "org", ok=False)            # exists
    run(env, "new", "Bad Id", ok=False)         # ids are permanent and plain
    assert "org-3" in run(env, "mail-key", "org")

    # the plain demo key: flagged no-account, then hashed — same key, same budget
    run(env, "noacct", "demo")
    assert val(env, "EXPORT_KEYS").startswith("plainkey123456:demo:5:30:5000:noacct")
    assert "hashed: demo" in run(env, "hash-env")
    assert "plainkey123456" not in env.read_text()
    assert "sha256:%s:demo:5:30:5000:noacct" % hashlib.sha256(b"plainkey123456").hexdigest() in val(env, "EXPORT_KEYS")
    assert "nothing to do" in run(env, "hash-env")

    # rotation: a second entry under the same name, the old one removable by hash prefix
    rot = json.loads(run(env, "rotate", "org", "--json"))["export_key"]
    assert rot != key and val(env, "EXPORT_KEYS").count(":org:") == 2
    old = hashlib.sha256(key.encode()).hexdigest()
    run(env, "rm-key", "org", old[:3], ok=False)  # too short
    run(env, "rm-key", "org", old[:10])
    assert old not in val(env, "EXPORT_KEYS") and hashlib.sha256(rot.encode()).hexdigest() in val(env, "EXPORT_KEYS")
    run(env, "rm-key", "org", hashlib.sha256(rot.encode()).hexdigest()[:10], ok=False)  # the last key: close, or --force

    run(env, "revoke", "org", "3")
    run(env, "revoke", "org", "3", ok=False)
    assert val(env, "ACCT_REVOKED") == "org.3"
    listing = run(env, "keys", "org")
    assert "member 3" in listing and "org-2" in listing and "demo" not in listing

    run(env, "close", "org", ok=False)  # needs --yes
    run(env, "close", "org", "--yes")
    assert ":org:" not in val(env, "EXPORT_KEYS") and "org-" not in val(env, "TREE_MAIL_KEYS") and val(env, "ACCT_REVOKED") == ""
    assert "abcd2345:solo" in val(env, "TREE_MAIL_KEYS") and ":demo:" in val(env, "EXPORT_KEYS")
    assert val(env, "ACCT_SECRET") == secret, "the secret must never change"
