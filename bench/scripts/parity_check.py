#!/usr/bin/env python3
"""parity_check.py — regression tripwire for the rclone-parity scorecard (#1467).

Diffs a fresh `dfsbench parity` scorecard against a committed baseline and fails
only on GROSS, structural regressions — dittofs throughput on a (quadrant, conc)
cell dropping below baseline/FACTOR (default 2x). The bench rig is noisy (±40%
run-to-run is normal on shared/WAN infra), so a tight threshold would flake; this
guard is deliberately loose and catches only real >2x cliffs, not jitter.

It also prints the dittofs-vs-rclone ratio per cell for context (not gated — the
rclone lane is itself noisy and occasionally throws cache-driven outliers).

Usage:
    parity_check.py SCORECARD.json [--baseline bench/parity/baselines/wan.json]
                                   [--factor 2.0] [--quiet]

Exit code 0 = no gross regression; 1 = at least one cell regressed >FACTOR;
2 = usage/IO error.
"""
import argparse
import json
import sys


def cells_by_key(scorecard):
    """Map (quadrant, conc) -> {tool: throughput_mbps or ops_per_sec}."""
    out = {}
    for c in scorecard.get("cells", []):
        # meta lane reports ops/sec; data lanes report throughput. Use whichever
        # is populated so the comparison is apples-to-apples per quadrant.
        val = c.get("throughput_mbps") or c.get("ops_per_sec") or 0.0
        out.setdefault((c["quadrant"], c["conc"]), {})[c["tool"]] = val
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("scorecard", help="fresh dfsbench parity scorecard JSON")
    ap.add_argument("--baseline", default=None, help="baseline scorecard to gate against; omit for an informational table only")
    ap.add_argument("--factor", type=float, default=2.0, help="regression trips when dittofs < baseline/FACTOR (default 2.0)")
    ap.add_argument("--quiet", action="store_true", help="only print regressions")
    args = ap.parse_args()

    try:
        fresh = cells_by_key(json.load(open(args.scorecard)))
    except (OSError, json.JSONDecodeError, KeyError) as e:
        print(f"parity_check: {e}", file=sys.stderr)
        return 2
    # No baseline (e.g. localhost-smoke in CI, which has no WAN reference): print
    # the dittofs-vs-rclone table for visibility, but gate nothing.
    base = {}
    if args.baseline:
        try:
            base = cells_by_key(json.load(open(args.baseline)))
        except (OSError, json.JSONDecodeError, KeyError) as e:
            print(f"parity_check: {e}", file=sys.stderr)
            return 2

    regressions = []
    if not args.quiet:
        print(f"{'quadrant':<16}{'conc':>5}{'dittofs':>12}{'baseline':>12}{'Δ':>8}{'vs rclone':>11}")
    for key in sorted(fresh):
        quad, conc = key
        d = fresh[key].get("dittofs", 0.0)
        r = fresh[key].get("rclone", 0.0)
        b = base.get(key, {}).get("dittofs", 0.0)
        ratio = f"{d / r * 100:.0f}%" if r else "—"
        delta = f"{(d / b - 1) * 100:+.0f}%" if b else "—"
        flag = ""
        if b and d < b / args.factor:
            regressions.append((quad, conc, d, b))
            flag = "  <-- REGRESSION"
        if not args.quiet or flag:
            print(f"{quad:<16}{conc:>5}{d:>12.1f}{b:>12.1f}{delta:>8}{ratio:>11}{flag}")

    if not args.baseline:
        print("\nparity_check: informational only (no --baseline given)")
        return 0
    if regressions:
        print(f"\nparity_check: {len(regressions)} cell(s) regressed >{args.factor}x vs baseline", file=sys.stderr)
        return 1
    print(f"\nparity_check: OK — no cell regressed >{args.factor}x vs {args.baseline}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
