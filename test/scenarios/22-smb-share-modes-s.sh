#!/usr/bin/env bash
# SMB sharing violations beyond the share root, and the share root behaving like any directory: the
# follow-ups on #2907 / PR #2915 (compare the root's and other opens' fields, audit what reads
# ShareAccess). Each case holds one open (smb-open, for 3 s) and tries a second while it is held.
# The expected status per case follows MS-FSA 2.1.5.1.2: an open with no data access (a stat
# open) neither conflicts nor is conflicted. Every file case runs on f in the share root and on d/f;
# the directory case runs on the share root and on d. Each pair must give the same statuses.
#   access: READ_DATA 0x1, WRITE_DATA 0x2, DELETE 0x10000, MAXIMUM_ALLOWED 0x2000000,
#           READ_ATTRIBUTES|SYNCHRONIZE 0x100080 (a stat open)
#   share:  READ 0x1, WRITE 0x2, DELETE 0x4
# Fails today, at the share root. Every file case matches MS-FSA, alike in the root and in d/.
# But while a lister that shares only reading holds the share root, an add-file open of the root
# succeeds, where d refuses the same open with STATUS_SHARING_VIOLATION.
U=smb://127.0.0.1/test

echo data >/tmp/f.txt
smbclient //127.0.0.1/test -c 'lcd /tmp; put f.txt f; put f.txt f:s; put f.txt f:t; mkdir d; put f.txt d\f; put f.txt d\f:s; put f.txt d\f:t'

# Files. r is f in the share root, s is d/f.
for L in r:f s:d/f; do
    smb-open "$U/${L#*:}" 0x3 0x1 < <(sleep 3) >/dev/null &
    sleep 1
    smb-open "$U/${L#*:}" 0x2 0x7 </dev/null >"/tmp/${L%%:*}.write-vs-share-read" || true
    smb-open "$U/${L#*:}" 0x1 0x7 </dev/null >"/tmp/${L%%:*}.read-vs-share-read" || true
    wait
    smb-open "$U/${L#*:}" 0x100080 0x0 < <(sleep 3) >/dev/null &
    sleep 1
    smb-open "$U/${L#*:}" 0x3 0x7 </dev/null >"/tmp/${L%%:*}.readwrite-vs-stat-open" || true
    wait
    smb-open "$U/${L#*:}" 0x3 0x0 < <(sleep 3) >/dev/null &
    sleep 1
    smb-open "$U/${L#*:}" 0x100080 0x0 </dev/null >"/tmp/${L%%:*}.stat-open-vs-share-none" || true
    wait
    smb-open "$U/${L#*:}" 0x2000000 0x0 < <(sleep 3) >/dev/null &
    sleep 1
    smb-open "$U/${L#*:}" 0x1 0x7 </dev/null >"/tmp/${L%%:*}.read-vs-maximum-allowed" || true
    wait
    smb-open "$U/${L#*:}" 0x10000 0x3 < <(sleep 3) >/dev/null &
    sleep 1
    smb-open "$U/${L#*:}" 0x10000 0x7 </dev/null >"/tmp/${L%%:*}.delete-vs-no-share-delete" || true
    smb-open "$U/${L#*:}" 0x1 0x7 </dev/null >"/tmp/${L%%:*}.read-vs-delete-holder" || true
    wait
    smb-open "$U/${L#*:}:s" 0x3 0x0 < <(sleep 3) >/dev/null &
    sleep 1
    smb-open "$U/${L#*:}" 0x1 0x7 </dev/null >"/tmp/${L%%:*}.file-vs-stream-holder" || true
    smb-open "$U/${L#*:}:t" 0x1 0x7 </dev/null >"/tmp/${L%%:*}.other-stream-vs-stream-holder" || true
    smb-open "$U/${L#*:}:s" 0x1 0x7 </dev/null >"/tmp/${L%%:*}.same-stream-vs-stream-holder" || true
    wait
done

# Directories. r is the share root, s is d: a lister that shares only reading, then an add-file open
for L in r: s:d; do
    smb-open "$U/${L#*:}" 0x1 0x1 < <(sleep 3) >/dev/null &
    sleep 1
    smb-open "$U/${L#*:}" 0x2 0x7 </dev/null >"/tmp/${L%%:*}.dir-add-file-vs-share-read" || true
    wait
done

# Outcomes first, then the checks: the expected status per case, then root against subdirectory
for f in /tmp/s.*; do printf '%-34s root %-26s d %s\n' "${f#/tmp/s.}" "$(cat "/tmp/r.${f#/tmp/s.}")" "$(cat "$f")"; done
grep -qx STATUS_SHARING_VIOLATION /tmp/s.write-vs-share-read
grep -qx STATUS_SUCCESS /tmp/s.read-vs-share-read
grep -qx STATUS_SUCCESS /tmp/s.readwrite-vs-stat-open
grep -qx STATUS_SUCCESS /tmp/s.stat-open-vs-share-none
grep -qx STATUS_SHARING_VIOLATION /tmp/s.read-vs-maximum-allowed
grep -qx STATUS_SHARING_VIOLATION /tmp/s.delete-vs-no-share-delete
grep -qx STATUS_SUCCESS /tmp/s.read-vs-delete-holder
grep -qx STATUS_SUCCESS /tmp/s.file-vs-stream-holder
grep -qx STATUS_SUCCESS /tmp/s.other-stream-vs-stream-holder
grep -qx STATUS_SHARING_VIOLATION /tmp/s.same-stream-vs-stream-holder
grep -qx STATUS_SHARING_VIOLATION /tmp/s.dir-add-file-vs-share-read
for f in /tmp/s.*; do cmp "$f" "/tmp/r.${f#/tmp/s.}"; done
