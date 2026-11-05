#!/usr/bin/env python3
"""Bir guard satırının (dosya:satır) hangi uçlardan erişildiğini çözer.

Kullanım: python3 -I resolve.py <backend-src kökü> <eps.json> <dosya:satır> [...]
Her girdi için: enclosing metot, dosya içi çağıran zinciri, uçlar.
"""
import json
import os
import re
import sys

ROOT = sys.argv[1]
JAVA = os.path.join(ROOT, "src/main/java/com/exskylab/koala")
M2E = json.load(open(sys.argv[2]))["method_to_endpoints"]

SIG = re.compile(r'^    (?:@\w+(?:\([^)]*\))?\s+)*(public|private|protected|static|\s)*[\w<>\[\],.? ]*?\b(\w+)\s*\([^;]*$')
KEYWORDS = {"if", "for", "while", "switch", "catch", "return", "new", "else", "throw", "synchronized"}


def methods(lines):
    """[(start, end, name, visibility)] üst düzey (4 boşluk girintili) metotlar"""
    out = []
    i = 0
    while i < len(lines):
        l = lines[i]
        m = SIG.match(l)
        if m and m.group(2) not in KEYWORDS and not l.strip().startswith(("return", "throw")):
            vis = "public" if " public " in f" {l} " else ("private" if " private " in f" {l} " else "package")
            depth = 0
            started = False
            j = i
            while j < len(lines):
                depth += lines[j].count("{") - lines[j].count("}")
                if "{" in lines[j]:
                    started = True
                if started and depth <= 0:
                    break
                if not started and lines[j].rstrip().endswith(";"):
                    break
                j += 1
            if started:
                out.append((i + 1, j + 1, m.group(2), vis))
                i = j + 1
                continue
        i += 1
    return out


def enclosing(ms, line):
    for s, e, n, v in ms:
        if s <= line <= e:
            return (s, e, n, v)
    return None


def public_roots(lines, ms, name, seen=None):
    seen = seen or set()
    if name in seen:
        return set()
    seen.add(name)
    me = [m for m in ms if m[2] == name]
    if any(m[3] in ("public", "package") for m in me):
        roots = {name}
    else:
        roots = set()
    if roots:
        return roots
    pat = re.compile(r'(?<![\w.])' + re.escape(name) + r'\s*\(|this::' + re.escape(name) + r'\b|\b\w+::' + re.escape(name) + r'\b')
    for idx, l in enumerate(lines, start=1):
        if pat.search(l):
            enc = enclosing(ms, idx)
            if enc and enc[2] != name:
                roots |= public_roots(lines, ms, enc[2], seen)
    return roots


def resolve(spec):
    path, line = spec.rsplit(":", 1)
    line = int(line)
    lines = open(os.path.join(JAVA, path), encoding="utf-8").read().split("\n")
    ms = methods(lines)
    enc = enclosing(ms, line)
    cls = os.path.basename(path)[:-5]
    if not enc:
        return {"spec": spec, "method": None, "roots": [], "endpoints": []}
    roots = public_roots(lines, ms, enc[2])
    eps = set()
    for r in roots:
        for e in M2E.get(f"{cls}.{r}", []):
            eps.add(e)
    return {"spec": spec, "method": enc[2], "roots": sorted(roots), "endpoints": sorted(eps)}


if __name__ == "__main__":
    for s in sys.argv[3:]:
        print(json.dumps(resolve(s), ensure_ascii=False))
