"""Benützungsart (NS) → 9 land-use groups. FROZEN with the algo id: any change to
this table (or to GROUP_ORDER) is a new ALGO id in algo.py. Codes not listed → 'sonst'."""

NS_TABLE_VERSION = "ns-groups-1"

# Fixed, contractual group order — index in this tuple is the byte position in lu[9].
GROUP_ORDER = ("geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst")

GROUPS = {
    "geb":     ("41",),
    "bau":     ("42", "43", "44", "45", "46", "47", "63", "83", "91"),
    "acker":   ("40", "51", "53", "66", "67"),
    "gruen":   ("48", "50", "52", "54", "55", "57"),
    "wald":    ("56", "58"),
    "wasser":  ("59", "60", "61", "64", "70", "71", "82"),
    "verkehr": ("49", "65", "73", "74", "75", "76", "86", "92", "95", "97"),
    "alpen":   ("62", "85", "87", "88", "90"),
    "sonst":   ("72", "77", "78", "79", "80", "81", "84", "93", "94", "96"),
}

_NS2G = {c: g for g, cs in GROUPS.items() for c in cs}


def group_of(ns) -> str:
    return _NS2G.get(str(ns).strip(), "sonst")


def group_index(g: str) -> int:
    return GROUP_ORDER.index(g)
