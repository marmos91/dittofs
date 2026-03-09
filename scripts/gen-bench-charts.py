#!/usr/bin/env python3
"""Generate benchmark charts for DittoFS docs/BENCHMARKS.md"""

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import matplotlib.ticker as ticker
import numpy as np
import os

# Output directory (relative to repo root)
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(SCRIPT_DIR, "..", "docs", "assets")
os.makedirs(OUT, exist_ok=True)

# ── Round 24 Data ──────────────────────────────────────────────────────────────
# Systems in display order
systems = ["DittoFS S3\n(NFSv4.1)", "DittoFS S3\n(NFSv3)", "kernel NFS\n(local disk)", "JuiceFS S3"]
systems_short = ["DittoFS S3", "DittoFS S3\n(NFSv3)", "kernel NFS", "JuiceFS S3"]

# Colors: DittoFS = brand blue, kernel = gray, JuiceFS = orange
colors = ["#2563EB", "#60A5FA", "#6B7280", "#F97316"]
edge_colors = ["#1D4ED8", "#3B82F6", "#4B5563", "#EA580C"]

# Data from round-24 JSON files
seq_write = [50.7, 50.8, 49.2, 31.2]  # MB/s
seq_read  = [63.9, 63.9, 63.9, 50.5]  # MB/s
rand_write = [635, 634, 1234, 60]      # IOPS
rand_read  = [1420, 1383, 2241, 1447]  # IOPS
metadata   = [609, 146, 290, 7]        # ops/s
small_files = [1792, 154, 492, 44]     # ops/s

plt.rcParams.update({
    'font.family': 'sans-serif',
    'font.size': 11,
    'axes.titlesize': 14,
    'axes.titleweight': 'bold',
    'figure.facecolor': 'white',
    'axes.facecolor': '#FAFAFA',
    'axes.grid': True,
    'grid.alpha': 0.3,
    'grid.linestyle': '--',
})


def add_value_labels(ax, bars, fmt="{:.0f}", suffix="", fontsize=9):
    for bar in bars:
        val = bar.get_width() if bar.get_width() > 0 else bar.get_height()
        if bar.get_width() > 0:  # horizontal
            ax.text(bar.get_width() + ax.get_xlim()[1] * 0.01, bar.get_y() + bar.get_height()/2,
                    fmt.format(val) + suffix, va='center', fontsize=fontsize, fontweight='bold')
        else:  # vertical
            ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + ax.get_ylim()[1] * 0.01,
                    fmt.format(val) + suffix, ha='center', fontsize=fontsize, fontweight='bold')


def multiplier_annotation(ax, idx_base, idx_target, values, y_offset=0):
    """Add a multiplier annotation between two bars."""
    ratio = values[idx_target] / values[idx_base] if values[idx_base] > 0 else float('inf')
    if ratio > 1:
        text = f"{ratio:.0f}x" if ratio >= 10 else f"{ratio:.1f}x"
        y = max(values[idx_target], values[idx_base])
        ax.annotate(text, xy=(idx_target, values[idx_target]),
                    fontsize=10, fontweight='bold', color='#DC2626',
                    ha='center', va='bottom',
                    xytext=(0, 5 + y_offset), textcoords='offset points')


# ── 1. Hero Chart: DittoFS vs JuiceFS Multipliers ─────────────────────────────
fig, ax = plt.subplots(figsize=(10, 6))

workloads = ['seq-write', 'seq-read', 'rand-write', 'rand-read', 'metadata', 'small-files']
dittofs_vals = [50.7, 63.9, 635, 1420, 609, 1792]
juicefs_vals = [31.2, 50.5, 60, 1447, 7, 44]
ratios = [d/j if j > 0 else 0 for d, j in zip(dittofs_vals, juicefs_vals)]

y_pos = np.arange(len(workloads))
bars = ax.barh(y_pos, ratios, color=['#2563EB' if r > 1 else '#EF4444' for r in ratios],
               edgecolor=['#1D4ED8' if r > 1 else '#DC2626' for r in ratios], linewidth=1.5, height=0.6)

ax.axvline(x=1, color='#374151', linewidth=2, linestyle='-', alpha=0.7, label='Parity (1x)')
ax.set_yticks(y_pos)
ax.set_yticklabels(['Sequential\nWrite', 'Sequential\nRead', 'Random\nWrite', 'Random\nRead', 'Metadata\nOps', 'Small\nFiles'],
                    fontsize=11)
ax.set_xlabel('DittoFS / JuiceFS (higher = DittoFS wins)', fontsize=12)
ax.set_title('DittoFS S3 vs JuiceFS S3 — Performance Ratio', fontsize=15, fontweight='bold', pad=15)

for i, (bar, ratio) in enumerate(zip(bars, ratios)):
    label = f"{ratio:.1f}x" if ratio < 10 else f"{ratio:.0f}x"
    ax.text(bar.get_width() + 0.5, bar.get_y() + bar.get_height()/2,
            label, va='center', fontsize=12, fontweight='bold',
            color='#1D4ED8' if ratio > 1 else '#DC2626')

ax.set_xlim(0, max(ratios) * 1.15)
ax.legend(fontsize=10)
fig.tight_layout()
fig.savefig(f"{OUT}/bench-vs-juicefs.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-vs-juicefs.png")


# ── 2. Sequential Throughput ───────────────────────────────────────────────────
fig, axes = plt.subplots(1, 2, figsize=(12, 5))

for ax_idx, (data, title, unit) in enumerate([
    (seq_write, 'Sequential Write', 'MB/s'),
    (seq_read, 'Sequential Read', 'MB/s'),
]):
    ax = axes[ax_idx]
    bars = ax.bar(range(len(systems)), data, color=colors, edgecolor=edge_colors,
                  linewidth=1.5, width=0.7)
    ax.set_xticks(range(len(systems)))
    ax.set_xticklabels(systems_short, fontsize=9)
    ax.set_ylabel(unit, fontsize=11)
    ax.set_title(title, fontsize=13, fontweight='bold')

    for bar, val in zip(bars, data):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 0.5,
                f"{val:.1f}", ha='center', fontsize=10, fontweight='bold')

    ax.set_ylim(0, max(data) * 1.15)

fig.suptitle('Sequential Throughput (4 threads, 1 GiB file)', fontsize=14, fontweight='bold', y=1.02)
fig.tight_layout()
fig.savefig(f"{OUT}/bench-throughput.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-throughput.png")


# ── 3. Random I/O ─────────────────────────────────────────────────────────────
fig, axes = plt.subplots(1, 2, figsize=(12, 5))

for ax_idx, (data, title) in enumerate([
    (rand_write, 'Random Write (4K)'),
    (rand_read, 'Random Read (4K)'),
]):
    ax = axes[ax_idx]
    bars = ax.bar(range(len(systems)), data, color=colors, edgecolor=edge_colors,
                  linewidth=1.5, width=0.7)
    ax.set_xticks(range(len(systems)))
    ax.set_xticklabels(systems_short, fontsize=9)
    ax.set_ylabel('IOPS', fontsize=11)
    ax.set_title(title, fontsize=13, fontweight='bold')

    for bar, val in zip(bars, data):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + max(data) * 0.01,
                f"{val:,.0f}", ha='center', fontsize=10, fontweight='bold')

    ax.set_ylim(0, max(data) * 1.15)

fig.suptitle('Random I/O Performance (4 threads, 60s)', fontsize=14, fontweight='bold', y=1.02)
fig.tight_layout()
fig.savefig(f"{OUT}/bench-iops.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-iops.png")


# ── 4. Metadata & Small Files ─────────────────────────────────────────────────
fig, axes = plt.subplots(1, 2, figsize=(12, 5))

for ax_idx, (data, title) in enumerate([
    (metadata, 'Metadata Ops (create+stat+delete)'),
    (small_files, 'Small Files (create+read+stat+delete)'),
]):
    ax = axes[ax_idx]
    bars = ax.bar(range(len(systems)), data, color=colors, edgecolor=edge_colors,
                  linewidth=1.5, width=0.7)
    ax.set_xticks(range(len(systems)))
    ax.set_xticklabels(systems_short, fontsize=9)
    ax.set_ylabel('ops/s', fontsize=11)
    ax.set_title(title, fontsize=13, fontweight='bold')

    for bar, val in zip(bars, data):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + max(data) * 0.01,
                f"{val:,.0f}", ha='center', fontsize=10, fontweight='bold')

    ax.set_ylim(0, max(data) * 1.15)

fig.suptitle('Metadata & Small File Performance (4 threads)', fontsize=14, fontweight='bold', y=1.02)
fig.tight_layout()
fig.savefig(f"{OUT}/bench-metadata.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-metadata.png")


# ── 5. Summary Heatmap (DittoFS vs all) ───────────────────────────────────────
fig, ax = plt.subplots(figsize=(10, 4))

# Ratio of DittoFS S3 NFSv4.1 vs each competitor
competitors = ['kernel NFS\n(local disk)', 'JuiceFS S3']
workload_labels = ['seq-write', 'seq-read', 'rand-write', 'rand-read', 'metadata', 'small-files']

kernel_vals = [49.2, 63.9, 1234, 2241, 290, 492]
juicefs_vals_raw = [31.2, 50.5, 60, 1447, 7, 44]
dittofs_nfs41 = [50.7, 63.9, 635, 1420, 609, 1792]

ratios_kernel = [d/k if k > 0 else 0 for d, k in zip(dittofs_nfs41, kernel_vals)]
ratios_juice = [d/j if j > 0 else 0 for d, j in zip(dittofs_nfs41, juicefs_vals_raw)]

data_matrix = np.array([ratios_kernel, ratios_juice])

# Custom colormap: red < 1, white = 1, green > 1
from matplotlib.colors import LinearSegmentedColormap
cmap_colors = [(0.8, 0.2, 0.2), (1, 1, 1), (0.1, 0.6, 0.3)]
cmap = LinearSegmentedColormap.from_list('rg', cmap_colors, N=256)

im = ax.imshow(data_matrix, cmap=cmap, aspect='auto', vmin=0.3, vmax=4.0)

ax.set_xticks(range(len(workload_labels)))
ax.set_xticklabels(workload_labels, fontsize=11)
ax.set_yticks(range(len(competitors)))
ax.set_yticklabels(competitors, fontsize=11)

for i in range(len(competitors)):
    for j in range(len(workload_labels)):
        val = data_matrix[i, j]
        text = f"{val:.1f}x" if val < 10 else f"{val:.0f}x"
        color = 'white' if val > 3 or val < 0.5 else 'black'
        ax.text(j, i, text, ha='center', va='center', fontsize=12, fontweight='bold', color=color)

ax.set_title('DittoFS S3 (NFSv4.1) Performance Ratio vs Competitors', fontsize=14, fontweight='bold', pad=10)
cbar = plt.colorbar(im, ax=ax, label='Ratio (>1 = DittoFS wins)', shrink=0.8)

fig.tight_layout()
fig.savefig(f"{OUT}/bench-summary.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-summary.png")


# ── 6. Radar/Spider Chart ─────────────────────────────────────────────────────
fig, ax = plt.subplots(figsize=(8, 8), subplot_kw=dict(polar=True))

categories = ['seq-write', 'seq-read', 'rand-write', 'rand-read', 'metadata', 'small-files']
N = len(categories)
angles = [n / float(N) * 2 * np.pi for n in range(N)]
angles += angles[:1]

# Normalize: each metric → % of best across all systems
all_values = {
    'DittoFS S3': dittofs_nfs41,
    'kernel NFS': kernel_vals,
    'JuiceFS S3': juicefs_vals_raw,
}
maxes = [max(v[i] for v in all_values.values()) for i in range(N)]

for name, vals, color, ls in [
    ('DittoFS S3', dittofs_nfs41, '#2563EB', '-'),
    ('kernel NFS', kernel_vals, '#6B7280', '--'),
    ('JuiceFS S3', juicefs_vals_raw, '#F97316', ':'),
]:
    normalized = [v / m * 100 if m > 0 else 0 for v, m in zip(vals, maxes)]
    normalized += normalized[:1]
    ax.plot(angles, normalized, linewidth=2.5, linestyle=ls, label=name, color=color)
    ax.fill(angles, normalized, alpha=0.1, color=color)

ax.set_xticks(angles[:-1])
ax.set_xticklabels(categories, fontsize=11, fontweight='bold')
ax.set_ylim(0, 110)
ax.set_yticks([25, 50, 75, 100])
ax.set_yticklabels(['25%', '50%', '75%', '100%'], fontsize=8, alpha=0.7)
ax.set_title('Performance Profile (% of best)', fontsize=14, fontweight='bold', pad=20)
ax.legend(loc='lower right', bbox_to_anchor=(1.2, -0.05), fontsize=11)

fig.tight_layout()
fig.savefig(f"{OUT}/bench-radar.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-radar.png")


# ── 7. Latency Comparison ─────────────────────────────────────────────────────
fig, axes = plt.subplots(2, 3, figsize=(15, 9))

# Latency data from JSON files (P50, P95, P99 in us → ms)
latency_data = {
    'seq-write': {
        'DittoFS S3': [0.676, 1.019, 1.508],
        'kernel NFS': [0.701, 0.978, 1.515],
        'JuiceFS S3': [0.640, 0.884, 1.044],
    },
    'seq-read': {
        'DittoFS S3': [16.2, 20.6, 211.6],
        'kernel NFS': [15.9, 20.4, 216.3],
        'JuiceFS S3': [3.3, 41.6, 265.9],
    },
    'rand-write': {
        'DittoFS S3': [1.354, 2.012, 2.814],
        'kernel NFS': [0.769, 0.998, 1.218],
        'JuiceFS S3': [1.505, 3.355, 11.655],
    },
    'rand-read': {
        'DittoFS S3': [0.710, 0.876, 1.007],
        'kernel NFS': [0.404, 0.917, 1.085],
        'JuiceFS S3': [0.530, 0.722, 2.734],
    },
    'metadata': {
        'DittoFS S3': [1.004, 4.013, 4.455],
        'kernel NFS': [2.848, 7.796, 11.533],
        'JuiceFS S3': [8.549, 584.9, 1170.1],
    },
    'small-files': {
        'DittoFS S3': [2.183, 4.179, 4.909],
        'kernel NFS': [2.400, 19.154, 27.271],
        'JuiceFS S3': [8.143, 405.9, 948.9],
    },
}

percentiles = ['P50', 'P95', 'P99']
system_colors_lat = {'DittoFS S3': '#2563EB', 'kernel NFS': '#6B7280', 'JuiceFS S3': '#F97316'}

for idx, (workload, sys_data) in enumerate(latency_data.items()):
    ax = axes[idx // 3][idx % 3]
    x = np.arange(len(percentiles))
    width = 0.25

    for i, (sys_name, lats) in enumerate(sys_data.items()):
        bars = ax.bar(x + i * width, lats, width, label=sys_name,
                      color=system_colors_lat[sys_name], edgecolor='white', linewidth=0.5)

    ax.set_xticks(x + width)
    ax.set_xticklabels(percentiles, fontsize=10)
    ax.set_ylabel('ms', fontsize=10)
    ax.set_title(workload, fontsize=12, fontweight='bold')
    ax.set_yscale('log')
    if idx == 0:
        ax.legend(fontsize=8)

fig.suptitle('Latency Distribution by Workload (log scale)', fontsize=15, fontweight='bold', y=1.02)
fig.tight_layout()
fig.savefig(f"{OUT}/bench-latency.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-latency.png")


# ── 8. Optimization History ───────────────────────────────────────────────────
fig, ax = plt.subplots(figsize=(12, 6))

# Before (round 15 old data) vs After (round 24)
metrics = ['seq-write\n(MB/s)', 'seq-read\n(MB/s)', 'rand-write\n(IOPS)', 'rand-read\n(IOPS)', 'metadata\n(ops/s)', 'small-files\n(ops/s)']
before = [50.9, 64.0, 308, 594, 486, 0]   # Round 15 S3 data (no small-files then)
after  = [50.7, 63.9, 635, 1420, 609, 1792]  # Round 24 NFSv4.1

x = np.arange(len(metrics))
width = 0.35

bars1 = ax.bar(x - width/2, before, width, label='Round 15 (baseline)', color='#CBD5E1', edgecolor='#94A3B8', linewidth=1.5)
bars2 = ax.bar(x + width/2, after, width, label='Round 24 (optimized)', color='#2563EB', edgecolor='#1D4ED8', linewidth=1.5)

# Add improvement percentages
for i in range(len(metrics)):
    if before[i] > 0:
        pct = (after[i] - before[i]) / before[i] * 100
        color = '#16A34A' if pct > 0 else '#DC2626'
        sign = '+' if pct > 0 else ''
        ax.text(i, max(after[i], before[i]) + max(max(after), max(before)) * 0.02,
                f"{sign}{pct:.0f}%", ha='center', fontsize=11, fontweight='bold', color=color)
    else:
        ax.text(i, after[i] + max(max(after), max(before)) * 0.02,
                "NEW", ha='center', fontsize=11, fontweight='bold', color='#16A34A')

ax.set_xticks(x)
ax.set_xticklabels(metrics, fontsize=10)
ax.set_title('DittoFS S3 Performance: Before vs After Optimization', fontsize=14, fontweight='bold', pad=15)
ax.legend(fontsize=11)
ax.set_ylim(0, max(max(after), max(before)) * 1.15)

fig.tight_layout()
fig.savefig(f"{OUT}/bench-improvement.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-improvement.png")


# ── 9. NFSv3 vs NFSv4.1 comparison ────────────────────────────────────────────
fig, ax = plt.subplots(figsize=(10, 5))

workloads_nfs = ['seq-write', 'seq-read', 'rand-write', 'rand-read', 'metadata', 'small-files']
nfsv3 = [50.8, 63.9, 634, 1383, 146, 154]
nfsv41 = [50.7, 63.9, 635, 1420, 609, 1792]

x = np.arange(len(workloads_nfs))
width = 0.35

bars1 = ax.bar(x - width/2, nfsv3, width, label='NFSv3', color='#94A3B8', edgecolor='#64748B', linewidth=1.5)
bars2 = ax.bar(x + width/2, nfsv41, width, label='NFSv4.1', color='#2563EB', edgecolor='#1D4ED8', linewidth=1.5)

for i in range(len(workloads_nfs)):
    if nfsv3[i] > 0:
        ratio = nfsv41[i] / nfsv3[i]
        if ratio > 1.1:
            ax.text(i, max(nfsv41[i], nfsv3[i]) + max(max(nfsv41), max(nfsv3)) * 0.02,
                    f"{ratio:.1f}x", ha='center', fontsize=11, fontweight='bold', color='#DC2626')

ax.set_xticks(x)
ax.set_xticklabels(workloads_nfs, fontsize=10)
ax.set_ylabel('Performance (mixed units)', fontsize=11)
ax.set_title('DittoFS S3: NFSv3 vs NFSv4.1', fontsize=14, fontweight='bold', pad=15)
ax.legend(fontsize=11)
ax.set_ylim(0, max(max(nfsv41), max(nfsv3)) * 1.15)

fig.tight_layout()
fig.savefig(f"{OUT}/bench-nfs-versions.png", dpi=150, bbox_inches='tight')
plt.close()
print("  bench-nfs-versions.png")

print("\nAll charts generated successfully!")
