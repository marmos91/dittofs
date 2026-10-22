#!/usr/bin/env python3
"""Scores a run of night.sh: score.py RUN_DIR REPO.

Reads the fio results under RUN_DIR/r<repeat>/<target>/ and writes RUN_DIR/points.csv (one row per
point, target and repeat) and RUN_DIR/summary.json (CTRF: the steps are its tests, the score is in
its extra), then prints the Slack summary: the first line is the message, the rest its details.
RUN_DIR/alert says what the night means for the Slack alert (night.sh post): new, a flag or a
failure the 7 nights before did not have; same, only ones they had; clear, none.

A point's value on a target is the median of its repeats. Its ratio is DittoFS's performance over
Samba's: a throughput as it is, a time or a latency inverted, so above 1 DittoFS is faster. Each
area's score is the geometric mean of its points' ratios, and the headline is the geometric mean of
the four areas' scores, so each area weighs the same. An area is complete when every step feeding
it finished and each of its points has a value above zero on both targets in every repeat. Without
all four complete there is no headline: the night says so, and stores none for later nights.

Flags need a noise band, which is set once from the calibration nights (CALIBRATION=yes) and kept
in band.json beside the nights: a point, an area or the headline is flagged when it falls below
its 7-day median by more than its band, and a flag or a failure seen in the 7 nights before is not
reported as new. Delete band.json when the scoring changes, and the next night sets it again.
"""
import csv
import glob
import json
import math
import os
import re
import statistics
import subprocess
import sys
import time

TARGETS = ("dittofs", "samba")
AREAS = ("sequential", "random", "small files", "mailbox")
# The areas each step of night.sh feeds: one that did not finish leaves them incomplete.
STEP_AREAS = {"lock": AREAS, "build": AREAS, "start": AREAS, "matrix": ("sequential", "random"),
              "mailbox": ("mailbox",), "files": ("small files",)}
MIN_BAND = 0.05
FILE_SIZES = {"4k": 4096, "64k": 65536, "1792k": 1792 * 1024}
# The mailbox scenarios: how each one scores (see the simulator's README).
MAILBOX = {
    "logon-outlook-start": "wall",
    "mail-flow": "p99",
    "folder-resort": "wall",
    "search-reindex": "ost-read",
    "ost-full-sync": "p99",
    "logoff-compaction": "wall",
}
# What the summary shows of each area beside its score: {point} is DittoFS vs Samba on that point.
FIGURES = {
    "sequential": "1M x 8 threads, read {seq-read-1m-t8}, write {seq-write-1m-t8}",
    "random": "4K x 1 thread, read {rand-read-4k-t1}, write {rand-write-4k-t1}; "
              "4K x 8 threads, read {rand-read-4k-t8}, write {rand-write-4k-t8}",
    "small files": "4K create {files-4k-create}, read {files-4k-read}; 64K create {files-64k-create}, "
                   "read {files-64k-read}; 1.75M create {files-1792k-create}, read {files-1792k-read}",
    "mailbox": "16 users, search re-index {mailbox-search-reindex-u16}, "
               "mail flow p99 {mailbox-mail-flow-u16} ms, OST full sync p99 {mailbox-ost-full-sync-u16} ms",
}


def load(path):
    with open(path) as f:
        return json.load(f)


def errors(doc):
    return [j["jobname"] for j in doc["jobs"] if j.get("error")]


def p99_ms(doc):
    """The 99th percentile latency of every I/O in a json+ result, reads and writes together."""
    bins = {}
    for job in doc["jobs"]:
        for side in ("read", "write"):
            lat = job[side].get("lat_ns") or job[side].get("clat_ns") or {}
            for ns, n in (lat.get("bins") or {}).items():
                bins[int(ns)] = bins.get(int(ns), 0) + n
    total, seen = sum(bins.values()), 0
    for ns in sorted(bins):
        seen += bins[ns]
        if seen >= 0.99 * total:
            return ns / 1e6
    return None


def measure(target_dir):
    """Every point of one target in one repeat: {name: (area, value, unit, higher_is_better)}, failures."""
    points, failures = {}, []
    for path in sorted(glob.glob(f"{target_dir}/matrix/*.json")):
        name = os.path.basename(path)[:-5]
        if name == "layout":
            continue
        doc = load(path)
        failures += errors(doc)
        side = "read" if "-read-" in name else "write"
        area = "sequential" if name.startswith("seq-") else "random"
        points[name] = (area, doc["jobs"][0][side]["bw_bytes"] / 2**20, "MiB/s", True)
    for path in sorted(glob.glob(f"{target_dir}/files/*.json")):
        name = os.path.basename(path)[:-5]
        doc = load(path)
        failures += errors(doc)
        size = FILE_SIZES[name.split("-")[1]]
        side = "write" if name.endswith("-create") else "read"
        points[name] = ("small files", doc["jobs"][0][side]["bw_bytes"] / size, "files/s", True)
    walls = {}
    if os.path.exists(f"{target_dir}/mailbox/wall.tsv"):
        with open(f"{target_dir}/mailbox/wall.tsv") as f:
            walls = dict(line.rstrip("\n").split("\t") for line in f if line.strip())
    for path in sorted(glob.glob(f"{target_dir}/mailbox/[0-9][0-9]-*.json")):
        m = re.match(r"\d\d-(.+)-u(\d+)-\d{8}-\d{6}\.json$", os.path.basename(path))
        if not m or m.group(1) not in MAILBOX:
            continue
        name = f"mailbox-{m.group(1)}-u{m.group(2)}"
        doc = load(path)
        failures += [f"{name} {j}" for j in errors(doc)]
        how = MAILBOX[m.group(1)]
        if how == "wall" and name in walls:
            points[name] = ("mailbox", float(walls[name]), "s", False)
        elif how == "p99" and p99_ms(doc):
            points[name] = ("mailbox", p99_ms(doc), "ms p99", False)
        elif how == "ost-read":
            bw = sum(j["read"]["bw_bytes"] for j in doc["jobs"] if j["jobname"] == "ost-read")
            points[name] = ("mailbox", bw / 2**20, "MiB/s", True)
    return points, failures


def geomean(values):
    """The geometric mean, or None when there is nothing to average or a value is missing or not above zero."""
    values = list(values)
    if not values or any(v is None or v <= 0 for v in values):
        return None
    return math.exp(sum(map(math.log, values)) / len(values))


def headline(ratios, area_of, complete=AREAS):
    """Each complete area's score and the headline, from {point: ratio}: None for an area that is
    not complete, and for the headline unless all four are."""
    areas = {a: geomean(r for p, r in ratios.items() if area_of[p] == a) if a in complete else None for a in AREAS}
    return areas, geomean(areas.values())


def label(name):
    if name == "score":
        return "the overall figure"
    if name in AREAS:
        return f"the {name} area"
    m = re.match(r"(seq|rand)-(read|write)-(\w+)-t(\d+)$", name)
    if m:
        kind = "sequential" if m.group(1) == "seq" else "random"
        n = int(m.group(4))
        return f"{kind} {m.group(2)} {m.group(3).upper()} x {n} thread{'s' if n > 1 else ''}"
    m = re.match(r"files-(\w+)-(create|read)$", name)
    if m:
        size = "1.75M" if m.group(1) == "1792k" else m.group(1).upper()
        return f"{'creating' if m.group(2) == 'create' else 'reading'} {size} files"
    m = re.match(r"mailbox-(.+)-u(\d+)$", name)
    if m:
        return f"mailbox {m.group(1).split('-')[0]}, {m.group(2)} user{'s' if m.group(2) != '1' else ''}"
    return name


def pct(x):
    return f"{x * 100:.0f}%" if x is not None else "n/a"


def figure(name, points):
    """DittoFS vs Samba on a point, each the median of its repeats, files/s as MiB/s (times the file
    size) and the rest in its own unit; n/a for a point without a value."""
    if name not in points:
        return "n/a"
    p = points[name]
    scale = FILE_SIZES[name.split("-")[1]] / 2**20 if p["unit"] == "files/s" else 1
    values = (statistics.median(p[t]) * scale for t in TARGETS)
    return " vs ".join(f"{v:.0f}" if v >= 10 else f"{v:.2g}" for v in values)


def history(work, night):
    """The summaries of the nights before this one, oldest first."""
    out = []
    for path in sorted(glob.glob(f"{work}/20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]/summary.json")):
        if os.path.basename(os.path.dirname(path)) < night:
            out.append(load(path)["results"]["extra"])
    return out


def band(work, nights):
    """The noise band per point, per area and for the headline, from the calibration nights'
    repeats: each repeat's areas and headline come from its points' ratios through headline()."""
    path = f"{work}/band.json"
    if os.path.exists(path):
        return load(path)
    calibration = [n for n in nights if n.get("mode") == "calibration"]
    if not calibration:
        return None
    spreads = {}
    for n in calibration:
        points = n["points"]
        for name, p in points.items():
            spreads.setdefault(name, []).extend(p["repeat_ratios"])
        repeats = {len(p["repeat_ratios"]) for p in points.values()}
        if len(repeats) != 1:
            continue
        complete = [a for a in AREAS if n["areas"].get(a) is not None]
        for i in range(repeats.pop()):
            areas, score = headline({name: p["repeat_ratios"][i] for name, p in points.items()},
                                    {name: p["area"] for name, p in points.items()}, complete)
            for name, v in list(areas.items()) + [("score", score)]:
                if v is not None:
                    spreads.setdefault(name, []).append(v)
    result = {}
    for name, ratios in spreads.items():
        mid = statistics.median(ratios)
        dev = sorted(abs(r / mid - 1) for r in ratios)
        result[name] = max(MIN_BAND, dev[int(0.9 * (len(dev) - 1))])
    with open(path, "w") as f:
        json.dump(result, f, indent=1, sort_keys=True)
    return result


def drive_wear(run):
    """Per drive: GB written during the run and the wear indicator, from nvme smart-log."""
    out = {}
    for after in glob.glob(f"{run}/nvme-after-*.json"):
        dev = after.rsplit("-", 1)[1][:-5]
        try:
            a, b = load(after), load(f"{run}/nvme-before-{dev}.json")
            # A data unit is 1,000 blocks of 512 bytes.
            written = (a["data_units_written"] - b["data_units_written"]) * 512000 / 1e9
            out[dev] = {"written_gb": round(written, 1), "percent_used": a.get("percent_used", a.get("percentage_used"))}
        except (OSError, KeyError, ValueError):
            continue
    return out


def main():
    run, repo = os.path.abspath(sys.argv[1]), sys.argv[2]
    work, night = os.path.dirname(run), os.path.basename(run)
    mode = "smoke" if os.environ.get("SMOKE") == "yes" else "calibration" if os.environ.get("CALIBRATION") == "yes" else "night"

    steps = []
    with open(f"{run}/steps.tsv") as f:
        steps = list(csv.DictReader(f, delimiter="\t"))
    failures = [f"{s['step']} {s['target']} r{s['repeat']}: {s['status']}" for s in steps if s["status"] != "ok"]

    # values[name][target] = [value per repeat]; meta[name] = (area, unit, higher_is_better)
    values, meta, rows = {}, {}, []
    for rdir in sorted(glob.glob(f"{run}/r[0-9]*")):
        repeat = rdir.rsplit("/r", 1)[1]
        for target in TARGETS:
            points, failed = measure(f"{rdir}/{target}")
            failures += [f"fio {target} r{repeat}: {j} error" for j in failed]
            for name, (area, value, unit, higher) in points.items():
                values.setdefault(name, {t: {} for t in TARGETS})[target][repeat] = value
                meta[name] = (area, unit, higher)
                rows.append([night, repeat, target, name, area, f"{value:.4g}", unit, "higher" if higher else "lower"])
    with open(f"{run}/points.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["night", "repeat", "target", "point", "area", "value", "unit", "better"])
        w.writerows(rows)

    def perf(name, v):
        return v if meta[name][2] else 1 / v

    # Why each incomplete area is: a step that did not finish, or a point without a value above
    # zero on a target in some repeat. An area with no points at all is incomplete too.
    repeats = sorted(rdir.rsplit("/r", 1)[1] for rdir in glob.glob(f"{run}/r[0-9]*"))
    incomplete = {}
    for s in steps:
        for a in STEP_AREAS.get(s["step"], ()) if s["status"] != "ok" else ():
            incomplete.setdefault(a, f"{s['step']} {s['target']} r{s['repeat']}: {s['status']}")
    points = {}
    for name, by in values.items():
        missing = [t for t in TARGETS for r in repeats if not by[t].get(r, 0) > 0]
        if missing:
            incomplete.setdefault(meta[name][0], f"{label(name)} has no value on {missing[0]}")
            continue
        d, s = by["dittofs"], by["samba"]
        points[name] = {"area": meta[name][0], "unit": meta[name][1], "better": "higher" if meta[name][2] else "lower",
                        "dittofs": [d[r] for r in repeats], "samba": [s[r] for r in repeats],
                        "ratio": perf(name, statistics.median(d.values())) / perf(name, statistics.median(s.values())),
                        "repeat_ratios": [perf(name, d[r]) / perf(name, s[r]) for r in repeats]}
    for a in AREAS:
        if not any(p["area"] == a for p in points.values()):
            incomplete.setdefault(a, "no points")
    areas, score = headline({n: p["ratio"] for n, p in points.items()}, {n: p["area"] for n, p in points.items()},
                            [a for a in AREAS if a not in incomplete])

    nights = history(work, night)[-7:] if mode != "smoke" else []
    bands = band(work, nights) if mode == "night" else None
    seen_flags = {f for n in nights for f in n.get("flags", [])}
    seen_failures = {re.sub(r" r\d+:", ":", f) for n in nights for f in n.get("failures", [])}
    flags = []
    if bands and nights:
        now = {name: p["ratio"] for name, p in points.items()} | {a: v for a, v in areas.items() if v is not None}
        if score is not None:
            now["score"] = score
        for name, v in now.items():
            past = [n["score"] if name == "score" else n["areas"].get(name) if name in AREAS
                    else n["points"].get(name, {}).get("ratio") for n in nights]
            past = [x for x in past if x is not None]
            if past and name in bands and v < statistics.median(past) * (1 - bands[name]):
                flags.append(name)
    failures += [f"{a}: incomplete, {why}" for a, why in incomplete.items()]
    new_flags = [f for f in flags if f not in seen_flags]
    new_failures = [f for f in failures if re.sub(r" r\d+:", ":", f) not in seen_failures]

    with open(f"{run}/alert", "w") as f:
        f.write("new\n" if new_flags or new_failures else "same\n" if flags or failures else "clear\n")

    commit = open(f"{run}/commit").read().strip() if os.path.exists(f"{run}/commit") else ""
    since = next((n.get("commit") for n in reversed(nights) if n.get("commit")), "")
    commits = ""
    if commit and since and since != commit:
        count = subprocess.run(["git", "-C", repo, "rev-list", "--count", f"{since}..{commit}"],
                               capture_output=True, text=True).stdout.strip()
        commits = f" ({count or '?'} commits since `{since[:9]}`)"

    harness = open(f"{run}/harness").read().strip() if os.path.exists(f"{run}/harness") else ""
    extra = {"night": night, "mode": mode, "commit": commit, "harness": harness, "score": score, "areas": areas, "points": points,
             "flags": flags, "failures": failures, "drives": drive_wear(run)}
    tests = [{"name": f"{s['step']} {s['target']} r{s['repeat']}".replace(" - r-", ""), "suite": ["ditto-nightly"],
              "status": {"ok": "passed", "skipped": "skipped"}.get(s["status"], "failed"), "rawStatus": s["status"],
              "duration": int(float(s["seconds"]) * 1000)} for s in steps]
    started = int(open(f"{run}/started").read()) * 1000 if os.path.exists(f"{run}/started") else 0
    summary = {"tests": len(tests), "passed": sum(t["status"] == "passed" for t in tests),
               "failed": sum(t["status"] == "failed" for t in tests), "skipped": sum(t["status"] == "skipped" for t in tests),
               "pending": 0, "other": 0, "start": started, "stop": int(time.time() * 1000)}
    with open(f"{run}/summary.json", "w") as f:
        json.dump({"reportFormat": "CTRF", "specVersion": "0.0.0", "generatedBy": "bench/ditto/score.py",
                   "results": {"tool": {"name": "ditto-nightly"}, "summary": summary, "tests": tests,
                               "environment": {"commit": commit, "branchName": "develop", "osPlatform": "linux"},
                               "extra": extra}}, f, indent=1)

    # The Slack summary.
    if score is None:
        title = f":x: Nightly performance: incomplete, no figure tonight ({', '.join(a for a in AREAS if a in incomplete)})"
    else:
        past = [n["score"] for n in nights if n.get("score") is not None]
        trend = f", 7-day median {pct(statistics.median(past))}" if past else ""
        if nights and nights[-1].get("score") is not None:
            trend = f"; was {pct(nights[-1]['score'])} last night{trend}"
        elif trend:
            trend = ";" + trend[1:]
        mark = ":warning: " if new_flags or new_failures else ""
        title = f"{mark}DittoFS at {pct(score)} of Samba (geometric mean of the 4 areas, {len(points)} points{trend})"
    lines = []
    if points:
        worst = min(points, key=lambda n: points[n]["ratio"])
        line = f"Worst: {label(worst)} at {pct(points[worst]['ratio'])}."
        logons = sorted((n for n in points if n.startswith("mailbox-logon")), key=lambda n: int(n.rsplit("-u", 1)[1]))
        if logons:
            n = logons[-1]
            line += f" Mailbox, {n.rsplit('-u', 1)[1]} users: logon {1 / points[n]['ratio']:.1f}x Samba's time."
        lines.append(line)
        median = f" (median of {len(repeats)} repeats)" if len(repeats) > 1 else ""
        lines.append(f"By area, DittoFS vs Samba in MiB/s{median}:")
        for a, v in areas.items():
            shown = re.sub(r"\{([\w-]+)\}", lambda m: figure(m.group(1), points), FIGURES[a])
            lines.append(f"{a.capitalize()} {pct(v) if v is not None else 'incomplete'}: {shown}")
    if mode == "night" and not bands:
        lines.append("No noise band yet, so no flags: it comes from the calibration nights.")
    elif flags:
        old = len(flags) - len(new_flags)
        lines.append("Below their 7-day median: " + (", ".join(label(f) for f in new_flags) or "nothing new")
                     + (f" ({old} already flagged this week)" if old else ""))
    if failures:
        old = len(failures) - len(new_failures)
        lines.append("Failed: " + ("; ".join(new_failures) or "nothing new") + (f" ({old} failing this week already)" if old else ""))
    wear = ", ".join(f"{d} {w['written_gb']} GB written, {w['percent_used']}% used" for d, w in extra["drives"].items())
    host = os.environ.get("SCENARIO_HOST") or os.uname().nodename.split(".")[0]
    lines.append(" · ".join(x for x in (f"develop `{commit[:9]}`{commits}" if commit else "", host, wear, f"`{run}/`") if x))
    print("\n".join([title] + lines))


if __name__ == "__main__":
    main()
