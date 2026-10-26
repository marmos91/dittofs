#!/usr/bin/env bash
# The ordinary user nfstest's run.sh runs its tests as. Sourced, not
# executed; the functions need root.
#
# As root the tests would mostly exercise root's permission bypass: nfstest
# grants every read and write check to a file root owns. The export squashes
# root, so only the mount is root's.
#
# nfstest expects the user to have passwordless sudo, for mount and tcpdump,
# and passwordless ssh to its second client.

TEST_USER_NAME=dfsnfstest
TEST_USER_UID=64000
TEST_USER_SUDOERS="/etc/sudoers.d/$TEST_USER_NAME"
# The account's comment field marks it as this harness's own. An account of
# the same name without it belongs to someone else, and is left alone.
TEST_USER_MARK="dittofs nfs-mount test user"

# test_user_home — the user's home directory.
test_user_home() { getent passwd "$TEST_USER_NAME" | cut -d: -f6; }

# test_user_ours — true when the account exists and this harness created it.
test_user_ours() { [[ "$(getent passwd "$TEST_USER_NAME" | cut -d: -f5)" == "$TEST_USER_MARK" ]]; }

# test_user_ensure — create the user with sudo and an ssh key that lets it log
# in to itself. A leftover account of this harness's own, from a run that was
# killed, is removed and created afresh.
test_user_ensure() {
    if getent passwd "$TEST_USER_NAME" >/dev/null || getent group "$TEST_USER_NAME" >/dev/null; then
        test_user_ours || {
            echo "a $TEST_USER_NAME account or group exists that this harness did not create; refusing to take it over" >&2
            return 1
        }
        test_user_release
    fi
    if [[ -e "$TEST_USER_SUDOERS" ]]; then
        echo "$TEST_USER_SUDOERS exists and is not this harness's; refusing to overwrite it" >&2
        return 1
    fi
    groupadd -g "$TEST_USER_UID" "$TEST_USER_NAME" || return 1
    # A password of '*' rather than useradd's locked '!': sshd refuses a
    # locked account even for a key.
    useradd -m -u "$TEST_USER_UID" -g "$TEST_USER_UID" -s /bin/bash -p '*' \
        -c "$TEST_USER_MARK" "$TEST_USER_NAME" || return 1

    echo "$TEST_USER_NAME ALL=(ALL) NOPASSWD: ALL" >"$TEST_USER_SUDOERS"
    chmod 0440 "$TEST_USER_SUDOERS"
    visudo -cqf "$TEST_USER_SUDOERS" || { rm -f "$TEST_USER_SUDOERS"; return 1; }

    local ssh
    ssh="$(test_user_home)/.ssh"
    install -d -m 0700 -o "$TEST_USER_NAME" -g "$TEST_USER_NAME" "$ssh" || return 1
    ssh-keygen -q -t ed25519 -N '' -f "$ssh/id_ed25519" || return 1
    cp "$ssh/id_ed25519.pub" "$ssh/authorized_keys"
    # The second client is a fresh namespace every run, with a fresh host key.
    printf '%s\n' 'Host *' '    StrictHostKeyChecking no' \
        '    UserKnownHostsFile /dev/null' '    LogLevel ERROR' '    BatchMode yes' >"$ssh/config"
    chown -R "$TEST_USER_NAME:$TEST_USER_NAME" "$ssh"
    chmod 0600 "$ssh/id_ed25519" "$ssh/authorized_keys" "$ssh/config"
}

# test_user_release — withdraw the sudo grant and remove the account, its home
# and its key. Touches only an account this harness created.
test_user_release() {
    test_user_ours || return 0
    rm -f "$TEST_USER_SUDOERS"
    pkill -KILL -u "$TEST_USER_NAME" 2>/dev/null
    userdel -r "$TEST_USER_NAME" 2>/dev/null
    getent group "$TEST_USER_NAME" >/dev/null && groupdel "$TEST_USER_NAME" 2>/dev/null
    return 0
}

# test_user_sshd_config DIR ADDR — write an sshd config that admits only the
# test user, on ADDR, with a host key generated into DIR, a new directory under
# a private parent. Prints its path.
test_user_sshd_config() {
    local dir="$1" addr="$2"
    mkdir -m 0700 "$dir" || return 1
    ssh-keygen -q -t ed25519 -N '' -f "$dir/host_key" || return 1
    cat >"$dir/sshd_config" <<EOF
ListenAddress $addr
HostKey $dir/host_key
PidFile /run/sshd-dfsnfs.pid
AllowUsers $TEST_USER_NAME
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PrintMotd no
EOF
    echo "$dir/sshd_config"
}

# test_user_copy SRC DEST — a copy of SRC at DEST, a path that must not exist
# yet, owned by the user. The suites are fetched into the checkout, which an
# ordinary user may not be able to reach when the home directory above it is
# not world-readable.
test_user_copy() {
    cp -a "$1" "$2" && chown -R "$TEST_USER_NAME:$TEST_USER_NAME" "$2"
}
