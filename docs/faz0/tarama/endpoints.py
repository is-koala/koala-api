#!/usr/bin/env python3
"""Java koala-backend: controller uçlarını ve servis çağrılarını çıkarır,
manager metodlarından uçlara geri eşleme sağlar.

Kullanım: python3 -I endpoints.py <backend-src kökü>
Çıktı: JSON (stdout) {"endpoints": [...], "method_to_endpoints": {...}}
"""
import json
import os
import re
import sys

ROOT = sys.argv[1]
JAVA = os.path.join(ROOT, "src/main/java/com/exskylab/koala")

MAP_RE = re.compile(r'@(Get|Post|Put|Patch|Delete|Request)Mapping\s*(\((.*?)\))?\s*$')
METHOD_SIG = re.compile(r'^\s{4}(public|private|protected)?\s*[\w<>\[\],.? ]+\s+(\w+)\s*\(')


def read(p):
    with open(p, encoding="utf-8") as f:
        return f.read().split("\n")


def mapping_path(args):
    if args is None:
        return [""]
    m = re.findall(r'"([^"]*)"', args)
    return m or [""]


def mapping_method(kind, args):
    if kind != "Request":
        return kind.upper()
    m = re.search(r'RequestMethod\.(\w+)', args or "")
    return m.group(1) if m else "ANY"


def impl_index():
    """interface -> concrete class (Manager) adı"""
    idx = {}
    for d in ("business/concretes", "core"):
        for dp, _, fs in os.walk(os.path.join(JAVA, d)):
            for f in fs:
                if not f.endswith(".java"):
                    continue
                txt = "\n".join(read(os.path.join(dp, f)))
                m = re.search(r'class\s+(\w+)[^{]*implements\s+([\w, <>]+)\{', txt)
                if m:
                    for itf in re.split(r'[,\s]+', m.group(2)):
                        itf = re.sub(r'<.*', '', itf).strip()
                        if itf:
                            idx.setdefault(itf, m.group(1))
    return idx


def controllers():
    out = []
    impl = impl_index()
    cdir = os.path.join(JAVA, "webAPI")
    for f in sorted(os.listdir(cdir)):
        if not f.endswith(".java"):
            continue
        lines = read(os.path.join(cdir, f))
        prefix = [""]
        fields = {}
        for i, l in enumerate(lines):
            m = re.match(r'^@RequestMapping\s*\((.*)\)', l.strip())
            if m and l.startswith("@"):
                prefix = mapping_path(m.group(1))
            fm = re.match(r'^\s+private\s+(?:final\s+)?(\w+)(?:<[^>]*>)?\s+(\w+)\s*;', l)
            if fm:
                fields[fm.group(2)] = fm.group(1)
        i = 0
        while i < len(lines):
            l = lines[i].strip()
            m = re.match(r'^@(Get|Post|Put|Patch|Delete|Request)Mapping\s*(?:\((.*)\))?\s*$', l)
            if m and lines[i].startswith("    @"):
                kind, args = m.group(1), m.group(2)
                # çok satırlı annotation
                j = i
                while args is not None and args.count("(") > args.count(")") and j + 1 < len(lines):
                    j += 1
                    args += lines[j].strip()
                http = mapping_method(kind, args)
                paths = mapping_path(args)
                # metot imzası ve gövdesi
                k = j + 1
                while k < len(lines) and not re.match(r'^\s{4}public\s', lines[k]):
                    k += 1
                sig = lines[k] if k < len(lines) else ""
                nm = re.search(r'\s(\w+)\s*\(', sig)
                name = nm.group(1) if nm else "?"
                depth = 0
                body = []
                started = False
                b = k
                while b < len(lines):
                    depth += lines[b].count("{") - lines[b].count("}")
                    if "{" in lines[b]:
                        started = True
                    body.append(lines[b])
                    if started and depth <= 0:
                        break
                    b += 1
                # annotation bloğu: mapping'den geriye, önceki metot sonuna ya da boş satıra kadar
                a = i - 1
                while a > 0 and lines[a].strip() not in ("", "}") and not lines[a].strip().endswith("}"):
                    a -= 1
                ann = " ".join(x.strip() for x in lines[a + 1:k])
                rp = re.search(r'@RequiresPermission(\s*\(([^)]*)\))?', ann)
                requires = None
                if rp:
                    inner = rp.group(2) or ""
                    perms = re.findall(r'StaffPermission\.(\w+)', inner)
                    act = re.search(r'action\s*=\s*"([^"]*)"', inner)
                    requires = {"permissions": perms, "action": act.group(1) if act else ""}
                calls = set()
                for bl in body:
                    for cm in re.finditer(r'\b(\w+)\.(\w+)\s*\(', bl):
                        fld, meth = cm.group(1), cm.group(2)
                        if fld in fields:
                            t = fields[fld]
                            calls.add((impl.get(t, t), meth))
                for p in prefix:
                    for q in paths:
                        out.append({
                            "controller": f,
                            "line": k + 1,
                            "handler": name,
                            "http": http,
                            "path": (p + q) or "/",
                            "calls": sorted(f"{c}.{mm}" for c, mm in calls),
                            "requires": requires,
                        })
                i = b + 1
                continue
            i += 1
    return out


if __name__ == "__main__":
    eps = controllers()
    m2e = {}
    for e in eps:
        for c in e["calls"]:
            m2e.setdefault(c, []).append(f'{e["http"]} {e["path"]}')
    json.dump({"endpoints": eps, "method_to_endpoints": m2e}, sys.stdout, ensure_ascii=False, indent=1)
