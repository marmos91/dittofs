#!/usr/bin/env python3
"""Generate benchmark comparison charts from JSON result files.

Usage:
    python3 scripts/gen-bench-charts.py

Outputs PNGs to docs/assets/bench-*.png
"""

import json
import os
import numpy as np
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import FancyBboxPatch

RESULTS_DIR = os.path.join(os.path.dirname(__file__), "..", "results")
OUTPUT_DIR = os.path.join(os.path.dirname(__file__), "..", "docs", "assets")

# Color palette
COLORS = {
    "dittofs-fs": "#2563eb",      # blue
    "dittofs-s3": "#7c3aed",      # violet
    "dittofs-old": "#94a3b8",     # slate (before optimization)
    "kernel-nfs": "#16a34a",      # green
    "ganesha": "#ea580c",         # orange
    "rclone": "#0891b2",          # cyan
    "samba": "#d946ef",           # fuchsia
    "juicefs": "#eab308",         # yellow
}

LABELS = {
    "dittofs-fs": "DittoFS (fs)",
    "dittofs-s3": "DittoFS (S3)",
    "dittofs-old": "DittoFS (pre-opt)",
    "kernel-nfs": "kernel NFS",
    "ganesha": "NFS-Ganesha",
    "rclone": "Rclone",
    "samba": "Samba",
    "juicefs": "JuiceFS",
}


def load_results():
    """Load all benchmark JSON files into a dict of {system: data}."""
    data = {}

    # Round 15 (current DittoFS)
    for name in ["dittofs-fs", "dittofs-s3"]:
        path = os.path.join(RESULTS_DIR, "dittofs-round15", f"{name}.json")
        if os.path.exists(path):
            with open(path) as f:
                data[name] = json.load(f)

    # Pre-optimization DittoFS
    path = os.path.join(RESULTS_DIR, "competitors", "dittofs.json")
    if os.path.exists(path):
        with open(path) as f:
            data["dittofs-old"] = json.load(f)

    # Competitors
    for name in ["kernel-nfs", "ganesha", "rclone", "samba", "juicefs"]:
        path = os.path.join(RESULTS_DIR, "competitors", f"{name}.json")
        if os.path.exists(path):
            with open(path) as f:
                data[name] = json.load(f)

    return data


def get_metric(data, workload, metric):
    """Extract a metric from a system's data, returning 0 if missing."""
    w = data.get("workloads", {}).get(workload, {})
    return w.get(metric, 0)


def setup_style():
    """Configure matplotlib style."""
    plt.rcParams.update({
        "figure.facecolor": "#ffffff",
        "axes.facecolor": "#fafafa",
        "axes.grid": True,
        "axes.axisbelow": True,
        "grid.alpha": 0.3,
        "grid.linestyle": "--",
        "font.family": "sans-serif",
        "font.size": 11,
        "axes.titlesize": 14,
        "axes.titleweight": "bold",
        "axes.labelsize": 12,
    })


def chart_throughput(data):
    """Bar chart: sequential throughput (MB/s)."""
    systems = ["dittofs-fs", "dittofs-s3", "kernel-nfs", "ganesha", "rclone", "samba", "juicefs"]
    systems = [s for s in systems if s in data]

    write_vals = [get_metric(data[s], "seq-write", "throughput_mbps") for s in systems]
    read_vals = [get_metric(data[s], "seq-read", "throughput_mbps") for s in systems]

    x = np.arange(len(systems))
    width = 0.35

    fig, ax = plt.subplots(figsize=(12, 6))
    bars1 = ax.bar(x - width/2, write_vals, width, label="Sequential Write",
                   color=[COLORS.get(s, "#666") for s in systems], alpha=0.85, edgecolor="white", linewidth=0.5)
    bars2 = ax.bar(x + width/2, read_vals, width, label="Sequential Read",
                   color=[COLORS.get(s, "#666") for s in systems], alpha=0.5, edgecolor="white", linewidth=0.5,
                   hatch="//")

    ax.set_ylabel("Throughput (MB/s)")
    ax.set_title("Sequential Throughput")
    ax.set_xticks(x)
    ax.set_xticklabels([LABELS.get(s, s) for s in systems], rotation=20, ha="right")
    ax.legend(loc="upper right")
    ax.set_ylim(0, max(max(write_vals), max(read_vals)) * 1.15)

    # Value labels
    for bar in bars1:
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 0.5,
                f"{bar.get_height():.1f}", ha="center", va="bottom", fontsize=8)
    for bar in bars2:
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 0.5,
                f"{bar.get_height():.1f}", ha="center", va="bottom", fontsize=8)

    fig.tight_layout()
    fig.savefig(os.path.join(OUTPUT_DIR, "bench-throughput.png"), dpi=150)
    plt.close(fig)
    print("  -> bench-throughput.png")


def chart_iops(data):
    """Bar chart: random I/O (IOPS)."""
    systems = ["dittofs-fs", "dittofs-s3", "kernel-nfs", "ganesha", "rclone", "samba", "juicefs"]
    systems = [s for s in systems if s in data]

    write_vals = [get_metric(data[s], "rand-write", "iops") for s in systems]
    read_vals = [get_metric(data[s], "rand-read", "iops") for s in systems]

    x = np.arange(len(systems))
    width = 0.35

    fig, ax = plt.subplots(figsize=(12, 6))
    bars1 = ax.bar(x - width/2, write_vals, width, label="Random Write",
                   color=[COLORS.get(s, "#666") for s in systems], alpha=0.85, edgecolor="white", linewidth=0.5)
    bars2 = ax.bar(x + width/2, read_vals, width, label="Random Read",
                   color=[COLORS.get(s, "#666") for s in systems], alpha=0.5, edgecolor="white", linewidth=0.5,
                   hatch="//")

    ax.set_ylabel("IOPS")
    ax.set_title("Random I/O Performance")
    ax.set_xticks(x)
    ax.set_xticklabels([LABELS.get(s, s) for s in systems], rotation=20, ha="right")
    ax.legend(loc="upper right")
    ax.set_ylim(0, max(max(write_vals), max(read_vals)) * 1.15)

    for bar in bars1:
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 10,
                f"{bar.get_height():.0f}", ha="center", va="bottom", fontsize=8)
    for bar in bars2:
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 10,
                f"{bar.get_height():.0f}", ha="center", va="bottom", fontsize=8)

    fig.tight_layout()
    fig.savefig(os.path.join(OUTPUT_DIR, "bench-iops.png"), dpi=150)
    plt.close(fig)
    print("  -> bench-iops.png")


def chart_metadata(data):
    """Horizontal bar chart: metadata ops/s."""
    systems = ["dittofs-fs", "dittofs-s3", "kernel-nfs", "ganesha", "rclone", "samba", "juicefs"]
    systems = [s for s in systems if s in data]

    vals = [get_metric(data[s], "metadata", "ops_per_sec") for s in systems]

    fig, ax = plt.subplots(figsize=(10, 5))
    y = np.arange(len(systems))
    colors = [COLORS.get(s, "#666") for s in systems]

    bars = ax.barh(y, vals, color=colors, alpha=0.85, edgecolor="white", linewidth=0.5)
    ax.set_yticks(y)
    ax.set_yticklabels([LABELS.get(s, s) for s in systems])
    ax.set_xlabel("Operations/sec (create + stat + delete)")
    ax.set_title("Metadata Performance")
    ax.invert_yaxis()

    for bar, val in zip(bars, vals):
        ax.text(bar.get_width() + 5, bar.get_y() + bar.get_height()/2,
                f"{val:.0f} ops/s", ha="left", va="center", fontsize=9)

    ax.set_xlim(0, max(vals) * 1.25)
    fig.tight_layout()
    fig.savefig(os.path.join(OUTPUT_DIR, "bench-metadata.png"), dpi=150)
    plt.close(fig)
    print("  -> bench-metadata.png")


def chart_latency(data):
    """Grouped bar chart: P50 / P99 latency for seq-write."""
    systems = ["dittofs-fs", "dittofs-s3", "kernel-nfs", "ganesha", "rclone", "samba", "juicefs"]
    systems = [s for s in systems if s in data]

    workloads = ["seq-write", "rand-write", "rand-read", "metadata"]
    wl_labels = ["Seq Write", "Rand Write", "Rand Read", "Metadata"]

    fig, axes = plt.subplots(1, 4, figsize=(18, 5), sharey=False)

    for idx, (wl, wl_label) in enumerate(zip(workloads, wl_labels)):
        ax = axes[idx]
        p50 = [get_metric(data[s], wl, "latency_p50_us") / 1000 for s in systems]  # ms
        p99 = [get_metric(data[s], wl, "latency_p99_us") / 1000 for s in systems]  # ms

        x = np.arange(len(systems))
        width = 0.35
        bars1 = ax.bar(x - width/2, p50, width, label="P50", alpha=0.85,
                       color=[COLORS.get(s, "#666") for s in systems], edgecolor="white", linewidth=0.5)
        bars2 = ax.bar(x + width/2, p99, width, label="P99", alpha=0.4,
                       color=[COLORS.get(s, "#666") for s in systems], edgecolor="white", linewidth=0.5,
                       hatch="xx")

        ax.set_title(wl_label)
        ax.set_xticks(x)
        ax.set_xticklabels([LABELS.get(s, s) for s in systems], rotation=45, ha="right", fontsize=7)
        ax.set_ylabel("Latency (ms)" if idx == 0 else "")
        if idx == 0:
            ax.legend(fontsize=8)

    fig.suptitle("Latency Distribution (P50 vs P99)", fontweight="bold", fontsize=14)
    fig.tight_layout()
    fig.savefig(os.path.join(OUTPUT_DIR, "bench-latency.png"), dpi=150)
    plt.close(fig)
    print("  -> bench-latency.png")


def chart_improvement(data):
    """Before/after chart showing DittoFS optimization improvements."""
    if "dittofs-old" not in data or "dittofs-fs" not in data:
        print("  -> skipping bench-improvement.png (missing data)")
        return

    old = data["dittofs-old"]
    new = data["dittofs-fs"]

    metrics = [
        ("seq-write", "throughput_mbps", "Seq Write\n(MB/s)", "MB/s"),
        ("seq-read", "throughput_mbps", "Seq Read\n(MB/s)", "MB/s"),
        ("rand-write", "iops", "Rand Write\n(IOPS)", "IOPS"),
        ("rand-read", "iops", "Rand Read\n(IOPS)", "IOPS"),
        ("metadata", "ops_per_sec", "Metadata\n(ops/s)", "ops/s"),
    ]

    old_vals = [get_metric(old, m[0], m[1]) for m in metrics]
    new_vals = [get_metric(new, m[0], m[1]) for m in metrics]
    labels = [m[2] for m in metrics]
    units = [m[3] for m in metrics]

    fig, axes = plt.subplots(1, 5, figsize=(16, 5))

    for idx, (ax, label, unit, ov, nv) in enumerate(zip(axes, labels, units, old_vals, new_vals)):
        bars = ax.bar(["Before", "After"], [ov, nv],
                      color=[COLORS["dittofs-old"], COLORS["dittofs-fs"]],
                      alpha=0.85, edgecolor="white", linewidth=0.5)
        ax.set_title(label, fontsize=10)
        ax.set_ylim(0, max(ov, nv) * 1.3)

        for bar, val in zip(bars, [ov, nv]):
            ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + max(ov, nv) * 0.02,
                    f"{val:.1f}" if "MB" in unit else f"{val:.0f}",
                    ha="center", va="bottom", fontsize=9, fontweight="bold")

        # Improvement percentage
        if ov > 0:
            pct = ((nv - ov) / ov) * 100
            color = "#16a34a" if pct > 0 else "#dc2626"
            sign = "+" if pct > 0 else ""
            ax.text(0.5, 0.92, f"{sign}{pct:.0f}%", transform=ax.transAxes,
                    ha="center", fontsize=12, fontweight="bold", color=color)

    fig.suptitle("DittoFS Optimization Impact (Round 12 vs Round 15)", fontweight="bold", fontsize=14)
    fig.tight_layout()
    fig.savefig(os.path.join(OUTPUT_DIR, "bench-improvement.png"), dpi=150)
    plt.close(fig)
    print("  -> bench-improvement.png")


def chart_radar(data):
    """Radar/spider chart comparing DittoFS against competitors."""
    # Normalize each metric to 0-1 range across all systems
    systems = ["dittofs-fs", "kernel-nfs", "ganesha", "rclone", "juicefs"]
    systems = [s for s in systems if s in data]

    metrics = [
        ("seq-write", "throughput_mbps", "Seq Write"),
        ("seq-read", "throughput_mbps", "Seq Read"),
        ("rand-write", "iops", "Rand Write"),
        ("rand-read", "iops", "Rand Read"),
        ("metadata", "ops_per_sec", "Metadata"),
    ]

    # Get raw values
    raw = {}
    for s in systems:
        raw[s] = [get_metric(data[s], m[0], m[1]) for m in metrics]

    # Normalize to 0-1 (max across all systems = 1)
    maxvals = [max(raw[s][i] for s in systems) for i in range(len(metrics))]
    norm = {}
    for s in systems:
        norm[s] = [raw[s][i] / maxvals[i] if maxvals[i] > 0 else 0 for i in range(len(metrics))]

    # Radar chart
    angles = np.linspace(0, 2 * np.pi, len(metrics), endpoint=False).tolist()
    angles += angles[:1]  # close the polygon

    fig, ax = plt.subplots(figsize=(8, 8), subplot_kw=dict(polar=True))

    for s in systems:
        values = norm[s] + norm[s][:1]
        ax.plot(angles, values, "o-", linewidth=2, label=LABELS.get(s, s),
                color=COLORS.get(s, "#666"), alpha=0.8)
        ax.fill(angles, values, alpha=0.08, color=COLORS.get(s, "#666"))

    ax.set_xticks(angles[:-1])
    ax.set_xticklabels([m[2] for m in metrics], fontsize=11)
    ax.set_ylim(0, 1.1)
    ax.set_yticks([0.25, 0.5, 0.75, 1.0])
    ax.set_yticklabels(["25%", "50%", "75%", "100%"], fontsize=8, alpha=0.6)
    ax.set_title("Performance Profile (normalized)", fontweight="bold", fontsize=14, pad=20)
    ax.legend(loc="upper right", bbox_to_anchor=(1.3, 1.1), fontsize=10)

    fig.tight_layout()
    fig.savefig(os.path.join(OUTPUT_DIR, "bench-radar.png"), dpi=150)
    plt.close(fig)
    print("  -> bench-radar.png")


def chart_summary_table(data):
    """Generate a visual summary table as an image."""
    systems = ["dittofs-fs", "dittofs-s3", "kernel-nfs", "ganesha", "rclone", "samba", "juicefs"]
    systems = [s for s in systems if s in data]

    rows = []
    for s in systems:
        sw = get_metric(data[s], "seq-write", "throughput_mbps")
        sr = get_metric(data[s], "seq-read", "throughput_mbps")
        rw = get_metric(data[s], "rand-write", "iops")
        rr = get_metric(data[s], "rand-read", "iops")
        md = get_metric(data[s], "metadata", "ops_per_sec")
        rows.append([
            LABELS.get(s, s),
            f"{sw:.1f} MB/s",
            f"{sr:.1f} MB/s",
            f"{rw:.0f} IOPS",
            f"{rr:.0f} IOPS",
            f"{md:.0f} ops/s",
        ])

    col_labels = ["System", "Seq Write", "Seq Read", "Rand Write", "Rand Read", "Metadata"]

    fig, ax = plt.subplots(figsize=(14, 4))
    ax.axis("off")

    table = ax.table(
        cellText=rows,
        colLabels=col_labels,
        cellLoc="center",
        loc="center",
    )

    table.auto_set_font_size(False)
    table.set_fontsize(11)
    table.scale(1, 1.6)

    # Style header
    for j in range(len(col_labels)):
        cell = table[0, j]
        cell.set_facecolor("#1e293b")
        cell.set_text_props(color="white", fontweight="bold")

    # Color DittoFS rows
    for i, s in enumerate(systems):
        for j in range(len(col_labels)):
            cell = table[i + 1, j]
            if s.startswith("dittofs"):
                cell.set_facecolor("#eff6ff")
            else:
                cell.set_facecolor("#ffffff" if i % 2 == 0 else "#f8fafc")

    fig.suptitle("Benchmark Results Summary", fontweight="bold", fontsize=14, y=0.98)
    fig.tight_layout()
    fig.savefig(os.path.join(OUTPUT_DIR, "bench-summary.png"), dpi=150, bbox_inches="tight")
    plt.close(fig)
    print("  -> bench-summary.png")


def main():
    os.makedirs(OUTPUT_DIR, exist_ok=True)
    setup_style()

    print("Loading benchmark data...")
    data = load_results()
    print(f"  Loaded {len(data)} systems: {', '.join(data.keys())}")

    print("\nGenerating charts...")
    chart_throughput(data)
    chart_iops(data)
    chart_metadata(data)
    chart_latency(data)
    chart_improvement(data)
    chart_radar(data)
    chart_summary_table(data)

    print(f"\nDone! Charts saved to {OUTPUT_DIR}/")


if __name__ == "__main__":
    main()
