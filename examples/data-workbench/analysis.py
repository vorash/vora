# Column statistics for a CSV file — an untrusted colleague's "quick script".
import csv
import sys

with open(sys.argv[1], newline="") as f:
    rows = list(csv.DictReader(f))

for column in rows[0]:
    values = [float(r[column]) for r in rows if r[column].replace(".", "").isdigit()]
    if values:
        print(f"{column}: n={len(values)} mean={sum(values) / len(values):.2f} max={max(values)}")
