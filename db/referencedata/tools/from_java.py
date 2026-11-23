"""Write the reference data files from Java's snapshot (ADR-0037 Karar 4).

Usage, from the repository root:

    git -C ../koala-backend show origin/main:src/main/resources/db/changelog/data/<table>.csv > <dir>/<table>.csv
    python3 -I db/referencedata/tools/from_java.py <dir> db/referencedata
    python3 -I db/referencedata/tools/clean.py db/referencedata

<dir> holds Java's six CSV files (koala-backend 0cbbce69). The ids and
names are kept; the audit columns (created_at, updated_at, created_by_id,
updated_by_id) are dropped, and Eminönü, which Java retired in its
2026-09-05-04 changelog (merged into Fatih in 2008), is left out. clean.py
then applies the cleanup. Only the standard library is used.
"""

import csv
import os
import sys

EMINONU = "9b43a8a6-f165-460d-80b1-e711a2db5714"

COLUMNS = {
    "countries": ["id", "code", "name"],
    "cities": ["id", "country_id", "name"],
    "districts": ["id", "city_id", "name"],
    "universities": ["id", "name"],
    "faculties": ["id", "university_id", "name"],
    "departments": ["id", "faculty_id", "name"],
}


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: from_java.py <java data dir> <db/referencedata>")
    src, dst = sys.argv[1], sys.argv[2]
    for table, cols in COLUMNS.items():
        with open(os.path.join(src, table + ".csv"), encoding="utf-8", newline="") as f:
            rows = list(csv.DictReader(f))
        if table == "districts":
            before = len(rows)
            rows = [r for r in rows if r["id"] != EMINONU]
            if before - len(rows) != 1:
                sys.exit("districts.csv: Eminönü not found")
        with open(os.path.join(dst, table + ".csv"), "w", encoding="utf-8", newline="") as f:
            w = csv.writer(f, lineterminator="\n")
            w.writerow(cols)
            for r in rows:
                w.writerow([r[c] for c in cols])
        print(f"{len(rows):6d}  rows in {table}.csv")


if __name__ == "__main__":
    main()
