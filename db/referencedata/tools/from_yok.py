"""Bring YÖK's associate degree (önlisans) programs into the reference data
files (ADR-0038).

Usage, from the repository root, after VERSION holds the data version the
run writes:

    curl -X POST -H 'Content-Type: application/json' \
        --data '{"filters":{"birimTuruId":47},"page":0,"size":10000,"sortBy":"basariSirasi","direction":"ASC"}' \
        https://yokatlas.yok.gov.tr/api/tercih-kilavuz/search > <dir>/onlisans.json
    python3 -I db/referencedata/tools/from_yok.py <dir>/onlisans.json db/referencedata
    python3 -I db/referencedata/tools/clean.py db/referencedata

    python3 -I db/referencedata/tools/from_yok.py --self-test

The source is YÖK Atlas's list of the programs of the year's ÖSYM guide
(tercih kılavuzu), filtered to associate degree programs (birimTuruId 47):
one record per ÖSYM program code (kilavuzKodu), with YÖK's ids of the
university (universiteId) and of the unit (fymkId, a vocational school or
another unit that teaches associate degree programs). Only the standard
library is used; -I keeps Python from importing anything next to the
downloaded file. clean.py's rules are loaded from this directory by path,
so the two scripts share them.

What the script writes:

- departments.csv: the associate degree programs (degree ASSOCIATE). One
  row per (unit, program name without the quota and fee suffixes; clean.py
  QUOTA): "Anestezi (Burslu)" and "Anestezi (%50 İndirimli)" are one
  program; every ÖSYM code of the merged records goes to
  department_source_codes.csv. A program named as a bachelor's program of
  the same unit gets "(Önlisans)" after its name, as YÖK writes
  "İlahiyat (Önlisans) (Açıköğretim)". Bachelor's programs are not
  touched.
- faculties.csv: the units, named in Turkish title case ("ANKARA HAVACILIK
  MESLEK YÜKSEKOKULU" becomes "Ankara Havacılık Meslek Yüksekokulu").
- universities.csv: the universities the files do not list yet, named as
  YÖK names them without the place in parentheses.
- tools/id_ledger.csv: the id ledger, appended to, never rewritten.

The id ledger (ADR-0038 Karar 6). Every source key the script has met is a
row: key, id, parent, name, the data version that added the row, and its
origin. The keys are "yok:university:<YÖK id>", "yok:unit:<YÖK id>" and
"osym:<ÖSYM code>". The latest row of a key is its current meaning. A new
id is a UUIDv7 ("minted"): it is never derived from an outside code, so a
code ÖSYM gives to another program cannot take the old program's id with
it. A key matched to a row that existed before (a university or a unit of
the Java snapshot) is "matched".

- A key of the ledger gets the ledger's id, as long as it comes under the
  same parent and with the same name (compared without case, circumflex and
  extra space). Otherwise the script stops: a renamed or reused code, or
  one that moved, is decided by a person in tools/id_decisions.csv ("same":
  the same program, renamed, under the same parent; "new": another
  program, a new id). The decision is recorded as a new ledger row, so the
  next run has nothing to decide.
- A program takes the id of any of its codes in the ledger, so one that
  loses the code it was first seen under keeps its id. Codes of one
  program naming two ids stop the script.
- A key the ledger does not have: a university is matched only by
  tools/yok_universities.csv (YÖK id -> our id, or "new"); a unit by
  tools/yok_units.csv, then by the same name under the same university; a
  program by the same name and degree under the same unit; else it gets a
  new id.

An associate degree program the source no longer lists leaves the files
(the sync retires it) with its codes; so does a unit or a university with a
minted id once it has no row under it. Rows of the Java snapshot are never
dropped here.

The script is idempotent: run again on the files it wrote, with the same
source, it changes nothing and appends nothing to the ledger. --self-test
runs the rules above on small made-up data in a temporary directory: the
reused code stops the run, the decisions resolve it, a program keeps its id
without its first code, the ledger only grows.
"""

import csv
import importlib.util
import json
import os
import re
import shutil
import sys
import tempfile
import time
import uuid

HERE = os.path.dirname(os.path.abspath(__file__))

ASSOCIATE = "ASSOCIATE"
UNIT_TYPE_ASSOCIATE = 47
CODE = re.compile(r"^[0-9]{9}$")

LEDGER = "id_ledger.csv"
LEDGER_HEADER = ["key", "id", "parent", "name", "version", "origin"]
DECISIONS = "id_decisions.csv"
DECISIONS_HEADER = ["key", "decision", "note"]
UNIVERSITIES = "yok_universities.csv"
UNIVERSITIES_HEADER = ["yok_id", "yok_name", "university_id", "basis"]
UNITS = "yok_units.csv"
UNITS_HEADER = ["yok_id", "yok_name", "faculty_id", "basis"]

# Words of a unit name kept as YÖK writes them (abbreviations and names).
UPPER_WORDS = {"OSB", "BİL", "KKTC", "ASBÜ", "TOBB", "THK", "SGK"}
# Joining words written in lower case inside a name.
LOWER_WORDS = {"VE", "İLE"}
# Words title case would write wrong (the Turkish dotless I of "AIRBUS").
WORD_EXCEPTIONS = {"AIRBUS": "Airbus"}
# Unit names title case cannot write.
TITLE_EXCEPTIONS = {
    "DOĞUBAYAZIT AHMED-İ HANİ MESLEK YÜKSEKOKULU": "Doğubayazıt Ahmed-i Hani Meslek Yüksekokulu",
    "SÜTÇÜLER PROF.DR.HASAN GÜRBÜZ MESLEK YÜKSEKOKULU": "Sütçüler Prof. Dr. Hasan Gürbüz Meslek Yüksekokulu",
}


def load_clean():
    spec = importlib.util.spec_from_file_location("clean", os.path.join(HERE, "clean.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


sys.dont_write_bytecode = True
clean = load_clean()


class Stop(Exception):
    """A run that must not write: the message says what to decide."""


def uuid7():
    """A UUIDv7 (RFC 9562 §5.7): 48 bits of Unix milliseconds, then random
    bits; written here because uuid.uuid7 is Python 3.14's."""
    ms = time.time_ns() // 1_000_000
    rand = int.from_bytes(os.urandom(10), "big")
    value = (ms & ((1 << 48) - 1)) << 80 | 0x7 << 76 | (rand >> 68 & 0xFFF) << 64 | 0b10 << 62 | (rand & ((1 << 62) - 1))
    return str(uuid.UUID(int=value))


def tr_lower(s):
    return s.replace("I", "ı").replace("İ", "i").lower()


def tr_upper(s):
    return s.replace("i", "İ").replace("ı", "I").upper()


def same_name(a, b):
    """Two names say the same: compared without case, circumflex and extra
    white space ("Yapay Zeka" and "Yapay Zekâ")."""
    def norm(s):
        s = tr_lower(clean.spaces(s))
        return s.translate(str.maketrans("âîû", "aiu"))
    return norm(a) == norm(b)


def title_word(word, first):
    if word in WORD_EXCEPTIONS:
        return WORD_EXCEPTIONS[word]
    if word in UPPER_WORDS or not any(ch.isalpha() for ch in word):
        return word
    if not first and word in LOWER_WORDS:
        return tr_lower(word)
    if word.isupper() or word == tr_upper(word):
        return tr_upper(word[0]) + tr_lower(word[1:])
    return word  # already in mixed case ("(Selçuk)")


def title(name):
    """A unit name in Turkish title case: every word capitalized, "ve" and
    "ile" in lower case, abbreviations kept; inside parentheses and around
    a hyphen too."""
    name = clean.spaces(name)
    if name in TITLE_EXCEPTIONS:
        return TITLE_EXCEPTIONS[name]
    out = []
    for i, token in enumerate(re.split(r"(\s+|[()\-/,.])", name)):
        if not token or re.fullmatch(r"\s+|[()\-/,.]", token):
            out.append(token)
            continue
        out.append(title_word(token, first=not out or i == 0))
    return "".join(out)


def associate_name(name, bachelor_names):
    """An associate degree program named as a bachelor's program of the same
    unit is told apart as YÖK does ("İlahiyat (Önlisans) (Açıköğretim)"):
    "(Önlisans)" after the program's name, before the other parentheses."""
    if name not in bachelor_names:
        return name
    base, sep, rest = name.partition(" (")
    return f"{base} (Önlisans)" + (f" ({rest}" if sep else "")


def university_name(yok_name):
    """YÖK's name of a university without the place in parentheses at its
    end ("MUDANYA ÜNİVERSİTESİ (BURSA)"), as universities.csv names them."""
    return clean.spaces(re.sub(r"\s*\([^()]*\)$", "", yok_name))


def read_table(path, header, missing_ok=False):
    if missing_ok and not os.path.exists(path):
        return []
    with open(path, encoding="utf-8", newline="") as f:
        r = csv.DictReader(f)
        if r.fieldnames != header:
            raise Stop(f"{path}: header {r.fieldnames}, want {header}")
        return list(r)


def write_table(path, header, rows):
    with open(path, "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(header)
        for r in rows:
            w.writerow([r[c] for c in header])


def source_records(path):
    with open(path, encoding="utf-8") as f:
        doc = json.load(f)
    records = doc["content"]
    if doc.get("totalElements") != len(records) or not doc.get("last", False):
        raise Stop(f"{path}: {len(records)} of {doc.get('totalElements')} records; fetch them in one page")
    years = {r["yil"] for r in records}
    if len(years) != 1:
        raise Stop(f"{path}: records of the years {sorted(years)}; want one guide")
    codes = set()
    for r in records:
        for key in ("kilavuzKodu", "universiteId", "universiteAdi", "fymkId", "fymkAdi", "birimAdi"):
            if r.get(key) in (None, ""):
                raise Stop(f"{path}: a record without {key}: {r.get('kilavuzKodu')}")
        code = str(r["kilavuzKodu"])
        if r["birimTuruId"] != UNIT_TYPE_ASSOCIATE:
            raise Stop(f"{path}: {code} is not an associate degree program (birimTuruId {r['birimTuruId']})")
        if not CODE.match(code) or code in codes:
            raise Stop(f"{path}: ÖSYM code {code!r} malformed or listed twice")
        codes.add(code)
    return years.pop(), records


class Ledger:
    """The id ledger: rows are appended, never changed or removed."""

    def __init__(self, path, decisions_path, version):
        self.path = path
        self.version = version
        self.rows = read_table(path, LEDGER_HEADER, missing_ok=True)
        self.before = len(self.rows)
        self.latest = {}
        for r in self.rows:
            if int(r["version"]) > version:
                raise Stop(f"{path}: {r['key']} was added in version {r['version']}, after VERSION {version}")
            self.latest[r["key"]] = r
        self.decisions = {}
        for d in read_table(decisions_path, DECISIONS_HEADER, missing_ok=True):
            if d["decision"] not in ("same", "new"):
                raise Stop(f"{decisions_path}: {d['key']}: decision {d['decision']!r}, want same or new")
            self.decisions[d["key"]] = d["decision"]
        self.used = set()
        self.conflicts = []

    def minted(self):
        return {r["id"] for r in self.rows if r["origin"] == "minted"}

    def lookup(self, key, parent, name):
        """The id the ledger gives key under parent and name; None for a key
        to be given one (unseen, or decided "new"). A conflict without a
        decision is collected and stops the run."""
        r = self.latest.get(key)
        if r is None:
            return None
        if r["parent"] == parent and same_name(r["name"], name):
            return r["id"]
        decision = self.decisions.get(key)
        what = "moved" if r["parent"] != parent else "renamed"
        if decision is None:
            self.conflicts.append(
                f"{key} is {what}: the ledger has {r['name']!r} under {r['parent'] or '-'} ({r['id']}), "
                f"the source {name!r} under {parent or '-'}; decide it in {DECISIONS} (same or new)")
            return None
        self.used.add(key)
        if decision == "new":
            return None
        if r["parent"] != parent:
            self.conflicts.append(f"{key}: decided same, but it moved; a moved row is a new row, decide new")
            return None
        return r["id"]

    def record(self, key, id_, parent, name, origin):
        """Append a row unless the latest one already says this."""
        r = self.latest.get(key)
        if r is not None and r["id"] == id_ and r["parent"] == parent and same_name(r["name"], name):
            return False
        row = {"key": key, "id": id_, "parent": parent, "name": name, "version": str(self.version), "origin": origin}
        self.rows.append(row)
        self.latest[key] = row
        return True

    def stop_on_conflicts(self):
        if self.conflicts:
            raise Stop("the id ledger meets keys to decide:\n  " + "\n  ".join(sorted(set(self.conflicts))))


def run(src, root, tools, out=print):
    """One run: read the source and the files, write the files and the
    ledger. A Stop is raised before anything is written."""
    year, records = source_records(src)
    with open(os.path.join(root, "VERSION"), encoding="utf-8") as f:
        version = int(f.read().strip())
    data = {t: clean.read(root, t) for t in clean.HEADERS}
    ledger = Ledger(os.path.join(tools, LEDGER), os.path.join(tools, DECISIONS), version)
    report = []

    # Universities.
    table = {r["yok_id"]: r for r in read_table(os.path.join(tools, UNIVERSITIES), UNIVERSITIES_HEADER)}
    universities = {r["id"]: r for r in data["universities"]}
    by_name = {r["name"]: r["id"] for r in data["universities"]}
    yok_names = {str(r["universiteId"]): r["universiteAdi"] for r in records}
    university_of = {}
    pending = []
    added_universities = matched_universities = 0
    for yid in sorted(yok_names, key=int):
        key, name = f"yok:university:{yid}", university_name(yok_names[yid])
        uid = ledger.lookup(key, "", name)
        origin = "matched"
        if uid is None:
            entry = table.get(yid)
            if entry is None or entry["yok_name"] != yok_names[yid]:
                ledger.conflicts.append(f"{key} {yok_names[yid]!r}: decide it in {UNIVERSITIES}")
                continue
            if entry["university_id"] == "new":
                if name in by_name:
                    ledger.conflicts.append(f"{key} {name!r} is marked new, but universities.csv has it ({by_name[name]})")
                    continue
                uid, origin = uuid7(), "minted"
            else:
                uid = entry["university_id"]
                if uid not in universities:
                    ledger.conflicts.append(f"{key}: {uid} is not a row of universities.csv")
                    continue
        university_of[yid] = uid
        pending.append((key, uid, "", name, origin))
    ledger.stop_on_conflicts()
    minted = ledger.minted() | {p[1] for p in pending if p[4] == "minted"}
    for key, uid, parent, name, origin in pending:
        if uid not in universities:
            if uid not in minted:
                raise Stop(f"{key}: {uid} is not a row of universities.csv")
            row = {"id": uid, "name": name}
            data["universities"].append(row)
            universities[uid] = row
            by_name[name] = uid
            added_universities += 1
        elif uid in minted:
            universities[uid]["name"] = name
        matched_universities += origin == "matched"
        ledger.record(key, uid, parent, name, origin)
    report.append(("universities: in the source", len(university_of)))
    report.append(("universities: new rows (changed)", added_universities))

    # Units, grouped by (university, title-cased name).
    unit_table = {r["yok_id"]: r for r in read_table(os.path.join(tools, UNITS), UNITS_HEADER)}
    faculties = {r["id"]: r for r in data["faculties"]}
    units = {}
    for r in records:
        fid = str(r["fymkId"])
        key = (university_of[str(r["universiteId"])], r["fymkAdi"])
        if units.setdefault(fid, key) != key:
            raise Stop(f"unit {fid} is listed under two universities or names: {units[fid]}, {key}")
    groups = {}
    for fid, (uid, yok_name) in units.items():
        groups.setdefault((uid, title(yok_name)), []).append(fid)
    faculty_of = {}
    pending = []
    merged_units = 0
    for (uid, name), fids in sorted(groups.items()):
        fids.sort(key=int)
        merged_units += len(fids) - 1
        known = {ledger.lookup(f"yok:unit:{f}", uid, name) for f in fids} - {None}
        if len(known) > 1:
            ledger.conflicts.append(f"units {fids} are one faculty but the ledger gives them {sorted(known)}")
            continue
        chosen, origin = (known.pop(), "matched") if known else (None, "matched")
        if chosen is None:
            listed = {unit_table[f]["faculty_id"] for f in fids if f in unit_table}
            if len(listed) > 1:
                ledger.conflicts.append(f"units {fids} are one faculty but {UNITS} maps them to {sorted(listed)}")
                continue
            if listed:
                chosen = listed.pop()
                if chosen not in faculties or faculties[chosen]["university_id"] != uid:
                    ledger.conflicts.append(f"units {fids}: {chosen} is not a faculty of {uid}")
                    continue
        if chosen is None:
            twins = [r["id"] for r in data["faculties"] if r["university_id"] == uid and r["name"] == name]
            if len(twins) > 1:
                ledger.conflicts.append(f"units {fids}: two faculties named {name!r} under {uid}")
                continue
            if twins:
                chosen = twins[0]
                out(f"unit {fids} {name!r}: matched by name to {chosen}")
        if chosen is None:
            chosen, origin = uuid7(), "minted"
        for f in fids:
            faculty_of[f] = chosen
            pending.append((f"yok:unit:{f}", chosen, uid, name, origin))
    ledger.stop_on_conflicts()
    minted |= {p[1] for p in pending if p[4] == "minted"}
    added_faculties = 0
    for key, fac, uid, name, origin in pending:
        if fac not in faculties:
            if fac not in minted:
                raise Stop(f"{key}: {fac} is not a row of faculties.csv")
            row = {"id": fac, "university_id": uid, "name": name}
            data["faculties"].append(row)
            faculties[fac] = row
            added_faculties += 1
        elif fac in minted:
            faculties[fac]["name"] = name
        ledger.record(key, fac, uid, name, origin)
    report.append(("units: in the source", len(units)))
    report.append(("units: one name under one university, merged", merged_units))
    report.append(("units: new faculties (changed)", added_faculties))

    # Programs, grouped by (faculty, name without the quota suffixes).
    departments = {r["id"]: r for r in data["departments"]}
    bachelor_names = {}
    for d in data["departments"]:
        if d["degree"] != ASSOCIATE:
            bachelor_names.setdefault(d["faculty_id"], set()).add(d["name"])
    programs = {}
    for r in records:
        plain = clean.without_quota(r["birimAdi"])
        programs.setdefault((faculty_of[str(r["fymkId"])], plain), []).append(str(r["kilavuzKodu"]))
    wanted, wanted_codes, pending = {}, {}, []
    told_apart = by_name_count = 0
    for (fac, plain), codes in sorted(programs.items()):
        codes.sort(key=int)
        name = associate_name(plain, bachelor_names.get(fac, set()))
        told_apart += name != plain
        known = {ledger.lookup(f"osym:{c}", fac, name) for c in codes} - {None}
        if len(known) > 1:
            ledger.conflicts.append(f"program {name!r} under {fac}: its codes {codes} name {sorted(known)} in the ledger")
            continue
        chosen, origin = (known.pop(), "matched") if known else (None, "matched")
        if chosen is None:
            twins = [d["id"] for d in data["departments"]
                     if d["faculty_id"] == fac and d["degree"] == ASSOCIATE and d["name"] == name]
            if twins:
                chosen = twins[0]
                by_name_count += 1
                out(f"program {name!r} under {fac}: matched by name to {chosen}")
        if chosen is None:
            chosen, origin = uuid7(), "minted"
        if chosen in wanted:
            ledger.conflicts.append(f"program {name!r} under {fac}: {chosen} is matched twice")
            continue
        wanted[chosen] = {"id": chosen, "faculty_id": fac, "degree": ASSOCIATE, "name": name}
        for c in codes:
            wanted_codes[c] = chosen
            pending.append((f"osym:{c}", chosen, fac, name, origin))
    ledger.stop_on_conflicts()
    added_departments = sum(1 for i in wanted if i not in departments)
    renamed = sum(1 for i, d in wanted.items() if i in departments and departments[i]["name"] != d["name"])
    for p in pending:
        ledger.record(*p)

    before = [d for d in data["departments"] if d["degree"] == ASSOCIATE]
    dropped = sum(1 for d in before if d["id"] not in wanted)
    kept = [d for d in data["departments"] if d["degree"] != ASSOCIATE]
    order = {d["id"]: i for i, d in enumerate(data["departments"])}
    associate = sorted(wanted.values(), key=lambda d: (order.get(d["id"], len(order)), d["faculty_id"], d["name"], d["id"]))
    data["departments"] = kept + associate
    report.append(("programs: records in the source", len(records)))
    report.append(("programs: quota or fee variants merged", len(records) - len(programs)))
    report.append(("programs: named as a bachelor's program of the unit, \"(Önlisans)\" added", told_apart))
    report.append(("programs: matched by name", by_name_count))
    report.append(("programs: new departments (changed)", added_departments))
    report.append(("programs: renamed (changed)", renamed))
    report.append(("programs: no longer listed, dropped (changed)", dropped))

    old_codes = {r["code"]: r["department_id"] for r in data["department_source_codes"]}
    associate_ids = {d["id"] for d in data["departments"] if d["degree"] == ASSOCIATE} | {d["id"] for d in before}
    other = [r for r in data["department_source_codes"]
             if r["code"] not in wanted_codes and r["department_id"] not in associate_ids]
    data["department_source_codes"] = sorted(
        other + [{"code": c, "department_id": d} for c, d in wanted_codes.items()], key=lambda r: r["code"])
    report.append(("source codes: listed", len(data["department_source_codes"])))
    report.append(("source codes: new or moved (changed)", sum(old_codes.get(c) != d for c, d in wanted_codes.items())))
    report.append(("source codes: dropped (changed)",
                   sum(1 for c in old_codes if c not in wanted_codes and c not in {r["code"] for r in other})))

    # Minted units and universities left without rows.
    with_departments = {d["faculty_id"] for d in data["departments"]}
    empty_units = {f["id"] for f in data["faculties"] if f["id"] in minted and f["id"] not in with_departments}
    data["faculties"] = [f for f in data["faculties"] if f["id"] not in empty_units]
    with_faculties = {f["university_id"] for f in data["faculties"]}
    empty_universities = {u["id"] for u in data["universities"] if u["id"] in minted and u["id"] not in with_faculties}
    data["universities"] = [u for u in data["universities"] if u["id"] not in empty_universities]
    report.append(("units with a minted id and no program, dropped (changed)", len(empty_units)))
    report.append(("universities with a minted id and no unit, dropped (changed)", len(empty_universities)))
    report.append(("id ledger: rows appended (changed)", len(ledger.rows) - ledger.before))
    unused = sorted(set(ledger.decisions) - ledger.used)
    if unused:
        out(f"{DECISIONS}: decisions the run did not need (already in the ledger?): {unused}")

    for t in clean.HEADERS:
        clean.write(root, t, data[t])
    write_table(os.path.join(tools, LEDGER), LEDGER_HEADER, ledger.rows)
    out(f"source: YÖK Atlas, ÖSYM guide {year}, {len(records)} associate degree records; data version {version}")
    for what, count in report:
        out(f"{count:6d}  {what}")
    for t in clean.HEADERS:
        out(f"{len(data[t]):6d}  rows in {t}.csv")
    out(f"{len(ledger.rows):6d}  rows in {LEDGER}")
    return dict((what, count) for what, count in report)


def self_test():
    """The ledger's rules on made-up data, in a temporary directory."""
    java_uni, java_fac = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"

    def record(code, unit, unit_name, program):
        return {"kilavuzKodu": code, "universiteId": 1, "universiteAdi": "ÖRNEK ÜNİVERSİTESİ (ANKARA)",
                "fymkId": unit, "fymkAdi": unit_name, "birimAdi": program, "birimTuruId": UNIT_TYPE_ASSOCIATE,
                "yil": 2026}

    myo, other = (10, "ÖRNEK MESLEK YÜKSEKOKULU"), (11, "BAŞKA MESLEK YÜKSEKOKULU")
    base = [record(100000001, *myo, "Anestezi"), record(100000002, *myo, "Anestezi (Burslu)"),
            record(100000003, *myo, "Aşçılık")]

    with tempfile.TemporaryDirectory() as tmp:
        root, tools = os.path.join(tmp, "data"), os.path.join(tmp, "tools")
        os.makedirs(root)
        os.makedirs(tools)
        for t, header in clean.HEADERS.items():
            write_table(os.path.join(root, t + ".csv"), header, [])
        write_table(os.path.join(root, "universities.csv"), clean.HEADERS["universities"],
                    [{"id": java_uni, "name": "ÖRNEK ÜNİVERSİTESİ"}])
        write_table(os.path.join(root, "faculties.csv"), clean.HEADERS["faculties"],
                    [{"id": java_fac, "university_id": java_uni, "name": "Mühendislik Fakültesi"}])
        write_table(os.path.join(root, "departments.csv"), clean.HEADERS["departments"],
                    [{"id": "33333333-3333-4333-8333-333333333333", "faculty_id": java_fac, "degree": "BACHELOR",
                      "name": "Bilgisayar Mühendisliği"}])
        write_table(os.path.join(tools, UNIVERSITIES), UNIVERSITIES_HEADER,
                    [{"yok_id": "1", "yok_name": "ÖRNEK ÜNİVERSİTESİ (ANKARA)", "university_id": java_uni,
                      "basis": "name without place"}])
        write_table(os.path.join(tools, UNITS), UNITS_HEADER, [])
        src = os.path.join(tmp, "source.json")

        def step(version, recs, decisions=(), stops=None):
            with open(os.path.join(root, "VERSION"), "w", encoding="utf-8") as f:
                f.write(f"{version}\n")
            with open(src, "w", encoding="utf-8") as f:
                json.dump({"content": recs, "totalElements": len(recs), "last": True}, f, ensure_ascii=False)
            write_table(os.path.join(tools, DECISIONS), DECISIONS_HEADER,
                        [{"key": k, "decision": d, "note": "self-test"} for k, d in decisions])
            ledger_before = read_table(os.path.join(tools, LEDGER), LEDGER_HEADER, missing_ok=True)
            files_before = {n: open(os.path.join(root, n), "rb").read() for n in os.listdir(root)}
            try:
                run(src, root, tools, out=lambda *_: None)
            except Stop as e:
                if stops is None or stops not in str(e):
                    raise AssertionError(f"version {version}: stopped: {e}") from e
                after = {n: open(os.path.join(root, n), "rb").read() for n in os.listdir(root)}
                assert after == files_before, "a stopped run wrote the files"
                assert read_table(os.path.join(tools, LEDGER), LEDGER_HEADER, missing_ok=True) == ledger_before
                return None
            if stops is not None:
                raise AssertionError(f"version {version}: ran, want a stop on {stops!r}")
            ledger = read_table(os.path.join(tools, LEDGER), LEDGER_HEADER)
            assert ledger[:len(ledger_before)] == ledger_before, "the ledger was rewritten, not appended to"
            return {d["name"]: d["id"] for d in clean.read(root, "departments") if d["degree"] == ASSOCIATE}

        # The first run: two programs (the Burslu variant merged), ids not
        # derived from the codes, the Java university matched.
        first = step(2, base)
        assert sorted(first) == ["Anestezi", "Aşçılık"], first
        for i in first.values():
            assert uuid.UUID(i).version == 7, i
        ledger = read_table(os.path.join(tools, LEDGER), LEDGER_HEADER)
        assert [r["key"] for r in ledger] == [
            "yok:university:1", "yok:unit:10", "osym:100000001", "osym:100000002", "osym:100000003"], ledger
        assert ledger[0]["id"] == java_uni and ledger[0]["origin"] == "matched"
        # Idempotent: the same source writes the same files, appends nothing.
        files = {n: open(os.path.join(root, n), "rb").read() for n in os.listdir(root)}
        assert step(2, base) == first
        assert files == {n: open(os.path.join(root, n), "rb").read() for n in os.listdir(root)}
        assert len(read_table(os.path.join(tools, LEDGER), LEDGER_HEADER)) == 5

        # The plain variant closes; "Burslu" carries the program's id.
        assert step(3, base[1:]) == first

        # ÖSYM gives 100000003 to another program: the run stops.
        reused = base[1:2] + [record(100000003, *myo, "Bilgisayar Programcılığı")]
        step(4, reused, stops="osym:100000003 is renamed")
        # Decided "new": a new id; Aşçılık leaves the files.
        decided = step(4, reused, decisions=[("osym:100000003", "new")])
        assert decided["Anestezi"] == first["Anestezi"]
        assert decided["Bilgisayar Programcılığı"] not in first.values() and "Aşçılık" not in decided, decided

        # The code moves to another unit: the run stops, "same" cannot keep
        # a moved program, "new" gives it a new id.
        moved = [record(100000002, *other, "Anestezi"), reused[1]]
        step(5, moved, stops="osym:100000002 is moved")
        step(5, moved, decisions=[("osym:100000002", "same")], stops="decided same, but it moved")
        after_move = step(5, moved, decisions=[("osym:100000002", "new")])
        assert after_move["Anestezi"] != first["Anestezi"], after_move

        # A rename decided "same" keeps the id and takes the new name.
        renamed = [record(100000002, *other, "Anestezi Teknikerliği"), reused[1]]
        step(6, renamed, stops="osym:100000002 is renamed")
        kept = step(6, renamed, decisions=[("osym:100000002", "same")])
        assert kept["Anestezi Teknikerliği"] == after_move["Anestezi"], kept
        # The decision is in the ledger now: the next run needs none.
        assert step(6, renamed) == kept
        # A name that differs only in case and circumflex is the same name.
        assert step(6, [record(100000002, *other, "ANESTEZİ TEKNİKERLİĞİ"), reused[1]]) is not None
    print("self-test: ok")


def main():
    if sys.argv[1:] == ["--self-test"]:
        self_test()
        return
    if len(sys.argv) != 3:
        sys.exit("usage: from_yok.py <onlisans.json> <db/referencedata> | --self-test")
    try:
        run(sys.argv[1], sys.argv[2], HERE)
    except Stop as e:
        sys.exit(str(e))


if __name__ == "__main__":
    main()
