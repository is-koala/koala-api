"""Bring YÖK's associate degree (önlisans) programs into the reference data
files (ADR-0038).

Usage, from the repository root:

    curl -X POST -H 'Content-Type: application/json' \
        --data '{"filters":{"birimTuruId":47},"page":0,"size":10000,"sortBy":"basariSirasi","direction":"ASC"}' \
        https://yokatlas.yok.gov.tr/api/tercih-kilavuz/search > <dir>/onlisans.json
    python3 -I db/referencedata/tools/from_yok.py <dir>/onlisans.json db/referencedata
    python3 -I db/referencedata/tools/clean.py db/referencedata

The source is YÖK Atlas's list of the programs of the year's ÖSYM guide
(tercih kılavuzu), filtered to associate degree programs (birimTuruId 47,
"ÖNLİSANS"): one record per ÖSYM program code (kilavuzKodu), with YÖK's
ids of the university (universiteId) and of the unit (fymkId, a vocational
school, meslek yüksekokulu, or another unit that teaches associate degree
programs). Only the standard library is used; -I keeps Python from
importing anything next to the downloaded file. clean.py's rules are
loaded from this directory by path, so the two scripts share them.

What the script writes:

- departments.csv: the associate degree programs (degree ASSOCIATE). One
  row per (unit, program name without the quota and fee suffixes; clean.py
  QUOTA): "Anestezi (Burslu)" and "Anestezi (%50 İndirimli)" are one
  program. Every ÖSYM code of the merged records goes to
  department_source_codes.csv. A program named as a bachelor's program of
  the same unit gets "(Önlisans)" after its name, as YÖK writes
  "İlahiyat (Önlisans) (Açıköğretim)". Bachelor's programs (degree
  BACHELOR) are not touched.
- faculties.csv: the units, named in Turkish title case ("ANKARA HAVACILIK
  MESLEK YÜKSEKOKULU" becomes "Ankara Havacılık Meslek Yüksekokulu"), as
  the faculties of the Java snapshot are.
- universities.csv: the universities the files do not list yet, named as
  YÖK names them without the place in parentheses, as the others are.

Matching, so that a second run, and next year's, keeps every id:

- A university is matched only by tools/yok_universities.csv (YÖK id ->
  our id, or "new"). A university of the source that the table does not
  list, or whose name the table gives differently, stops the script: a
  rename or a new university is decided by a person, in the table.
- A unit is matched, in order: by tools/yok_units.csv (YÖK id -> an
  existing faculty); by the id derived from its YÖK id, if the files have
  it; by the same name under the same university (README, annual update,
  step 1). Two units of one university with one name are one faculty.
- A program is matched by any of its ÖSYM codes in
  department_source_codes.csv; then by the same name and degree under the
  same unit. A code found under another unit stops the script: a moved
  program is a new program with a new id (ADR-0037 Karar 3).
- A new row's id is a UUIDv5 under NAMESPACE: "yok:university:<YÖK id>",
  "yok:unit:<YÖK id>" (the smallest YÖK id of a merged unit),
  "osym:<ÖSYM code>" (the code of the record without a suffix, else the
  smallest code). The same source gives the same ids.

An associate degree program the source no longer lists leaves the files
(the sync retires it) with its codes; so does a unit or a university with
an id this script derived once it has no row under it. Rows of the Java
snapshot are never dropped here.

The script is idempotent: run again on the files it wrote, with the same
source, it changes nothing and its "changed" lines are zero.
"""

import csv
import importlib.util
import json
import os
import re
import sys
import uuid

HERE = os.path.dirname(os.path.abspath(__file__))

# NAMESPACE is uuid5(NAMESPACE_URL, NAMESPACE_SOURCE), written out so the
# ids never depend on the URL again; main checks the two agree.
NAMESPACE = uuid.UUID("c1f995d4-bea2-5c30-870f-ea391ab09716")
NAMESPACE_SOURCE = "https://github.com/is-koala/koala-api/tree/main/db/referencedata"

ASSOCIATE = "ASSOCIATE"
UNIT_TYPE_ASSOCIATE = 47
CODE = re.compile(r"^[0-9]{9}$")

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


clean = load_clean()


def derived(kind, key):
    return str(uuid.uuid5(NAMESPACE, f"{kind}:{key}"))


def tr_lower(s):
    return s.replace("I", "ı").replace("İ", "i").lower()


def tr_upper(s):
    return s.replace("i", "İ").replace("ı", "I").upper()


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


def read_table(path, header):
    with open(path, encoding="utf-8", newline="") as f:
        r = csv.DictReader(f)
        if r.fieldnames != header:
            sys.exit(f"{path}: header {r.fieldnames}, want {header}")
        return list(r)


def source_records(path):
    with open(path, encoding="utf-8") as f:
        doc = json.load(f)
    records = doc["content"]
    if doc.get("totalElements") != len(records) or not doc.get("last", False):
        sys.exit(f"{path}: {len(records)} of {doc.get('totalElements')} records; fetch them in one page")
    years = {r["yil"] for r in records}
    if len(years) != 1:
        sys.exit(f"{path}: records of the years {sorted(years)}; want one guide")
    codes = set()
    for r in records:
        for key in ("kilavuzKodu", "universiteId", "universiteAdi", "fymkId", "fymkAdi", "birimAdi"):
            if r.get(key) in (None, ""):
                sys.exit(f"{path}: a record without {key}: {r.get('kilavuzKodu')}")
        code = str(r["kilavuzKodu"])
        if r["birimTuruId"] != UNIT_TYPE_ASSOCIATE:
            sys.exit(f"{path}: {code} is not an associate degree program (birimTuruId {r['birimTuruId']})")
        if not CODE.match(code) or code in codes:
            sys.exit(f"{path}: ÖSYM code {code!r} malformed or listed twice")
        codes.add(code)
    return years.pop(), records


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: from_yok.py <onlisans.json> <db/referencedata>")
    src, root = sys.argv[1], sys.argv[2]
    if uuid.uuid5(uuid.NAMESPACE_URL, NAMESPACE_SOURCE) != NAMESPACE:
        sys.exit("NAMESPACE is not uuid5(NAMESPACE_URL, NAMESPACE_SOURCE)")
    year, records = source_records(src)
    data = {t: clean.read(root, t) for t in clean.HEADERS}
    report = []

    # Universities, by the table only.
    table = {r["yok_id"]: r for r in read_table(
        os.path.join(HERE, "yok_universities.csv"), ["yok_id", "yok_name", "university_id", "basis"])}
    universities = {r["id"]: r for r in data["universities"]}
    by_name = {r["name"]: r["id"] for r in data["universities"]}
    university_of = {}
    unlisted, renamed = set(), set()
    for r in records:
        yid = str(r["universiteId"])
        entry = table.get(yid)
        if entry is None:
            unlisted.add(f"{yid} {r['universiteAdi']}")
            continue
        if entry["yok_name"] != r["universiteAdi"]:
            renamed.add(f"{yid}: the table says {entry['yok_name']!r}, YÖK {r['universiteAdi']!r}")
            continue
        university_of[yid] = entry
    if unlisted or renamed:
        sys.exit("tools/yok_universities.csv must decide these universities:\n  " + "\n  ".join(sorted(unlisted | renamed)))
    added_universities = 0
    yok_names = {}
    for r in records:
        yok_names[str(r["universiteId"])] = r["universiteAdi"]
    for yid, entry in sorted(university_of.items()):
        if entry["university_id"] == "new":
            uid = derived("yok:university", yid)
            name = university_name(yok_names[yid])
            if name in by_name and by_name[name] != uid:
                sys.exit(f"university {yid} {name!r} is marked new, but universities.csv has it ({by_name[name]})")
            if uid not in universities:
                row = {"id": uid, "name": name}
                data["universities"].append(row)
                universities[uid] = row
                by_name[name] = uid
                added_universities += 1
            entry["id"] = uid
        else:
            if entry["university_id"] not in universities:
                sys.exit(f"university {yid}: {entry['university_id']} is not a row of universities.csv")
            entry["id"] = entry["university_id"]
    report.append(("universities: in the source", len(university_of)))
    report.append(("universities: matched to a row", sum(e["university_id"] != "new" for e in university_of.values())))
    report.append(("universities: new rows (changed)", added_universities))

    # Units: grouped by (university, title-cased name).
    unit_table = {r["yok_id"]: r for r in read_table(
        os.path.join(HERE, "yok_units.csv"), ["yok_id", "yok_name", "faculty_id", "basis"])}
    faculties = {r["id"]: r for r in data["faculties"]}
    units = {}  # fymkId -> (university id, YÖK name)
    for r in records:
        fid = str(r["fymkId"])
        key = (university_of[str(r["universiteId"])]["id"], r["fymkAdi"])
        if units.setdefault(fid, key) != key:
            sys.exit(f"unit {fid} is listed under two universities or names: {units[fid]}, {key}")
    groups = {}
    for fid, (uid, yok_name) in units.items():
        groups.setdefault((uid, title(yok_name)), []).append(fid)
    faculty_of = {}  # fymkId -> faculty id
    added_faculties = matched_table = matched_id = matched_name = merged_units = 0
    for (uid, name), fids in sorted(groups.items()):
        fids.sort(key=int)
        merged_units += len(fids) - 1
        chosen = None
        listed = {unit_table[f]["faculty_id"] for f in fids if f in unit_table}
        if len(listed) > 1:
            sys.exit(f"units {fids} are one faculty but tools/yok_units.csv maps them to {sorted(listed)}")
        if listed:
            chosen = listed.pop()
            for f in fids:
                if f in unit_table and unit_table[f]["yok_name"] != units[f][1]:
                    sys.exit(f"unit {f}: tools/yok_units.csv says {unit_table[f]['yok_name']!r}, YÖK {units[f][1]!r}")
            if chosen not in faculties or faculties[chosen]["university_id"] != uid:
                sys.exit(f"units {fids}: {chosen} is not a faculty of {uid}")
            matched_table += 1
        if chosen is None:
            chosen = next((derived("yok:unit", f) for f in fids if derived("yok:unit", f) in faculties), None)
            if chosen is not None:
                if faculties[chosen]["university_id"] != uid:
                    sys.exit(f"units {fids}: {chosen} is under another university; a moved unit needs a decision")
                matched_id += 1
        if chosen is None:
            twins = [r["id"] for r in data["faculties"] if r["university_id"] == uid and r["name"] == name]
            if len(twins) > 1:
                sys.exit(f"units {fids}: two faculties named {name!r} under {uid}")
            if twins:
                chosen = twins[0]
                matched_name += 1
                print(f"unit {fids} {name!r}: matched by name to {chosen}")
        if chosen is None:
            chosen = derived("yok:unit", fids[0])
            row = {"id": chosen, "university_id": uid, "name": name}
            data["faculties"].append(row)
            faculties[chosen] = row
            added_faculties += 1
        for f in fids:
            faculty_of[f] = chosen
    report.append(("units: in the source", len(units)))
    report.append(("units: one name under one university, merged", merged_units))
    report.append(("units: matched by tools/yok_units.csv", matched_table))
    report.append(("units: matched by their derived id", matched_id))
    report.append(("units: matched by name", matched_name))
    report.append(("units: new faculties (changed)", added_faculties))

    # Programs: grouped by (faculty, name without the quota suffixes).
    departments = {r["id"]: r for r in data["departments"]}
    code_owner = {r["code"]: r["department_id"] for r in data["department_source_codes"]}
    programs = {}
    for r in records:
        plain = clean.without_quota(r["birimAdi"])
        programs.setdefault((faculty_of[str(r["fymkId"])], plain), []).append(r)
    bachelor_names = {}
    for d in data["departments"]:
        if d["degree"] != ASSOCIATE:
            bachelor_names.setdefault(d["faculty_id"], set()).add(d["name"])
    wanted = {}  # department id -> row
    wanted_codes = {}  # code -> department id
    added_departments = matched_code = matched_dept_name = renamed_departments = told_apart = 0
    for (fac, plain), rs in sorted(programs.items()):
        name = associate_name(plain, bachelor_names.get(fac, set()))
        told_apart += name != plain
        codes = sorted(str(r["kilavuzKodu"]) for r in rs)
        owners = {code_owner[c] for c in codes if c in code_owner}
        if len(owners) > 1:
            sys.exit(f"program {plain!r} under {fac}: its codes {codes} name the departments {sorted(owners)}")
        chosen = owners.pop() if owners else None
        if chosen is not None:
            d = departments.get(chosen)
            if d is None or d["faculty_id"] != fac or d["degree"] != ASSOCIATE:
                sys.exit(f"program {plain!r} ({codes}): {chosen} is under another unit or degree; "
                         "a moved program is a new program with a new id")
            matched_code += 1
        else:
            twins = [d["id"] for d in data["departments"]
                     if d["faculty_id"] == fac and d["degree"] == ASSOCIATE and d["name"] == name]
            if twins:
                chosen = twins[0]
                matched_dept_name += 1
        if chosen is None:
            plain_codes = [str(r["kilavuzKodu"]) for r in rs if clean.spaces(r["birimAdi"]) == plain]
            chosen = derived("osym", min(plain_codes or codes, key=int))
            if chosen in departments:
                sys.exit(f"program {plain!r}: the derived id {chosen} is taken")
            added_departments += 1
        elif departments[chosen]["name"] != name:
            renamed_departments += 1
        if chosen in wanted:
            sys.exit(f"program {plain!r}: {chosen} is matched twice")
        wanted[chosen] = {"id": chosen, "faculty_id": fac, "degree": ASSOCIATE, "name": name}
        for c in codes:
            wanted_codes[c] = chosen
    variants = len(records) - len(programs)

    before = {d["id"]: d for d in data["departments"] if d["degree"] == ASSOCIATE}
    dropped_departments = [i for i in before if i not in wanted]
    kept = [d for d in data["departments"] if d["degree"] != ASSOCIATE]
    order = {d["id"]: i for i, d in enumerate(data["departments"])}
    associate = sorted(wanted.values(), key=lambda d: (order.get(d["id"], len(order)), d["faculty_id"], d["name"], d["id"]))
    data["departments"] = kept + associate
    report.append(("programs: records in the source", len(records)))
    report.append(("programs: quota or fee variants merged", variants))
    report.append(("programs: named as a bachelor's program of the unit, \"(Önlisans)\" added", told_apart))
    report.append(("programs: matched by a source code", matched_code))
    report.append(("programs: matched by name", matched_dept_name))
    report.append(("programs: new departments (changed)", added_departments))
    report.append(("programs: renamed (changed)", renamed_departments))
    report.append(("programs: no longer listed, dropped (changed)", len(dropped_departments)))

    # Source codes: exactly the codes of the programs above, and the codes of
    # departments this script does not manage (none today).
    old_codes = {r["code"]: r["department_id"] for r in data["department_source_codes"]}
    other = [r for r in data["department_source_codes"]
             if r["code"] not in wanted_codes and departments.get(r["department_id"], {}).get("degree") != ASSOCIATE]
    new_codes = [{"code": c, "department_id": d} for c, d in wanted_codes.items()]
    data["department_source_codes"] = sorted(other + new_codes, key=lambda r: r["code"])
    changed_codes = sum(old_codes.get(c) != d for c, d in wanted_codes.items())
    dropped_codes = sum(1 for c in old_codes if c not in wanted_codes and c not in {r["code"] for r in other})
    report.append(("source codes: listed", len(data["department_source_codes"])))
    report.append(("source codes: new (changed)", changed_codes))
    report.append(("source codes: dropped (changed)", dropped_codes))

    # Derived units and universities left without rows.
    with_departments = {d["faculty_id"] for d in data["departments"]}
    derived_units = {derived("yok:unit", f) for f in units} | {
        f["id"] for f in data["faculties"] if uuid.UUID(f["id"]).version == 5}
    empty_units = [f["id"] for f in data["faculties"] if f["id"] in derived_units and f["id"] not in with_departments]
    data["faculties"] = [f for f in data["faculties"] if f["id"] not in empty_units]
    with_faculties = {f["university_id"] for f in data["faculties"]}
    empty_universities = [u["id"] for u in data["universities"]
                          if uuid.UUID(u["id"]).version == 5 and u["id"] not in with_faculties]
    data["universities"] = [u for u in data["universities"] if u["id"] not in empty_universities]
    report.append(("units with a derived id and no program, dropped (changed)", len(empty_units)))
    report.append(("universities with a derived id and no unit, dropped (changed)", len(empty_universities)))
    unused = sorted(set(table) - set(university_of))
    if unused:
        print(f"tools/yok_universities.csv rows the source does not list: {unused}")

    for t in clean.HEADERS:
        clean.write(root, t, data[t])
    print(f"source: YÖK Atlas, ÖSYM guide {year}, {len(records)} associate degree records")
    for what, count in report:
        print(f"{count:6d}  {what}")
    for t in clean.HEADERS:
        print(f"{len(data[t]):6d}  rows in {t}.csv")


if __name__ == "__main__":
    main()
