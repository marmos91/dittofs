# Running a fix on real hardware (Scaleway)

Read this only when a laptop genuinely cannot prove the fix: crash and
device-loss behaviour, real kernel NFS/SMB client interaction, throughput, or
anything where an in-memory double would lie. Everything else stays local —
a VM costs real money per hour and adds an hour of setup.

The `scw` CLI is already authenticated.

## Before you touch any VM: the identity check

`.bench-vm.json` in the repo root records the *most recent* provisioned VM. It
is not necessarily yours. Development VMs have been recorded there, and
`dfsbench teardown` will happily terminate whatever it finds.

Destroying someone's development VM is unrecoverable, so the rule is absolute:
**never tear down a server whose `server_id` you did not personally record when
you created it.**

```bash
[ -f .bench-vm.json ] && cat .bench-vm.json || echo "no .bench-vm.json — nothing recorded"
scw instance server list    # confirm name/tags match the VM you created
```

No `.bench-vm.json` means no VM is recorded, so there is nothing to tear down —
that is a clean state, not an error.

If the recorded `server_id` is not the one you provisioned, do not tear down.
Stop and report — this is one of the skill's hard stop conditions.

## Provisioning

For benchmark-shaped work, `dfsbench` owns the whole lifecycle and writes
`.bench-vm.json` itself:

```bash
dfsbench setup      # SCW_* env selects type/zone/image
dfsbench teardown   # terminates the VM in .bench-vm.json — after the identity check
```

Environment that shapes the instance:

```
SCW_ZONE=fr-par-1
SCW_INSTANCE_TYPE=POP2-8C-32G
SCW_IMAGE=ubuntu_noble
SCW_ROOT_VOLUME=sbs:100GB:5000
SCW_NAME=<something identifying this issue, e.g. dfs-1909-crash>
SCW_SSH_KEY=<key>
```

Name the VM after the issue — that name is how anyone later tells your VM from a
development one.

For a plain repro box unrelated to benchmarking, provision directly with
`scw instance server create` and record the returned ID somewhere you control
rather than overwriting `.bench-vm.json`.

## SSH

User is `root`. The Scaleway key lives in the **1Password agent**, which refuses
to sign for ssh invoked from the Bash tool. Start a dedicated agent instead:

```bash
ssh-agent -a "$HOME/.dfsb.sock" >/dev/null
SSH_AUTH_SOCK=$HOME/.dfsb.sock ssh-add ~/.ssh/<scw-key>
SSH_AUTH_SOCK=$HOME/.dfsb.sock ssh root@<ip>
```

## Long runs

Anything longer than a few minutes runs detached on the VM. Poll it; do not
block on it — a `--remote` poller hangs silently when the remote side stalls.

Aborting a detached `dfsbench` run needs two kills — the local poller and the
remote binary:

```bash
pkill -f 'dfsbench run'                          # local
ssh root@<ip> 'pkill -9 -x dfsbench; pkill -9 -x fio'   # remote
```

Use `-x` (exact name) on the remote side. `pkill -f dfsbench` matches your own
ssh command line and kills the session before the command runs.

## Two-build verification

When the question is "does this fix actually change behaviour", one build proves
nothing. Push both a pre-fix and a post-fix binary to the same VM and run the
same reproduction against each. A repro that fails on the old build and passes
on the new one is evidence; a repro that only passes on the new one is a guess.

## Teardown

Tear down as soon as the evidence is captured — pull results back first, because
the VM's disk goes with it.

```bash
dfsbench teardown     # or: scw instance server terminate <your-server-id> with-block=true
```

Verify it is gone rather than trusting the exit code:

```bash
scw instance server list
```

If teardown fails because the server is already gone, that is fine. If it fails
for any other reason, retry — a leaked VM bills until someone notices.
