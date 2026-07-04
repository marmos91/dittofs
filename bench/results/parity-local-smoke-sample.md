# rclone-parity scorecard — local-smoke

- started: 2026-07-03 19:27:21Z  finished: 19:27:41Z
- host: amaterasu  commit: 86eb1d70
- target: 127.0.0.1:19000 / bucket parity-smoke
- baseline: rclone v1.74.3
- dataset: large 2 x 32MiB, small 128 x 64KiB (seed 1467)

## upload-large

| conc | dittofs (Mbit/s) | rclone (Mbit/s) | dittofs/rclone |
|---:|---:|---:|---:|
| 1 | 186.2 | 343.7 | 54% |
| 4 | 322.0 | 610.7 | 53% |

## download-large

| conc | dittofs (Mbit/s) | rclone (Mbit/s) | dittofs/rclone |
|---:|---:|---:|---:|
| 1 | 3108.3 | 408.0 | 762% |
| 4 | 2203.2 | 576.4 | 382% |

## upload-small

| conc | dittofs (Mbit/s) | rclone (Mbit/s) | dittofs/rclone |
|---:|---:|---:|---:|
| 1 | 19.5 | 58.9 | 33% |
| 4 | 37.0 | 114.3 | 32% |

## download-small

| conc | dittofs (Mbit/s) | rclone (Mbit/s) | dittofs/rclone |
|---:|---:|---:|---:|
| 1 | 78.7 | 136.0 | 58% |
| 4 | 269.0 | 261.9 | 103% |

## meta

| conc | dittofs (obj/s) | rclone (obj/s) | dittofs/rclone |
|---:|---:|---:|---:|
| 1 | 103.4 | 442.3 | 23% |
| 4 | 95.0 | 679.6 | 14% |

