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
import io
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
LEDGER_HEADER = ["key", "id", "parent", "name", "version", "origin", "decision"]
DECISIONS = "id_decisions.csv"
DECISIONS_HEADER = ["key", "version", "decision", "ledger_parent", "ledger_name", "source_parent", "source_name", "note"]
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
    """Two names say the same: compared without case and extra white space
    only. A circumflex counts ("kar" and "kâr" are two words), so a name
    that gains or loses one stops the run until it is decided."""
    return tr_lower(clean.spaces(a)) == tr_lower(clean.spaces(b))


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


def csv_line(values):
    buf = io.StringIO()
    csv.writer(buf, lineterminator="").writerow(values)
    return buf.getvalue()


class Ledger:
    """The id ledger: rows are appended, never changed or removed.

    A row's origin is "minted" (the id was made for the key), "matched"
    (the key was bound to a row that existed, or a dropped id came back)
    or "dropped" (the id left the files; the key still names it). Its
    decision is the tools/id_decisions.csv decision that added it, if any.

    A decision holds the state it was made for: the data version, the
    ledger's parent and name of the key, and the source's parent (its
    source key: "yok:unit:<id>", "yok:university:<id>", or empty) and name.
    It is applied only in that version and only to exactly that state;
    a decision of another version, one that matches nothing, or one that
    does not meet the state it names stops the run. Its row in the ledger
    is the record of it, so the file holds the current version's decisions
    only and is emptied with the next VERSION."""

    def __init__(self, path, decisions_path, version):
        self.path = path
        self.version = version
        self.rows = read_table(path, LEDGER_HEADER, missing_ok=True)
        self.before = len(self.rows)
        self.latest, self.previous = {}, {}
        for r in self.rows:
            if int(r["version"]) > version:
                raise Stop(f"{path}: {r['key']} was added in version {r['version']}, after VERSION {version}")
            if r["key"] in self.latest:
                self.previous[r["key"]] = self.latest[r["key"]]
            self.latest[r["key"]] = r
        self.decisions = {}
        for d in read_table(decisions_path, DECISIONS_HEADER, missing_ok=True):
            if d["decision"] not in ("same", "new"):
                raise Stop(f"{decisions_path}: {d['key']}: decision {d['decision']!r}, want same or new")
            if d["version"] != str(version):
                raise Stop(f"{decisions_path}: {d['key']} was decided for version {d['version']}, VERSION is {version}; "
                           "the ledger holds the decisions of earlier versions, so empty the file")
            if d["key"] in self.decisions:
                raise Stop(f"{decisions_path}: {d['key']} decided twice")
            self.decisions[d["key"]] = d
        self.used = set()
        self.applied = {}  # key -> decision, for the rows this run appends
        self.conflicts = []

    def minted(self):
        return {r["id"] for r in self.rows if r["origin"] == "minted"}

    def lookup(self, key, parent, name, source_parent):
        """The id the ledger gives key under parent and name; None for a key
        to be given one (unseen, or decided "new"). source_parent is the
        source key of the parent, as a decision names it. A conflict or a
        decision that does not fit is collected and stops the run."""
        r, d = self.latest.get(key), self.decisions.get(key)
        state = [key, str(self.version), "<same|new>", r["parent"] if r else "", r["name"] if r else "", source_parent, name]
        if r is None:
            if d is not None:
                self.conflicts.append(f"{key}: decided, but the ledger does not have it")
            return None
        if r["parent"] == parent and same_name(r["name"], name):
            if d is not None:
                before = self.previous.get(key)
                done = (r["version"] == str(self.version) and r["decision"] == d["decision"] and before is not None
                        and (d["ledger_parent"], d["ledger_name"]) == (before["parent"], before["name"])
                        and (d["source_parent"], d["source_name"]) == (source_parent, name))
                if not done:
                    self.conflicts.append(f"{key}: decided, but there is nothing to decide (the ledger has "
                                          f"{r['name']!r} under {r['parent'] or '-'}, as the source)")
                    return None
                self.used.add(key)
            return r["id"]
        what = "moved" if r["parent"] != parent else "renamed"
        if d is None:
            self.conflicts.append(
                f"{key} is {what}: the ledger has {r['name']!r} under {r['parent'] or '-'} ({r['id']}), the source "
                f"{name!r} under {source_parent or '-'}; decide it in {DECISIONS}:\n    {csv_line(state + [''])}")
            return None
        if (d["ledger_parent"], d["ledger_name"], d["source_parent"], d["source_name"]) != (
                r["parent"], r["name"], source_parent, name):
            self.conflicts.append(
                f"{key}: the decision does not meet the state it names; the state is:\n    {csv_line(state + [''])}")
            return None
        self.used.add(key)
        self.applied[key] = d["decision"]
        if d["decision"] == "new":
            return None
        if r["parent"] != parent:
            self.conflicts.append(f"{key}: decided same, but it moved; a moved row is a new row, decide new")
            return None
        return r["id"]

    def check_new(self, key, id_):
        """A key decided "new" must not end up with its old id."""
        r = self.latest.get(key)
        if self.applied.get(key) == "new" and r is not None and r["id"] == id_:
            self.conflicts.append(f"{key}: decided new, but its program keeps {id_} through another code")

    def record(self, key, id_, parent, name, origin):
        """Append a row unless the latest one already says this."""
        r = self.latest.get(key)
        if (r is not None and r["origin"] != "dropped" and r["id"] == id_ and r["parent"] == parent
                and same_name(r["name"], name)):
            return False
        if r is not None and r["origin"] == "dropped" and r["id"] == id_:
            origin = "matched"  # a dropped id back in the files
        row = {"key": key, "id": id_, "parent": parent, "name": name, "version": str(self.version),
               "origin": origin, "decision": self.applied.get(key, "")}
        self.rows.append(row)
        if r is not None:
            self.previous[key] = r
        self.latest[key] = row
        return True

    def drop(self, ids, prefix):
        """Record that ids left the files: every key of prefix whose latest
        row names one gets a "dropped" row."""
        n = 0
        for key, r in sorted(self.latest.items()):
            if key.startswith(prefix) and r["id"] in ids and r["origin"] != "dropped":
                row = dict(r, version=str(self.version), origin="dropped", decision="")
                self.rows.append(row)
                self.previous[key] = r
                self.latest[key] = row
                n += 1
        return n

    def unused_decisions(self):
        for key in sorted(set(self.decisions) - self.used):
            self.conflicts.append(f"{key}: decided, but the source does not list it")

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
    added_universities = 0
    for yid in sorted(yok_names, key=int):
        key, name = f"yok:university:{yid}", university_name(yok_names[yid])
        uid = ledger.lookup(key, "", name, "")
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
    for p in pending:
        ledger.check_new(p[0], p[1])
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
        ledger.record(key, uid, parent, name, origin)
    report.append(("universities: in the source", len(university_of)))
    report.append(("universities: new rows (changed)", added_universities))

    # Units, grouped by (university, title-cased name).
    unit_table = {r["yok_id"]: r for r in read_table(os.path.join(tools, UNITS), UNITS_HEADER)}
    faculties = {r["id"]: r for r in data["faculties"]}
    units, unit_university = {}, {}
    for r in records:
        unit_university[str(r["fymkId"])] = str(r["universiteId"])
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
        known = {ledger.lookup(f"yok:unit:{f}", uid, name, f"yok:university:{unit_university[f]}") for f in fids} - {None}
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
        ledger.check_new(key, fac)
    ledger.stop_on_conflicts()
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
        programs.setdefault((faculty_of[str(r["fymkId"])], plain), []).append((str(r["kilavuzKodu"]), str(r["fymkId"])))
    wanted, wanted_codes, pending = {}, {}, []
    told_apart = by_name_count = 0
    for (fac, plain), coded in sorted(programs.items()):
        coded.sort(key=lambda c: int(c[0]))
        codes = [c for c, _ in coded]
        name = associate_name(plain, bachelor_names.get(fac, set()))
        told_apart += name != plain
        known = {ledger.lookup(f"osym:{c}", fac, name, f"yok:unit:{u}") for c, u in coded} - {None}
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
        ledger.check_new(p[0], p[1])
    ledger.unused_decisions()
    ledger.stop_on_conflicts()
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
    report.append(("id ledger: keys of dropped units and universities, marked (changed)",
                   ledger.drop(empty_units, "yok:unit:") + ledger.drop(empty_universities, "yok:university:")))
    report.append(("id ledger: rows appended (changed)", len(ledger.rows) - ledger.before))

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
    """The ledger's rules on made-up data, in a temporary directory. Each
    step names what it shows; a step that should stop must stop on its own
    message and write nothing."""
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

        def ledger():
            return read_table(os.path.join(tools, LEDGER), LEDGER_HEADER, missing_ok=True)

        def latest(key):
            return [r for r in ledger() if r["key"] == key][-1]

        def decide(version, key, decision, source_parent, source_name, state=None):
            """A decision for the ledger's current state of key, or for
            state (parent, name) when given."""
            r = latest(key)
            parent, name = state or (r["parent"], r["name"])
            return {"key": key, "version": str(version), "decision": decision, "ledger_parent": parent,
                    "ledger_name": name, "source_parent": source_parent, "source_name": source_name,
                    "note": "self-test"}

        def files():
            out = {}
            for n in sorted(os.listdir(root)):
                with open(os.path.join(root, n), "rb") as f:
                    out[n] = f.read()
            return out

        def step(what, version, recs, decisions=(), stops=None):
            with open(os.path.join(root, "VERSION"), "w", encoding="utf-8") as f:
                f.write(f"{version}\n")
            with open(src, "w", encoding="utf-8") as f:
                json.dump({"content": recs, "totalElements": len(recs), "last": True}, f, ensure_ascii=False)
            write_table(os.path.join(tools, DECISIONS), DECISIONS_HEADER, list(decisions))
            ledger_before, files_before = ledger(), files()
            try:
                run(src, root, tools, out=lambda *_: None)
            except Stop as e:
                if stops is None or stops not in str(e):
                    raise AssertionError(f"{what}: stopped: {e}") from e
                assert files() == files_before, f"{what}: a stopped run wrote the files"
                assert ledger() == ledger_before, f"{what}: a stopped run wrote the ledger"
                print(f"  ok  {what}: stops ({stops})")
                return None
            if stops is not None:
                raise AssertionError(f"{what}: ran, want a stop on {stops!r}")
            after = ledger()
            assert after[:len(ledger_before)] == ledger_before, f"{what}: the ledger was rewritten, not appended to"
            print(f"  ok  {what}")
            return {d["name"]: d["id"] for d in clean.read(root, "departments") if d["degree"] == ASSOCIATE}

        first = step("v2: first run, the Burslu variant merged, ids not derived", 2, base)
        assert sorted(first) == ["Anestezi", "Aşçılık"], first
        assert all(uuid.UUID(i).version == 7 for i in first.values()), first
        assert [r["key"] for r in ledger()] == [
            "yok:university:1", "yok:unit:10", "osym:100000001", "osym:100000002", "osym:100000003"], ledger()
        assert ledger()[0]["id"] == java_uni and ledger()[0]["origin"] == "matched"
        snapshot = files()
        assert step("v2: a second run changes nothing", 2, base) == first
        assert files() == snapshot and len(ledger()) == 5

        assert step("v3: the plain variant closes, Burslu keeps the id", 3, base[1:]) == first

        reused = base[1:2] + [record(100000003, *myo, "Bilgisayar Programcılığı")]
        step("v4: ÖSYM reuses 100000003", 4, reused, stops="osym:100000003 is renamed")
        unit10 = latest("yok:unit:10")["id"]
        step("v4: a decision for another state", 4, reused,
             [decide(4, "osym:100000003", "new", "yok:unit:10", "Bilgisayar Programcılığı", (unit10, "Aşçılık Programı"))],
             stops="does not meet the state")
        step("v4: a decision of another version", 4, reused,
             [decide(3, "osym:100000003", "new", "yok:unit:10", "Bilgisayar Programcılığı")],
             stops="decided for version 3")
        decision = decide(4, "osym:100000003", "new", "yok:unit:10", "Bilgisayar Programcılığı")
        decided = step("v4: decided new", 4, reused, [decision])
        assert decided["Anestezi"] == first["Anestezi"]
        assert decided["Bilgisayar Programcılığı"] not in first.values() and "Aşçılık" not in decided, decided
        assert latest("osym:100000003")["decision"] == "new"
        assert step("v4: the same decision again changes nothing", 4, reused, [decision]) == decided
        step("v5: the v4 decision left in the file", 5, reused, [decision], stops="decided for version 4")
        step("v5: a decision with nothing to decide", 5, reused,
             [decide(5, "osym:100000002", "same", "yok:unit:10", "Anestezi")], stops="nothing to decide")

        # The stale decision of the review: a "same" kept in the file must
        # not let the code's next program take the id.
        renamed = [base[1], record(100000003, *myo, "Bilgisayar Programcılığı Teknikerliği")]
        kept = step("v5: decided same, a rename", 5, renamed,
                    [decide(5, "osym:100000003", "same", "yok:unit:10", "Bilgisayar Programcılığı Teknikerliği")])
        assert kept["Bilgisayar Programcılığı Teknikerliği"] == decided["Bilgisayar Programcılığı"], kept
        stale = [base[1], record(100000003, *myo, "Aşçılık")]
        step("v6: the v5 same left in the file, ÖSYM reuses the code", 6, stale,
             [decide(5, "osym:100000003", "same", "yok:unit:10", "Bilgisayar Programcılığı Teknikerliği")],
             stops="decided for version 5")
        step("v6: the same rewritten for v6 but for the v5 state", 6, stale,
             [decide(6, "osym:100000003", "same", "yok:unit:10", "Bilgisayar Programcılığı Teknikerliği",
                     (unit10, "Bilgisayar Programcılığı"))], stops="does not meet the state")
        step("v6: no decision", 6, stale, stops="osym:100000003 is renamed")

        # A circumflex counts: "Aşçılık" and "Âşçılık" are two names; case
        # and white space do not count.
        assert step("v6: case and white space only", 6,
                    [base[1], record(100000003, *myo, "BİLGİSAYAR  PROGRAMCILIĞI TEKNİKERLİĞİ")]) is not None
        step("v6: a circumflex", 6, [base[1], record(100000003, *myo, "Bilgisayar Programcılığı Teknikerlîği")],
             stops="osym:100000003 is renamed")

        # Two codes of one program naming two ids: a "same" that merges two
        # programs stops instead of picking one of them.
        merge = [record(100000002, *myo, "Anestezi"), record(100000003, *myo, "Anestezi (Burslu)")]
        step("v7: codes of one program name two ids", 7, merge,
             [decide(7, "osym:100000003", "same", "yok:unit:10", "Anestezi")], stops="in the ledger")

        # The code moves to another unit: "same" cannot keep it, "new" can.
        moved = [record(100000002, *other, "Anestezi"), renamed[1]]
        step("v7: moved", 7, moved, stops="osym:100000002 is moved")
        step("v7: decided same for a move", 7, moved,
             [decide(7, "osym:100000002", "same", "yok:unit:11", "Anestezi")], stops="decided same, but it moved")
        after_move = step("v7: decided new for a move", 7, moved,
                          [decide(7, "osym:100000002", "new", "yok:unit:11", "Anestezi")])
        assert after_move["Anestezi"] != first["Anestezi"], after_move
        unit11 = latest("yok:unit:11")
        assert unit11["origin"] == "minted"

        # Unit 11 closes: it leaves the files and the ledger says so; it
        # comes back under its id.
        closed = step("v8: unit 11 closes", 8, [renamed[1]])
        faculty_ids = {f["id"] for f in clean.read(root, "faculties")}
        assert unit11["id"] not in faculty_ids and "Anestezi" not in closed
        assert latest("yok:unit:11")["origin"] == "dropped" and latest("yok:unit:11")["id"] == unit11["id"]
        back = step("v9: unit 11 comes back", 9, moved)
        assert unit11["id"] in {f["id"] for f in clean.read(root, "faculties")}
        assert latest("yok:unit:11")["origin"] == "matched" and back["Anestezi"] == after_move["Anestezi"], back
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
