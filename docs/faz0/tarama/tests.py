#!/usr/bin/env python3
"""Java testlerini indeksler: test metodu -> HTTP istekleri, beklenen statüler,
çağrılan manager metotları, beklenen istisnalar.

Kullanım: python3 -I tests.py <backend-src kökü>  > tests.json
"""
import json
import os
import re
import sys

ROOT = sys.argv[1]
TEST = os.path.join(ROOT, "src/test/java/com/exskylab/koala")

REQ = re.compile(r'\b(get|post|put|patch|delete|multipart)\(\s*"')


def requests_of(text):
    """get("/a/" + id + "/b", ...) -> GET /a/{}/b ; get("/a/{x}", id) -> GET /a/{x}"""
    out = set()
    for m in REQ.finditer(text):
        i = m.end() - 1
        depth = 0
        buf = []
        inq = False
        while i < len(text):
            c = text[i]
            if c == '"' and text[i - 1] != "\\":
                inq = not inq
                buf.append(c)
            elif not inq and c == "(":
                depth += 1
                buf.append(c)
            elif not inq and c in ",)" and depth == 0:
                break
            elif not inq and c == ")":
                depth -= 1
                buf.append(c)
            else:
                buf.append(c)
            i += 1
        expr = "".join(buf)
        parts = re.split(r'("[^"]*")', expr)
        path = ""
        for part in parts:
            if part.startswith('"'):
                path += part[1:-1]
            elif part.strip().strip("+").strip():
                path += "{}"
        out.add(f"{m.group(1).upper()} {path}")
    return out
STATUS = re.compile(r'status\(\)\.(is\w+)\(\s*(\d*)\s*\)')
EXC = re.compile(r'assertThrows\(\s*(\w+)\.class')
EXC2 = re.compile(r'isInstanceOf\(\s*(\w+)\.class')
CALL = re.compile(r'\b(\w+)\.(\w+)\s*\(')
TESTANN = re.compile(r'^\s*@(Test|ParameterizedTest|RepeatedTest)\b')
METH = re.compile(r'^\s{4}(?:[\w<>\[\], ]+\s+)?(\w+)\s*\([^)]*\)?\s*(throws [\w, .]+)?\s*\{?\s*$')


def blocks(lines):
    """[(name, start, end, is_test)] üst düzey metotlar ve iç sınıflardaki metotlar"""
    out = []
    i = 0
    pending_test = False
    while i < len(lines):
        l = lines[i]
        if TESTANN.match(l):
            pending_test = True
            i += 1
            continue
        m = re.match(r'^(\s+)(?:(?:public|private|protected|static|final|void|<[^>]+>)\s+)*[\w<>\[\],.? ]*?\b(\w+)\s*\(', l)
        if m and len(m.group(1)) in (4, 8) and not l.strip().startswith(("return", "if", "for", "while", "new ", "throw", "@", "assert", "jdbc", "mockMvc", "when", "verify")) and ("void" in l or "throws" in l or re.search(r'\)\s*\{\s*$', l) or l.rstrip().endswith("(")):
            name = m.group(2)
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
                out.append((name, i, j, pending_test))
                pending_test = False
                i = j + 1
                continue
        if l.strip() and not l.strip().startswith("@"):
            pending_test = False if not TESTANN.match(l) else pending_test
        i += 1
    return out


def index():
    res = []
    for dp, _, fs in os.walk(TEST):
        for f in fs:
            if not f.endswith(".java"):
                continue
            p = os.path.join(dp, f)
            rel = os.path.relpath(p, os.path.join(ROOT, "src/test/java"))
            lines = open(p, encoding="utf-8").read().split("\n")
            bl = blocks(lines)
            helpers = {}
            for name, s, e, t in bl:
                body = "\n".join(lines[s:e + 1])
                helpers.setdefault(name, []).append(body)
            for name, s, e, t in bl:
                if not t:
                    continue
                body = "\n".join(lines[s:e + 1])
                # bir düzey yardımcı çözümleme
                extra = []
                for cm in re.finditer(r'(?<![\w.])(\w+)\s*\(', body):
                    hn = cm.group(1)
                    if hn in helpers and hn != name:
                        extra.extend(helpers[hn])
                full = body + "\n" + "\n".join(extra)
                res.append({
                    "file": rel,
                    "line": s + 1,
                    "test": name,
                    "requests": sorted(requests_of(full)),
                    "statuses": sorted({(m.group(1) + m.group(2)) for m in STATUS.finditer(body)}),
                    "exceptions": sorted({m.group(1) for m in EXC.finditer(body)} | {m.group(1) for m in EXC2.finditer(body)}),
                    "calls": sorted({f"{m.group(1)}.{m.group(2)}" for m in CALL.finditer(body)}),
                })
    return res


if __name__ == "__main__":
    json.dump(index(), sys.stdout, ensure_ascii=False, indent=0)
