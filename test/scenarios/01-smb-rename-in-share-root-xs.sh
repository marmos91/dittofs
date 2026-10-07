#!/usr/bin/env bash
# Issue #2907, fixed by PR #2915: a rename in the share root while another client holds the root open,
# as Explorer always does, failed with STATUS_SHARING_VIOLATION. smbclient's notify holds a directory
# open for listing. Controls first: no holder, then a held subfolder.

smbclient //127.0.0.1/test -c 'mkdir "New folder"; rename "New folder" "Named A"'
smbclient //127.0.0.1/test -c 'ls' | grep -q 'Named A'

smbclient //127.0.0.1/test -c 'mkdir sub'
smbclient //127.0.0.1/test -c 'notify sub' >/dev/null 2>&1 &
HOLDER=$!
sleep 2
smbclient //127.0.0.1/test -c 'cd sub; mkdir "New folder"; rename "New folder" "Named C"'
kill "$HOLDER"

smbclient //127.0.0.1/test -c 'notify \' >/dev/null 2>&1 &
HOLDER=$!
sleep 2
smbclient //127.0.0.1/test -c 'mkdir "New folder"; rename "New folder" "Named B"'
smbclient //127.0.0.1/test -c 'put /etc/hostname f.txt; rename f.txt g.txt'
kill "$HOLDER"
smbclient //127.0.0.1/test -c 'ls' | grep -q 'Named B'
smbclient //127.0.0.1/test -c 'ls' | grep -q 'g.txt'
