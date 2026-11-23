"""Clean the reference data files in place (ADR-0037 Karar 4a).

Usage, from the repository root:

    python3 -I db/referencedata/tools/clean.py db/referencedata

The files came from Java's snapshot (0cbbce69). This script applies the
cleanup decided on 23.11.2026, before the first load. It is idempotent:
run on files it already cleaned, it changes nothing and reports zeros.
Each rule prints what it removed or renamed. Only the standard library is
used; -I keeps Python from importing anything next to the files.

Rules, in order:

1. Whitespace: NBSP becomes a space, runs of white space become one space,
   and the ends are trimmed.
2. Districts: "MERKEZ" in the 30 metropolitan provinces (büyükşehir) is
   removed. Law 6360 (2012) and the 2014 local elections turned those
   provinces' central districts into named districts, so no metropolitan
   province has a "Merkez" district any more. The other 51 provinces keep
   theirs. The script stops if the metropolitan list does not name 30 of
   the cities.
3. Faculties: the two rows whose "name" is YÖK's note on the universities
   abroad (Hoca Ahmet Yesevi, Manas) are removed; they are no academic
   unit and have no departments.
4. Faculties: İzmir Yüksek Teknoloji Enstitüsü, listed as a faculty of
   İzmir Katip Çelebi Üniversitesi, is a state university of its own. It
   becomes a row of universities.csv under the same id (no database holds
   the faculty yet: this is the first load).
5. Faculties: a faculty without departments whose sibling (same
   university, same name) has departments is removed.
6. Faculties: a quota or fee suffix ("(Burslu)", "(%50 İndirimli)",
   "(Ücretli)", "(İÖ)", ...; QUOTA below) names a seat type, not a unit.
   YÖK lists the faculty-level programs (Tıp, Hukuk, Diş Hekimliği) once
   per seat type, so "Hukuk Fakültesi (Ücretli)" appears as a faculty.
   Every (university, name without those suffixes) keeps one faculty: the
   one whose name had no such suffix, then one with departments, then the
   smallest id. It takes the name without the suffixes; the departments of
   the others move under it (no database holds them yet: first load), and
   the others are removed.
7. Departments: the same rule per (faculty, name without the suffixes):
   the row without a suffix, else the smallest id, takes the plain name;
   the others are removed.

The language of instruction, distance and open education, joint programs
(UOLP) and campuses stay part of a name: YÖK lists them as programs of
their own, with their own curriculum or diploma.
"""

import csv
import os
import re
import sys

METROPOLITAN = [
    "ADANA", "ANKARA", "ANTALYA", "AYDIN", "BALIKESİR", "BURSA", "DENİZLİ", "DİYARBAKIR", "ERZURUM",
    "ESKİŞEHİR", "GAZİANTEP", "HATAY", "İSTANBUL", "İZMİR", "KAHRAMANMARAŞ", "KAYSERİ", "KOCAELİ", "KONYA",
    "MALATYA", "MANİSA", "MARDİN", "MERSİN", "MUĞLA", "ORDU", "SAKARYA", "SAMSUN", "ŞANLIURFA", "TEKİRDAĞ",
    "TRABZON", "VAN",
]

ABROAD_NOTES = {
    "9c91f25a-d029-452b-8008-768f117bb9e4",  # under HOCA AHMET YESEVİ
    "339d2654-b490-4863-8963-35f4b27980e8",  # under KIRGIZİSTAN-TÜRKİYE MANAS
}

IYTE = "f458e88f-1201-4231-b4f7-7ede110e9c05"
IYTE_NAME = "İZMİR YÜKSEK TEKNOLOJİ ENSTİTÜSÜ"

QUOTA = {
    "Burslu", "%25 İndirimli", "%50 İndirimli", "%75 İndirimli", "Ücretli", "İÖ", "KKTC Uyruklu",
    "M.T.O.K.", "Erkek", "Kız", "Milli Savunma Bakanlığı Adına", "İçişleri Bakanlığı Adına",
}

HEADERS = {
    "countries": ["id", "code", "name"],
    "cities": ["id", "country_id", "name"],
    "districts": ["id", "city_id", "name"],
    "universities": ["id", "name"],
    "faculties": ["id", "university_id", "name"],
    "departments": ["id", "faculty_id", "name"],
}

SUFFIX = re.compile(r"\s*\(([^()]*)\)")


def read(root, table):
    with open(os.path.join(root, table + ".csv"), encoding="utf-8", newline="") as f:
        r = csv.DictReader(f)
        if r.fieldnames != HEADERS[table]:
            sys.exit(f"{table}.csv: header {r.fieldnames}, want {HEADERS[table]}")
        return list(r)


def write(root, table, rows):
    with open(os.path.join(root, table + ".csv"), "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(HEADERS[table])
        for r in rows:
            w.writerow([r[c] for c in HEADERS[table]])


def spaces(name):
    return re.sub(r"\s+", " ", name.replace("\xa0", " ")).strip()


def without_quota(name):
    return spaces(SUFFIX.sub(lambda m: "" if m.group(1) in QUOTA else m.group(0), name))


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: clean.py <db/referencedata>")
    root = sys.argv[1]
    data = {t: read(root, t) for t in HEADERS}
    report = []

    # 1. Whitespace.
    n = 0
    for rows in data.values():
        for r in rows:
            clean = spaces(r["name"])
            if clean != r["name"]:
                r["name"] = clean
                n += 1
    report.append(("names with NBSP or extra white space, normalized", n))

    # 2. MERKEZ in the metropolitan provinces.
    cities = {r["name"]: r["id"] for r in data["cities"]}
    missing = [c for c in METROPOLITAN if c not in cities]
    if len(set(METROPOLITAN)) != 30 or missing:
        sys.exit(f"the metropolitan list must name 30 cities; missing {missing}")
    metro = {cities[c] for c in METROPOLITAN}
    before = len(data["districts"])
    data["districts"] = [r for r in data["districts"] if not (r["name"] == "MERKEZ" and r["city_id"] in metro)]
    report.append(("districts: MERKEZ of a metropolitan province, removed", before - len(data["districts"])))

    # 3. YÖK's note as a faculty name.
    before = len(data["faculties"])
    data["faculties"] = [r for r in data["faculties"] if r["id"] not in ABROAD_NOTES]
    report.append(("faculties: YÖK's note on universities abroad, removed", before - len(data["faculties"])))

    # 4. İYTE is a university.
    moved = [r for r in data["faculties"] if r["id"] == IYTE]
    data["faculties"] = [r for r in data["faculties"] if r["id"] != IYTE]
    if moved:
        data["universities"].append({"id": IYTE, "name": IYTE_NAME})
    report.append(("faculties: İYTE, moved to universities under the same id", len(moved)))

    # 5. Empty twins.
    with_departments = {r["faculty_id"] for r in data["departments"]}
    named = {}
    for r in data["faculties"]:
        named.setdefault((r["university_id"], r["name"]), []).append(r)
    drop = set()
    for rows in named.values():
        if any(r["id"] in with_departments for r in rows):
            drop |= {r["id"] for r in rows if r["id"] not in with_departments}
    before = len(data["faculties"])
    data["faculties"] = [r for r in data["faculties"] if r["id"] not in drop]
    report.append(("faculties: empty twin of a faculty with departments, removed", before - len(data["faculties"])))

    # 6. Faculty variants.
    groups = {}
    for r in data["faculties"]:
        groups.setdefault((r["university_id"], without_quota(r["name"])), []).append(r)
    keep, moved_to = {}, {}
    renamed = 0
    for (_, plain), rows in groups.items():
        chosen = min(rows, key=lambda r: (r["name"] != plain, r["id"] not in with_departments, r["id"]))
        keep[chosen["id"]] = plain
        renamed += chosen["name"] != plain
        for r in rows:
            moved_to[r["id"]] = chosen["id"]
    before = len(data["faculties"])
    kept = []
    for r in data["faculties"]:
        if r["id"] in keep:
            r["name"] = keep[r["id"]]
            kept.append(r)
    data["faculties"] = kept
    reparented = 0
    for r in data["departments"]:
        target = moved_to.get(r["faculty_id"], r["faculty_id"])
        if target != r["faculty_id"]:
            r["faculty_id"] = target
            reparented += 1
    report.append(("faculties: quota or fee variant, removed", before - len(kept)))
    report.append(("faculties: kept row renamed to the unit's name", renamed))
    report.append(("departments: moved under the kept faculty", reparented))

    # 7. Department variants.
    groups = {}
    for r in data["departments"]:
        groups.setdefault((r["faculty_id"], without_quota(r["name"])), []).append(r)
    keep = {}
    renamed = 0
    for (_, plain), rows in groups.items():
        chosen = min(rows, key=lambda r: (r["name"] != plain, r["id"]))
        keep[chosen["id"]] = plain
        renamed += chosen["name"] != plain
    before = len(data["departments"])
    kept = []
    for r in data["departments"]:
        if r["id"] in keep:
            r["name"] = keep[r["id"]]
            kept.append(r)
    data["departments"] = kept
    report.append(("departments: quota or fee variant (or a duplicate after a faculty merge), removed", before - len(kept)))
    report.append(("departments: kept row renamed to the program's name", renamed))

    faculty_ids = {r["id"] for r in data["faculties"]}
    orphans = [r["id"] for r in data["departments"] if r["faculty_id"] not in faculty_ids]
    if orphans:
        sys.exit(f"departments left without their faculty: {orphans[:5]}")

    for table, rows in data.items():
        write(root, table, rows)
    for what, count in report:
        print(f"{count:6d}  {what}")
    for table, rows in data.items():
        print(f"{len(rows):6d}  rows in {table}.csv")


if __name__ == "__main__":
    main()
