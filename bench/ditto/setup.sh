#!/usr/bin/env bash
# Sets up a Linux host for night.sh, as root, so a replaced host is ready again in minutes. It can
# run again at any time; it changes only what is missing or different.
#   setup.sh                      packages, the run's user, directories and the checkout
#   setup.sh cron                 the cron file that starts the night; after a smoke run passed
#   setup.sh remove               undoes all of the above but the packages, when no run holds the
#                                 lock; a split drive stays split but leaves /srv/perf and fstab
#   setup.sh split-drive DEV --yes
#                                 not part of the setup: takes DEV out of the host's RAID1 mirrors and
#                                 mounts it alone at /srv/perf/data, so the runs' writes stop
#                                 landing on the OS drive. The OS loses its redundancy, so the
#                                 host's administrator decides when.
# Environment: BRANCH (the checkout's branch, default develop), PERF_USER (dittofs-perf), SLACK_DIR
# and SCENARIO_HOST (written into the cron file for the nightly post). The Slack bot's token and
# channel go, by hand, in SECRETS: slack-token and slack-channel, root's and 0600, as SECRETS is 0700.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
CHECKOUT=/srv/perf/dittofs
CRON=/etc/cron.d/dittofs-perf
SECRETS=/etc/dittofs-perf
LOCK=/var/tmp/dittofs-scenarios
PERF_USER="${PERF_USER:-dittofs-perf}"

# installed PACKAGE: whether PACKAGE is installed, not merely known (a removed one keeps its config).
installed() { dpkg-query -W -f='${db:Status-Abbrev}' "$1" 2>/dev/null | grep -q '^ii'; }

refuse() { echo "setup.sh: $*" >&2; exit 2; }

install() {
    local p missing=() go
    # Go, which night.sh builds dfs with: the minor version go.mod pins, as Ubuntu packages it
    # (golang-1.26-go, with golang-go for the go command). night.sh builds with GOTOOLCHAIN=auto, so a
    # later patch or toolchain line on develop fetches its own toolchain.
    go="$(awk '$1 == "go" { split($2, v, "."); print v[1] "." v[2] }' "$REPO/go.mod")"
    for p in fio samba cifs-utils nvme-cli python3 git curl iproute2 util-linux "golang-$go-go" golang-go; do
        installed "$p" || missing+=("$p")
    done
    if test "${#missing[@]}" -gt 0; then
        DEBIAN_FRONTEND=noninteractive apt-get update -qq
        apt-cache show "golang-$go-go" >/dev/null 2>&1 || refuse "this release has no golang-$go-go, the Go that go.mod pins; install Go $go first"
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "${missing[@]}" >/dev/null
    fi
    [[ "$(go version)" == "go version go$go"[.\ ]* ]] ||
        refuse "go on PATH is $(go version | cut -d' ' -f3), go.mod pins $go; put /usr/lib/go-$go/bin first on PATH"
    # The run starts its own smbd; a host that had no Samba keeps none running.
    if [[ " ${missing[*]} " == *" samba "* ]] && command -v systemctl >/dev/null; then
        systemctl disable --now smbd nmbd 2>/dev/null || true
    fi
    # The Unix account the Samba baseline maps its SMB user to; the DittoFS user gets its uid.
    id "$PERF_USER" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$PERF_USER"
    mkdir -p /srv/perf/data /srv/perf/mnt /var/tmp/dittofs-perf
    mkdir -p "$SECRETS"
    chmod 700 "$SECRETS"
    test -d "$CHECKOUT/.git" ||
        git clone -q --branch "${BRANCH:-develop}" https://github.com/marmos91/dittofs "$CHECKOUT"
}

cron() {
    cat >"$CRON" <<EOF
# DittoFS nightly performance run (bench/ditto in $CHECKOUT): as root from 18:45 to its hard stop
# at 05:45, under the lock of the scenario runs (/var/tmp/dittofs-scenarios), so they never overlap.
# Results and logs: /var/tmp/dittofs-perf. Remove with: $CHECKOUT/bench/ditto/setup.sh remove
# CALIBRATION=yes runs every point three times and posts nothing: the first week. With no, it
# posts one summary a night to the channel in SLACK_DIR.
CALIBRATION=yes
SLACK_DIR=${SLACK_DIR:-$SECRETS}
SCENARIO_HOST=${SCENARIO_HOST:-$(hostname -s)}
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
45 18 * * * root $CHECKOUT/bench/ditto/night.sh >>/var/tmp/dittofs-perf/cron.log 2>&1
EOF
    chmod 644 "$CRON"
}

remove() {
    if test -d "$LOCK"; then
        exec 9<"$LOCK"
        flock -n 9 || refuse "a run holds $LOCK; remove when it has ended"
    fi
    rm -f "$CRON"
    umount /srv/perf/mnt/* 2>/dev/null || true
    # A drive that split-drive took out of the mirror stays split; only its mount goes.
    ! mountpoint -q /srv/perf/data || umount /srv/perf/data
    sed -i '/^LABEL=dittofs-perf /d' /etc/fstab
    rm -rf /srv/perf /var/tmp/dittofs-perf "$SECRETS"
    ! id "$PERF_USER" >/dev/null 2>&1 || userdel "$PERF_USER"
}

# split_drive DEV: takes DEV out of the md arrays it mirrors, then makes DEV one ext4 file system
# mounted at /srv/perf/data. It checks first and changes nothing unless all of these hold: DEV is a
# whole disk; neither it nor any of its partitions is mounted or swap by itself (the EFI system
# partition is mounted from the drive the host boots from); and everything using them is an md
# array that is not degraded, so its member on the other drive keeps it whole. To undo: unmount
# /srv/perf/data and drop its fstab line, copy the other drive's partition table onto DEV
# (sfdisk -d OTHER | sfdisk DEV), add each partition back to its array (mdadm /dev/mdN --add PART),
# wait for the resync, then grub-install DEV.
split_drive() {
    local dev="$1" disk sys part holder members=() m
    test "${2:-}" = --yes || refuse "split-drive $dev --yes: it breaks the RAID1 mirror"
    disk="${dev#/dev/}"
    test -d "/sys/block/$disk" || refuse "split-drive: $dev is not a whole disk; nothing was changed"
    for sys in "/sys/block/$disk" "/sys/block/$disk/$disk"*; do
        test -d "$sys/holders" || continue
        part="/dev/${sys##*/}"
        ! findmnt -rn --source "$part" >/dev/null || refuse "split-drive: $part is mounted; nothing was changed"
        ! grep -q "^$part " /proc/swaps || refuse "split-drive: $part is swap; nothing was changed"
        for holder in "$sys"/holders/*; do
            test -e "$holder" || continue
            holder="${holder##*/}"
            [[ "$holder" == md* ]] || refuse "split-drive: $part is used by $holder; nothing was changed"
            test "$(cat "/sys/block/$holder/md/degraded")" -eq 0 ||
                refuse "split-drive: /dev/$holder is degraded, $part may be its last copy; nothing was changed"
            members+=("/dev/$holder $part")
        done
    done
    for m in "${members[@]}"; do
        mdadm "${m% *}" --fail "${m#* }" --remove "${m#* }"
        mdadm --zero-superblock "${m#* }"
    done
    wipefs -a "$dev"
    parted -s "$dev" mklabel gpt mkpart dittofs-perf ext4 1MiB 100%
    udevadm settle
    mkfs.ext4 -q -L dittofs-perf "${dev}p1"
    grep -q '^LABEL=dittofs-perf ' /etc/fstab || echo 'LABEL=dittofs-perf /srv/perf/data ext4 defaults,noatime 0 2' >>/etc/fstab
    mkdir -p /srv/perf/data
    mount /srv/perf/data
}

test "$(id -u)" -eq 0 || { echo "setup.sh: run it as root" >&2; exit 2; }
case "${1:-}" in
"") install ;;
cron) cron ;;
remove) remove ;;
split-drive) split_drive "${2:?setup.sh split-drive DEV --yes}" "${3:-}" ;;
*) echo "usage: setup.sh [cron | remove | split-drive DEV --yes]" >&2; exit 2 ;;
esac
