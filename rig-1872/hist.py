#!/usr/bin/env python3
"""Histogram of S3 object sizes from s3snap.sh output.

usage: hist.py SNAP [--since ISO8601] [--until ISO8601]
Sizes are exact PUT sizes; buckets are powers of two plus an exact-value
top-10 so the pre-fix quantisation (4168/6216/8264 B) stays visible.
"""
import sys, collections, statistics

path = sys.argv[1]
since = until = None
args = sys.argv[2:]
for i, a in enumerate(args):
    if a == "--since": since = args[i+1]
    if a == "--until": until = args[i+1]

sizes = []
for line in open(path):
    f = line.rstrip("\n").split("\t")
    if len(f) < 3: continue
    sz, lm = int(f[0]), f[1]
    if since and lm < since: continue
    if until and lm > until: continue
    sizes.append(sz)

if not sizes:
    print("no objects in window"); sys.exit(0)

n = len(sizes); tot = sum(sizes)
print(f"objects: {n}   bytes: {tot} ({tot/2**20:.1f} MiB)")
print(f"mean: {tot/n:,.0f} B   median: {statistics.median(sizes):,.0f} B")
print(f"min: {min(sizes):,} B   max: {max(sizes):,} B")
print("\npower-of-two buckets:")
buckets = collections.Counter()
for s in sizes:
    b = 1
    while b < s: b <<= 1
    buckets[b] += 1
for b in sorted(buckets):
    lbl = f"{b}" if b < 1024 else (f"{b>>10}K" if b < 1<<20 else f"{b>>20}M")
    print(f"  <= {lbl:>6}  {buckets[b]:8d}  {100*buckets[b]/n:5.1f}%")
print("\ntop exact sizes:")
for sz, c in collections.Counter(sizes).most_common(10):
    print(f"  {sz:>10,} B  x{c}")
