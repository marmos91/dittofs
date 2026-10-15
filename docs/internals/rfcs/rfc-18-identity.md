---
rfc: 18
title: "RFC 18 — identity and authentication"
component: identity
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-26-catalog-backups]]"
aliases:
  - RFC 18
  - identity and authentication
tags:
  - rfc
---
# RFC 18 — identity and authentication

**Status:** draft. [§12](#12.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing how a client signs in, how a user, group or
Windows SID becomes a principal, how squashing works, or any cache keyed on who
the caller is.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Every call a client makes is made by someone. This RFC says who, and how we
know. It covers three steps.

1. **Authentication.** The adapter runs the protocol's handshake — an NFS
   credential, a Kerberos ticket, an NTLM exchange — and hands the filesystem
   service what the handshake proved: an `Identity`
   ([RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)). Where the handshake
   needs a secret or a directory, the adapter calls the authentication
   interface this RFC defines (§3).
2. **Mapping.** The service turns what was proved — a Kerberos name, a SID, a
   numeric uid — into a **principal**: the minted, never-reused ID that files,
   ACLs, grants and quotas name
   ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)). Mapping
   runs in both directions, because the adapters must also show a stored
   principal on the wire, as a uid, a SID or an `owner@domain` string (§4).
3. **Resolution per share.** On every call, the service applies the share's
   policy: squashing replaces the principal with the share's anonymous one, and
   the share grants decide whether the caller is `authenticated`, `admin` or
   privileged on that share (§6). The result is the **caller** — what the call
   acts as — which the metadata store's one permission check reads
   ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)).

What the caller may then do to a file is [RFC 19](rfc-19-authorization.md)'s.
This RFC stops once the caller is known.

**The problem, with one example.** alice and bob are users in the company's
directory. Neither has a Unix uid recorded there. A server that fills a missing
uid with a default — 1000, say — gives both of them uid 1000.

alice signs in on alice-pc over SMB. Her Kerberos ticket carries her Windows
groups, among them `Finance`. bob mounts `builds` from build01 with `sec=krb5`.
The NFS adapter, seeing uid 1000 for both, caches "uid 1000 on `builds`" with
the identity it built first: alice's, with `Finance`. bob's next call hits that
entry. He now matches every ACL entry that grants `Finance`, on a share where
alice's profile container is one `cd` away through a shared path. Nothing
errors and no test goes red: every test used one user.

With this RFC:

1. alice and bob are two principals with two minted IDs. A missing uid is
   never defaulted; each gets one allocated from the installation's range,
   recorded once and never reissued (§2.3).
2. Every cache of who the caller is keys on what was proved — the flavour, the
   Kerberos name, the SIDs — never on a uid alone (§7.1). bob's call misses
   alice's entry.
3. The conformance suite signs two principals in with the same uid and asserts
   that each gets only its own groups (§11).

![From a credential to the caller: the adapter runs the handshake, calling the authentication interface for secrets, and hands the service an Identity holding only what was proved; the service maps it to a principal through the store's protocol-ID index and the directory cache, admits it to the share, squashes it if the policy says so, resolves its share grant, and sets authenticated, admin and privileged; the chokepoint reads the caller](img/rfc18-resolution.svg)

**The words you need.**

- **principal** — a user, group, machine or well-known identity, named by a
  minted `PrincipalID` that is never reissued
  ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)).
- **spelling** — one protocol's name for a principal: a uid or gid, a SID, a
  Kerberos name, a user name, an `owner@domain` string. A principal has at most
  one spelling per scheme (§4).
- **flavour** — how a request's sender was identified: `AUTH_NONE`, `AUTH_SYS`,
  Kerberos (over RPCSEC_GSS or SMB), NTLMv2, or an anonymous SMB session.
  `AUTH_SYS` and `AUTH_NONE` prove nothing; the others prove a name (§3).
- **proof** — what one handshake established: the flavour, the name, the
  verified group SIDs it carried, the session key. The adapter keeps it in the
  connection's security context and builds each request's `Identity` from it
  (§3.1).
- **directory** — an external source of users and groups, such as Active
  Directory over LDAP, consulted when the store has no record (§5).
- **squash** — replacing the caller's principal with the share's anonymous
  principal: root squash for uid 0, all squash for everyone (§6.2).
- **caller** — the principal a call acts as on one share, with its groups and
  the `authenticated`, `admin` and privileged flags (§6.5).

**What this RFC promises.**

- Two different proofs never collapse into one caller, whatever numeric IDs
  they share, and no cache can make them.
- An ID, once given to a principal, is never given to another — not a
  `PrincipalID`, not an allocated uid, gid or SID.
- An outage of the store or the directory refuses calls it cannot decide; it
  never grants the share's default or a guest's access in their place.
- A failed sign-in is a failed sign-in, never a guest session.
- One mapping, used by both protocols: alice is the same principal over NFS
  and SMB, and a file she creates over one shows her as owner over the other.
- Adapters authenticate and encode; they never map, squash or grant.

**How the rest is organised.** §1 gives the purpose and non-goals. §2 defines
principals and the rule that keeps their IDs stable. §3 is authentication:
the interface adapters call and each flavour's rules. §4 is mapping in both
directions. §5 is the directory, its cache and what a stale entry may grant.
§6 is resolution per share: squashing, groups, grants and the caller's flags.
§7 is caches. §8 separates management-API users from file principals. §9
collects failure modes. §10 and §11 are invariants, metrics and conformance;
§12 lists open questions. Appendix A is prior art, Appendix B where the
current code differs.

### Scenarios

**alice signs in on alice-pc and opens her profile container.**

1. alice-pc sends `SESSION_SETUP` with a Kerberos ticket for
   `cifs/files.corp.example`. The SMB adapter calls `AcceptKerberos` (§3.1),
   which decrypts the ticket with the key from the installation's keytab,
   checks its times and addresses, and verifies the ticket's PAC — the
   Windows group list the domain controller signed (§3.4).
2. The proof is `alice@CORP.EXAMPLE`, Kerberos, her SID and twelve group SIDs.
   The adapter keeps it with the session's keys.
3. Her first `CREATE` on `profiles` carries an `Identity` built from that
   proof. The service maps the name through the store's `krb` index to alice's
   principal (§4.2) and her group SIDs to their group principals.
4. Admission passes; the share squashes nothing; alice's grant is read-write.
   The caller is alice, `authenticated`, not `admin`. The chokepoint walks the
   file's ACL against her and her groups.

Outcome: one ticket, one principal, the same on every call of the session.

**build01's root runs `tar xpf` on `builds`, which squashes root.**

1. build01 sends `AUTH_SYS` with uid 0, gid 0. `AUTH_SYS` proves nothing; the
   uid is the client's assertion (§3.2).
2. Admission finds `Squash = root`. uid 0 is replaced by the share's anonymous
   principal, uid 65534 (§6.2). The caller is not privileged and not
   `authenticated`.
3. `tar` tries to `chown` a file to uid 1500. The chokepoint refuses: changing
   an owner to another principal needs privilege
   ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)).
   The client sees `EPERM`.

Outcome: a client's root is the share's anonymous user, as on a Linux NFS
server with `root_squash`.

**The domain controller stops answering during a sign-in storm.**

1. At 08:55 the directory stops answering LDAP. Two hundred users are signing
   in.
2. Those whose directory entries the node resolved within the last
   20 minutes — the 5-minute TTL plus a 15-minute grace — are served from the
   cache (§5.3). Kerberos keeps working for them: the
   tickets were issued earlier and verify against the keytab alone.
3. A user the node has never resolved, or resolved too long ago, is refused —
   `STATUS_LOGON_FAILURE` — and the refusal is counted
   (`identity_directory_refusals_total`). Nobody is let in as a guest.

Outcome: known users keep working for a bounded time; nobody new gets in
until the directory answers.

**An administrator disables bob while bob-pc holds open handles.**

1. The management API writes bob's user record with `Disabled` set, which
   raises the principals generation (§7.2).
2. Within one second every node's resolver sees the new generation and drops
   its cached resolutions. bob's next call resolves afresh and is refused.
3. His open handles stay, but every call through them is refused, as
   [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)
   admits every call.

Outcome: a local change takes effect within a second, on every protocol.

**bob lists `builds` over NFSv4 from build01.**

1. `GETATTR` returns `owner` for each file. On this `sec=krb5` mount the
   adapter asks the service for each owner's spelling in the `owner@domain`
   scheme and gets `bob@corp.example` (§4.5).
2. A file owned by a principal deleted last year has no spelling: the adapter
   shows `nobody@corp.example`.
3. `chown alice file` sends `alice@corp.example`; the service maps it back to
   alice's principal. A name in another domain is refused
   `NFS4ERR_BADOWNER`.

Outcome: names round-trip through one mapping; unknown ones fail visibly.

## In short

- A principal is a minted ID. Its uid, gid, SID and names are spellings, each
  unique in its scheme, each stored once with the principal
  ([RFC 16](rfc-16-metadata-store.md)). No ID is defaulted, and none is reused.
- Adapters run handshakes and call one authentication interface: verify an
  NTLMv2 response, accept a Kerberos ticket against the keytab, pass NTLM
  through to a domain controller. The result is a proof, held in memory.
- `AUTH_SYS` and `AUTH_NONE` prove nothing: their callers are never
  `authenticated`, never `admin`, and an unmapped uid acts as the anonymous
  principal.
- The service maps a proof to a principal on every call, through a cache keyed
  by the whole proof, then squashes, resolves the share grant and sets the
  caller's flags.
- The directory fills in principals the store does not hold. Its answers are
  cached with a TTL; when it is down, known principals are served for a
  bounded grace and unknown ones are refused.
- Errors fail closed. A store or directory error is never read as "unknown
  user", and unknown is never read as "guest".
- Management-API roles grant nothing on files; that API's own sign-in is
  planned RFC 23's.

---

## 1. Purpose

Identity is decided in many places when nothing owns it: each adapter maps
names its own way, each cache keys on the fields its author thought of, and
each outage path picks its own fallback. Each copy is where a gap opens — a
cache key that misses a field, an error read as "not found", a protocol that
lets a disabled user through. This RFC owns identity once:

- the principal kinds and the rule that keeps their IDs stable (§2);
- the authentication interface adapters call, and each flavour's rules (§3);
- the mapping between principals and every protocol spelling (§4);
- directory integration, its cache and its failure behaviour (§5);
- resolution per share: squashing, group membership, share grants and the
  caller's flags (§6);
- the keys of every cache of identity (§7).

### 1.1 Non-goals

This RFC **MUST NOT** be read as specifying:

- what a caller may do to a file — ACL semantics, the walk, and the mappings
  between the stored ACL, SMB security descriptors and NFSv4 ACLs are
  [RFC 19](rfc-19-authorization.md)'s, and the chokepoint is
  [RFC 7](rfc-7-namespace-metadata.md)'s;
- the records themselves — `User`, `Group`, `Membership`, `ShareGrant`,
  `ExportPolicy`, `Secret` and the protocol-ID index are
  [RFC 16](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)'s;
  this RFC says what their identity fields mean and how they are used;
- the order of admission — share state, flavour, client rules, squash — which
  is [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s;
  this RFC defines the squash step and what admission's flavour rule means by
  "authenticated";
- the wire form of a handshake — RPCSEC_GSS framing, SPNEGO, NTLM messages,
  signing and encryption — which [RFC 21](rfc-21-nfs.md) and
  [RFC 22](rfc-22-smb.md) own;
- how the management API authenticates its users, which is planned RFC 23's
  (§8);
- where secrets are stored and how they rotate, which is
  [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)'s.

## 2. Principals

### 2.1 Kinds

A principal is a `Principal` value
([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)): a kind and a
minted ID. Every principal a call can act as, and every principal a file, ACL,
grant or quota can name, is one of these:

| Kind | Where its record comes from | Spellings it carries | Can it sign in? |
| --- | --- | --- | --- |
| **local user** | the management API: a `User` record | name, uid, gid of its primary group, SID; optionally a Kerberos name | NTLMv2 against its NT hash; Kerberos if a KDC issues it tickets; `AUTH_SYS` by uid |
| **directory user** | minted on first resolution from the directory (§5), then a `User` record like any other | the directory's name and SID; uid and gid from the directory or allocated (§4.3) | Kerberos; NTLM pass-through; `AUTH_SYS` by uid |
| **group** | a `Group` record, local or minted from the directory | name, gid, SID | never |
| **machine** | minted on first Kerberos authentication of a host or service name (§2.4) | its Kerberos name | Kerberos only |
| **well-known** | fixed IDs, the same in every installation ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)) | the SIDs and NFSv4 names [RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode) lists | never, except **anonymous** (below) |

**Anonymous** is the well-known principal an `AUTH_NONE` caller, an anonymous
SMB session and a squashed caller act as, unless the share's policy names
another (§6.2). Its uid and gid are 65534 and its SID is `S-1-5-7`
(`ANONYMOUS LOGON`). It is never `authenticated` and never `admin`.

**Administrator is not a kind.** A principal is an administrator of a share
when that share's grant names it `admin`
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
and that counts only on an authenticated flavour (§6.5). No record, uid, group
name or management-API role makes a principal an administrator of files
(§8).

**Guest is not a kind either.** An SMB client that asks for a guest session
gets an anonymous one (§3.6).

### 2.2 Spellings

A principal's protocol spellings are held on its `User` or `Group` record and,
each, as one row of the protocol-ID index that maps it back
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)).
The schemes are:

| Scheme | Spelling | Unique among | Who has one |
| --- | --- | --- | --- |
| `uid` | a 32-bit number | users | every user, always (§2.3) |
| `gid` | a 32-bit number | groups | every group, always (§2.3) |
| `sid` | a Windows SID, binary form | users and groups together | every user and group, always (§4.4) |
| `krb` | `name@REALM`, as the KDC spells it | users and machines | users that sign in with Kerberos under a name §4.2's rule does not derive; every machine |
| user or group name | the record's `Name` | users; groups | every user and group |

- Every spelling **MUST** be unique in its scheme. A write that would give a
  second principal a spelling another holds **MUST** fail, as RFC 16's index
  makes it.
- The kind decides the scheme a number is in: uid 1500 and gid 1500 are two
  spellings of two principals, or of none.
- A principal **MUST NOT** be told apart from another by any spelling. Every
  comparison of identity — a cache key, an open's principal
  ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), a client record's creator
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), a quota charge — compares
  `PrincipalID`s.

### 2.3 IDs are never defaulted and never reused

**The stable-ID rule.** An ID given to a principal is never given to another:

1. `PrincipalID` is minted at random and never reissued
   ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)).
2. A uid, gid or RID (the last part of a SID, §4.4) that the installation
   allocates is drawn from a counter per scheme that only rises. An allocated
   ID **MUST NOT** be allocated again, even after its principal is deleted.
3. A uid or gid the operator assigns, or the directory supplies, **MAY** be
   any value no current principal holds. Assigning a deleted principal's uid
   to a new one is the operator's choice; it gives the new principal no file of
   the old one's, because files name principals, never uids.
4. A missing uid or gid **MUST NOT** be filled with a default. A user created
   without one, or resolved from a directory that has none, gets one allocated
   from the range `identity.id_range` (default 1 000 000 to 1 999 999 999) in
   the transaction that creates the record (§4.3).

> decision: a missing uid is allocated, never defaulted. A default makes every
> principal without one the same principal to every check that reads a uid —
> `AUTH_SYS` admission, an NFSv3 attribute, a cache key — and that is the bug
> the opening example walks through. The cost is that an operator who creates
> a user without a uid sees a number they did not choose; they may change it
> before the user owns anything. Revisit never: the alternative is the bug.

**Changing a spelling.** An operator **MAY** change a user's uid or a group's
gid. The change writes the record and its index rows in one transaction and
touches no file. Over NFS the principal's files then show the new number;
`AUTH_SYS` clients that still send the old one act as whoever now holds it, or
as anonymous (§4.3). A SID **MUST NOT** change once the principal has one:
it is what Windows clients cache and store in their own ACLs.

### 2.4 Machine principals

A machine principal is a host or service signing in for itself: the
`host/build01.corp.example@CORP.EXAMPLE` or `nfs/build01…` name an NFS client
uses for its lease and state operations, or a Windows machine account
`BUILD01$@CORP.EXAMPLE`.

- A Kerberos name whose first component is a service name (`host`, `nfs`,
  `cifs`, or any name with a `/`), or whose user part ends in `$`, is a
  machine name.
- The first successful Kerberos authentication of a machine name with no
  record **MUST** mint a machine principal for it, with that `krb` spelling
  and no other. A Kerberos ticket is a KDC's proof, so only names the KDC
  issues can be minted.
- A machine principal acts on files as the share's anonymous principal, unless
  the operator links the machine name to a user (§4.2). It is `authenticated`;
  a grant can name it.
- It is the principal NFSv4.1 state protection records
  ([RFC 21 §3.3](rfc-21-nfs.md#3.3%20Trunking%2C%20server%20owner%20and%20scope),
  RFC 14's `MachCred`), so two machines are two principals.

> decision: machine principals are minted on sight, user principals are not
> (§3.3). A machine needs a principal of its own for state protection to mean
> anything, and minting one grants no file access, since it acts as anonymous.
> A user principal minted on sight would get the share's everyone and
> authenticated-users grants with no operator ever having seen it. Revisit if
> a KDC is shown to issue machine tickets to names the operator does not
> control.

### 2.5 Groups and membership

A principal's groups are the closure of its memberships: the `Membership`
records of the store
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
nested groups included, and the groups a directory or a verified PAC reports
(§5, §3.4).

- The closure **MUST** be computed whole, or not at all. A resolution that
  cannot read every membership — a store error, a directory timeout, a cycle
  it cannot finish — **MUST** fail, never return the part it read. A missing
  group drops a deny entry as surely as an allow, and a dropped deny fails
  open.
- A principal in more groups than `identity.max_groups` (default 1 024)
  **MUST** be refused at resolution, with the count logged.
- A group's membership never makes a principal `admin`; only a grant does
  (§6.5).

> decision: a principal over the group cap is refused, not truncated. A
> truncated group list is a wrong answer that looks right, and it can drop the
> group a deny entry names. 1 024 sits just above what a Windows token carries
> by default, so a real account rarely meets it. Raise the default, or make
> the cap per share, when a deployment shows accounts above it.

## 3. Authentication

### 3.1 The authentication interface

Adapters run handshakes; this interface does the parts that need a secret or
a directory. It is what [RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity) calls
"the authentication interface RFC 18 defines". Signatures are indicative:

```go
package identity

// Authenticator is what an adapter calls during a handshake. It returns a
// Proof, which the adapter keeps in the connection's security context and
// copies into every request's Identity (RFC 17 §3). It never returns a
// principal's uid, groups or grants: those are the service's, per call (§6).
type Authenticator interface {
	// VerifyNTLM checks an NTLMv2 AUTHENTICATE message against the challenge
	// the adapter sent: locally against the principal's NT hash, or by
	// pass-through to a domain controller (§3.5).
	VerifyNTLM(ctx context.Context, m NTLMAuthenticate, challenge [8]byte) (Proof, error)
	// AcceptKerberos accepts a Kerberos AP-REQ for one of the installation's
	// service names, using the keytab's keys, and verifies a PAC if present
	// (§3.3, §3.4).
	AcceptKerberos(ctx context.Context, apreq []byte, service Service, client netip.Addr) (Proof, AcceptResult, error)
}

// Proof is what one handshake established, and nothing more.
type Proof struct {
	Flavor     Flavor        // krb5, ntlm (an anonymous session and AUTH_SYS need no call)
	Name       string        // canonical: "alice@CORP.EXAMPLE", "CORP\\alice", a local user name
	Principal  Principal     // set only when the authenticator read it from the store itself (local NTLM)
	SIDs       []SID         // from a verified PAC or a pass-through reply: user SID first; else empty
	Machine    bool          // §2.4
	SessionKey []byte        // for the adapter's signing and encryption; never copied into an Identity
	Expires    time.Time     // the ticket's end time; zero for NTLM
}
```

- Every error from the interface **MUST** be one of: the credential is wrong
  (`ErrLogonFailure`), the account cannot sign in (`ErrAccountDisabled`), the
  message is malformed (`ErrInvalid`), or the answer could not be decided
  (`ErrUnavailable` — a store, keytab or directory fault). The adapter answers
  the first three as the protocol's logon failure and the last as a retryable
  failure where the protocol has one; none of them as success.
- An adapter **MUST NOT** turn any of these errors into a guest or anonymous
  session (§3.6).
- The authenticator reads only the `Principals` view and the secret provider
  ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer),
  [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)), and runs on the
  `protocol` node that holds the connection.
- A proof is valid until the connection's security context ends, or until
  `Expires`. A request on a context past `Expires` **MUST** be refused with
  the protocol's expired-credential answer, so the client renews its ticket.

### 3.2 AUTH_SYS and AUTH_NONE prove nothing

`AUTH_SYS` carries a uid, a primary gid and up to 16 supplementary gids, as
the client asserts them. It is decoded by the NFS adapter and needs no call to
the authenticator. Its rules:

- The uid and gids are taken as sent. They are mapped through the `uid` and
  `gid` indexes on every call (§4.3); nothing about them is verified.
- The credential's machine name and stamp **MUST** be ignored: they are as
  asserted as the uid.
- A credential with more than 16 supplementary gids is malformed on the wire
  (RFC 5531), which RFC 21's decoder refuses.
- An `AUTH_SYS` caller is **never** `authenticated`, and an `admin` grant
  confers nothing on it. Only an unsquashed uid 0 is privileged
  ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)).
- Which hosts may send `AUTH_SYS` at all is the share's flavour and client
  rules ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)):
  the only thing that makes an asserted uid worth anything is the network it
  came from.

`AUTH_NONE` carries nothing. Its caller acts as the share's anonymous principal
(§6.2), is never `authenticated` and never privileged. `AUTH_DH` and every
other flavour **MUST** be refused as `AUTH_BADCRED` (RFC 21 decides the
wire answer).

**Supplementary groups past 16.** `AUTH_SYS` cannot carry more than 16. A
share whose `ExportPolicy` sets `ManageGIDs` replaces the credential's gids
with the closure of the mapped principal's memberships (§2.5), as a Linux
server's `--manage-gids` does: the principal is found by uid, and the gids sent
are discarded. Without it, the gids sent are the caller's groups.

> decision: `ManageGIDs` is off by default. With it on, a principal's groups
> are what the store says, which is right where the store is the source of
> truth for groups and wrong where clients get groups from a local
> `/etc/group` the store has never seen. Off keeps NFS's usual behaviour.
> Turn the default on if most deployments are shown to hold groups in the
> store or a directory.

### 3.3 Kerberos

Kerberos is accepted over RPCSEC_GSS ([RFC 21 §10](rfc-21-nfs.md#10.%20RPCSEC_GSS))
and inside SMB's SPNEGO ([RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)).
Both call `AcceptKerberos`.

- **Service names.** The installation accepts tickets for the service names
  its keytab holds — `nfs/<host>` for NFS, `cifs/<host>` for SMB, and `host/<host>`
  for either — for every host name a `protocol` node answers on. A ticket for
  any other name **MUST** be refused.
- **The keytab is a secret.** Each key is a `Secret` record of kind keytab,
  sealed under the `protocol` wrapping key
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)). A ticket whose key
  version the keytab holds in any version still resolvable is accepted, so a
  key rotated at the KDC does not fail tickets issued under the old key until
  every node holds the new one.
- **Checks.** The authenticator **MUST** verify the ticket's encryption, its
  start and end times within `identity.clock_skew` (default 5 minutes, as
  Kerberos implementations use), the authenticator's freshness against a
  replay cache held in the node's memory, and, over SMB, the client-address
  rule [RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in) states.
- **The name.** The proof's `Name` is the ticket's client name and realm,
  exactly as the ticket spells them. Mapping it to a principal is §4.2's.
- **An authenticated user name that maps to no principal** — no `krb` row, no
  local user by §4.2's rule, and no directory entry — **MUST** be refused at
  authentication with `ErrLogonFailure`. A machine name is minted instead
  (§2.4).

> decision: an unknown user principal is refused, not made anonymous. A
> Kerberos user the installation has no record of is either a directory
> misconfiguration or someone outside the organisation the KDC trusts; acting
> as anonymous would hide the first and admit the second to every share with
> an everyone grant. Revisit if a deployment is shown to need cross-realm
> users with no directory, and then as a share-scoped option.

### 3.4 The PAC

A Kerberos ticket from Active Directory carries a PAC: the user's SID, its
group SIDs and their attributes, signed by the domain controller.

- The PAC's server signature **MUST** verify under the service key before any
  SID in it is used. A PAC that does not verify **MUST** fail the
  authentication; a ticket without a PAC is accepted, with groups then read
  from the store and the directory (§2.5).
- The proof's `SIDs` are the user SID and every group SID the PAC lists:
  its group memberships, extra SIDs and resource groups, excluding any whose
  attributes mark them disabled.
- PAC groups are the caller's groups as of the ticket's issue. Resolution
  (§6.3) unions them with the store's memberships; a PAC never removes a group
  the store records.
- The PAC is the one source of groups that needs no directory call, which is
  why a Kerberos sign-in keeps working while the directory is down (§5.3).

### 3.5 NTLMv2

NTLMv2 is accepted inside SPNEGO when `identity.ntlm` is on (the default).
NTLMv1 and LM are refused ([RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)).
`VerifyNTLM` decides one message from one of two sources:

| The message's domain | Source | How |
| --- | --- | --- |
| empty, or the installation's own NetBIOS name | **local** | the user is found by name; its NT hash, a `Secret` of kind nt-hash, verifies the NTLMv2 response computed with the user and domain as the message names them (MS-NLMP 3.3.2) |
| the directory's NetBIOS domain, with pass-through configured | **pass-through** | the response is sent to a domain controller over the secure channel of the installation's machine account (MS-NRPC `NetrLogonSamLogonEx`), whose password is a `Secret`; the reply carries the user's SID and group SIDs, used as a PAC's are (§3.4) |
| anything else | — | `ErrLogonFailure` |

- An NT hash **MUST** be stored only while `identity.ntlm` is on. Turning NTLM
  off **MUST** delete every stored NT hash, since an NT hash is equivalent to
  the password for NTLM. Turning it on again stores a hash only at a user's
  next password set.
- A local user with no NT hash **MUST** fail `ErrLogonFailure`; it never falls
  through to pass-through or to anonymous.
- The NTLMv2 session key is weaker than a Kerberos one. Which shares may be
  reached over an NTLM session is the share's flavour rule
  ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)),
  where `ntlm` is a flavour like `krb5`.

> decision: NTLM is on by default. Workgroup deployments with local users have
> no KDC, and macOS and Linux SMB clients outside a domain fall back to NTLM.
> A deployment that has Kerberos everywhere turns it off, which also removes
> every NT hash from the store. Revisit the default once local users are shown
> to be rare.

> ponytail: one directory, one domain for pass-through. A message naming a
> trusted domain of the directory's forest is refused. Follow trusts — find
> the domain's controller and route the pass-through there — when a
> deployment shows users from a second domain signing in with NTLM.

### 3.6 Anonymous and guest sessions

An SMB client may set up a session that proves nothing: an anonymous one, with
no user, or a guest one, naming the guest account.

- Both **MUST** be set up as **anonymous** sessions: the proof is empty, the
  caller is the anonymous principal, and the session holds no key.
- [RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in) confines such a session to
  `IPC$`. This RFC admits it on those terms and widens nothing: no setting
  lets a keyless session reach a share.
- A session setup whose credential fails **MUST** fail. It **MUST NOT** become
  an anonymous or guest session — not for an unknown user, not for a wrong
  password, not for a directory error.

> decision: no "map to guest". A server that answers a wrong password with a
> guest session turns every typo into a session the client did not ask for,
> and turns a directory outage into access. Because keyless sessions reach
> only `IPC$`, there is nothing a guest could reach that anonymous cannot, so
> one anonymous principal covers both. Revisit only if `IPC$` confinement is
> ever withdrawn.

### 3.7 Disabled and deleted principals

- A principal whose record is `Disabled`, or marked deleted
  ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)),
  **MUST** fail authentication with `ErrAccountDisabled`, and **MUST** be
  refused at resolution on every call, whatever proof its connection holds.
  The second rule is what stops a session or GSS context set up before the
  change; it takes effect within §7.2's bound.
- A disabled directory account is refused by the directory's own answer, at
  the next lookup (§5.3).
- A disabled user's open handles stay open; every call through them is
  refused.

## 4. Identity mapping

![The mapping triangle: a principal's minted ID at the centre, joined to its uid or gid, its SID and its names — user name, Kerberos name, owner@domain — each spelling stored once with the record and as one index row back to the principal; from wire to principal through the index, from principal to wire through the record; a spelling with no principal behind it maps to anonymous for AUTH_SYS, refusal for a name, and an unknown SID for a SID](img/rfc18-mapping.svg)

### 4.1 Two directions, one mapping

Mapping runs in two directions, and both use the same records:

- **Wire to principal**, on every call and every name a client sends: a
  Kerberos name, a uid, a SID, an `owner@domain` string, a name in an `lsarpc`
  lookup. It reads the protocol-ID index.
- **Principal to wire**, whenever an adapter shows a stored principal: a
  file's owner and group in NFS attributes or an SMB security descriptor, an
  ACL entry's principal, a quota entry. It reads the principal's record.

The filesystem service exposes both to adapters as lookups, so an adapter
asks and never decides:

```go
// Identities is the group of vfs.Service an adapter calls to encode and
// decode principals (RFC 17 §3.1). It holds no policy: squashing and grants
// are applied at admission, not here.
type Identities interface {
	// Spell returns p's spelling in scheme, or ErrNoSpelling.
	Spell(ctx context.Context, p metadata.Principal, scheme Scheme) (Spelling, error)
	// Find returns the principal holding s, or ErrUnknownPrincipal.
	Find(ctx context.Context, s Spelling) (metadata.Principal, error)
}
```

- An adapter **MUST** encode and decode every principal through these calls,
  and **MUST NOT** derive one spelling from another — a SID from a uid, a uid
  from a RID, a name from a SID.
- A lookup error other than "not found" **MUST** reach the client as a server
  failure, never as "unknown principal": a store outage that made every owner
  `nobody` would be read by a client as data.

### 4.2 Kerberos names

A Kerberos name maps to a principal in this order, stopping at the first that
answers:

1. the `krb` index row for the name exactly as the ticket spells it — an
   operator's explicit link, or a minted machine principal;
2. if the realm is one of `identity.local_realms` (default: the realms of the
   keytab's service names), the user whose name equals the name's user part,
   compared as user names are (§4.6);
3. the directory, by the name's user part, if the realm is the directory's
   realm (§5);
4. otherwise none: a user name is refused (§3.3), a machine name minted
   (§2.4).

A realm in the name **MUST** be compared as the KDC spells it, case and all.
A name with a service part (`nfs/build01`) never matches step 2 or 3.

### 4.3 uid and gid

- A uid maps to the user holding it in the `uid` index; a gid to the group in
  the `gid` index.
- **An `AUTH_SYS` uid with no principal behind it** acts as the share's
  anonymous principal, and the call is counted
  (`identity_unmapped_total{scheme="uid"}`).
- **A credential gid with no principal** is no group at all: it is dropped
  from the caller's groups, and a create falls back to the principal's primary
  group, as [RFC 7 §2.4](rfc-7-namespace-metadata.md#2.4%20Attributes%2C%20and%20who%20writes%20them)
  states.
- **A principal shown over NFS** is its uid or gid. A principal with no
  spelling in that scheme — a well-known principal as a file's owner, a
  deleted principal — is shown as the anonymous uid or gid, 65534.
- **Directory principals.** When a principal is minted from the directory,
  its uid and gid come from the directory in the mode the directory record
  names:

  | Mode | uid and gid |
  | --- | --- |
  | `rfc2307` | the entry's `uidNumber` and `gidNumber`; an entry with none gets one allocated (§2.3) |
  | `rid` | the range's base plus the SID's RID, so every installation using the same base computes the same number |
  | `allocate` | allocated from `identity.id_range` (§2.3) |

  Whatever the mode, the number is written once, with the principal, and never
  recomputed: a later change of mode or of the directory's attribute changes
  no existing principal.

> decision: an unmapped `AUTH_SYS` uid acts as anonymous rather than as a
> principal minted for that number. Any client may send any of 2^32 uids, so
> minting on sight would let one host fill the store with principals, and each
> would own files no operator can name. The cost: a client user with no record
> in the store creates files owned by anonymous. Add a share option that mints
> numeric principals, with a cap, when a deployment shows NFS users the store
> cannot hold records for.

### 4.4 SIDs

- **Every user and group has a SID**, written with its record. A local
  principal's SID is the installation's domain SID followed by a RID drawn
  from the RID counter (§2.3). A directory principal's SID is the directory's.
- **The installation's domain SID** is `S-1-5-21-a-b-c`, where `a`, `b` and
  `c` are the first twelve bytes of the installation ID
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities))
  read as three little-endian 32-bit numbers. It is derived, never stored
  separately, and never changes. A principal carried into another
  installation by an export keeps the SID it has
  ([RFC 26 §3.2](rfc-26-catalog-backups.md#3.2%20Import%20is%20staged%20and%20published%20atomically));
  only new principals take the importing installation's domain SID.
- **A RID is never derived from a uid.** A uid can change (§2.3); a SID cannot.
- **Well-known SIDs** map to the well-known principals of
  [RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode),
  and anonymous to `S-1-5-7`.
- **A SID with no principal behind it** — a client's security descriptor
  naming a user of a domain the installation does not know — is
  `ErrUnknownPrincipal` from `Find`. What a set of such a descriptor does is
  [RFC 19](rfc-19-authorization.md)'s. A SID of the directory's domain is
  resolved through the directory first, and minted as a directory principal if
  it names a user or group there (§5).
- **A principal shown over SMB with no SID** — only a deleted one — is shown
  as the null SID `S-1-0-0`, which Windows displays as such.

> decision: a deleted principal is shown as the null SID, not as the SID it
> had, because deleting a user drops its index rows
> ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)) and
> its SID with them. The cost is that two deleted owners look alike in the
> security tab. Keep deleted principals' SIDs as tombstone rows when an
> operator needs to tell them apart.

### 4.5 NFSv4 owner strings

NFSv4 names owners and groups as strings. The adapter encodes them; this RFC
decides the mapping
([RFC 20 §2.4](rfc-20-adapters.md#2.4%20Protocol%20details%20that%20stay%20in%20the%20adapter)).

- **Encoding.** On a call whose flavour is Kerberos, a principal with a name
  is `name@domain`, where `domain` is `identity.nfs4_domain` (default: the
  first local realm in lower case, or `localdomain` with none). On `AUTH_SYS`
  and `AUTH_NONE`, it is the decimal uid or gid, as RFC 8881 §5.9 allows. A
  principal with no spelling is `nobody@domain`, or `65534`.
- **Decoding.** A string `name@domain` whose domain equals
  `identity.nfs4_domain`, compared case-insensitively, maps by name (§4.6). A
  decimal string maps by uid or gid, on any flavour. Any other string,
  including a name in another domain, **MUST** be refused `NFS4ERR_BADOWNER`.
- The special names an ACL entry carries — `OWNER@`, `GROUP@`, `EVERYONE@` —
  are not owner strings; they are [RFC 19](rfc-19-authorization.md)'s.

> decision: numeric owner strings on `AUTH_SYS`. A client using `AUTH_SYS`
> already names users by number, and its idmapper may know none of the
> server's names; numbers keep `ls -l` showing what the client sent. Names on
> Kerberos, where the client and server share a realm and its names. Revisit
> if a client is shown to need names on `AUTH_SYS`.

### 4.6 Names

- User and group names are compared case-insensitively, under simple Unicode
  case folding, as Windows and Active Directory compare them, and **MUST** be
  unique under that comparison within their kind.
- A user and a group may share a name
  ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer));
  a name lookup always says which kind it wants.
- Over SMB, `lsarpc` name lookups and answers
  ([RFC 22 §5.2](rfc-22-smb.md#5.2%20IPC%24%20and%20the%20RPC%20services))
  spell a local principal as `<NetBIOS name>\<name>` and a directory one as
  `<directory domain>\<name>`. A SID that maps to no principal is answered as
  unknown, with the raw SID, as Windows servers answer it.

## 5. Directory integration

### 5.1 What a directory provides

A directory is one external source of users and groups — Active Directory, or
another LDAP server with RFC 2307 attributes — named by the installation's
directory record (`identity.directory`: its address, base, bind credential
reference, realm, NetBIOS domain and §4.3's mode). There is at most one.

A directory answers four questions, each by one bounded query:

| Question | Asked when |
| --- | --- |
| who is this name, or this SID? | §4.2 step 3 and §4.4 find no record |
| what are this principal's groups, nested? | resolving a directory principal's groups (§2.5) |
| is this account enabled? | with every lookup of a directory user |
| what is this group's SID and gid? | minting a group a member's lookup names |

- A directory principal's **record** is minted once, from the first answer,
  and holds its `PrincipalID`, name, SID, uid and gid from then on (§4.3).
  Later answers update its name and enabled state; they never change its
  `PrincipalID`, SID, uid or gid.
- A directory principal's **groups and enabled state** are not written to the
  store. They are cached (§5.2), so a change in the directory takes effect at
  the next lookup.
- The bind credential is a `Secret` sealed under the `protocol` key
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)). The connection
  **MUST** use TLS; a directory record that asks for plain LDAP is refused at
  validation.

> ponytail: one directory, read directly over LDAP. No forest trusts, no
> global catalogue, no second directory. Add them when a deployment has users
> in two forests.

### 5.2 The directory cache

![Directory lookup: a lookup first checks the node's cache; an entry younger than the TTL is used; an older one triggers a directory query; when the directory answers, the entry is refreshed, or a not-found answer is cached briefly; when it fails or times out, an entry younger than the stale grace is used and counted, and anything else is refused](img/rfc18-directory.svg)

Each `protocol` node keeps one cache of directory answers, in memory.

- A positive answer is fresh for `identity.directory_ttl` (default
  5 minutes). A "no such entry" answer is fresh for
  `identity.directory_negative_ttl` (default 30 seconds), so a user created in
  the directory a moment ago can sign in soon after.
- An error or timeout (`identity.directory_timeout`, default 5 seconds)
  **MUST NOT** be cached as "no such entry", and **MUST NOT** replace a
  positive entry.
- Concurrent lookups of one key **MUST** share one query, so a sign-in storm
  sends the directory one query per user, not one per connection.
- The cache's key is the question: (directory, scheme, spelling) for "who is
  this", the principal for "what are its groups".

### 5.3 What a stale entry may grant

When the directory fails or times out:

- an entry whose last successful answer is no older than the TTL plus
  `identity.directory_stale_grace` (default 15 minutes) **MAY** be used, and
  each use is counted (`identity_directory_stale_total`);
- anything else **MUST** be refused: a lookup with no entry, or with one older
  than that, fails with `ErrUnavailable`, and a sign-in that needed it fails.

So a stale entry grants at most what the principal had at its last successful
lookup — its groups, its enabled state — for at most the TTL plus the grace
after that lookup. A directory account disabled during an
outage keeps that access until the grace runs out; a local `Disabled` flag,
which is the store's, takes effect within §7.2's bound regardless.

> decision: serve stale for a bounded grace, then fail closed. Failing closed
> at once turns every directory blip into a sign-in storm of refusals for
> users the server knew a minute ago; serving stale without bound lets a fired
> user in for as long as the directory is down. The grace is the window an
> operator accepts. Set it to zero where a disabled account must stop within the TTL,
> and lengthen it where the directory is known to be remote and flaky.

### 5.4 Errors never become answers

- A store or directory error **MUST NOT** be read as "no such principal".
  "No such principal" leads to anonymous for an `AUTH_SYS` uid (§4.3) and to
  the everyone grant at a share; an outage read that way grants access.
- An error **MUST NOT** fall back to the share's everyone grant, to a guest,
  or to a cached negative answer.
- Every such refusal is counted with its cause (§10.2).

## 6. Resolution per share

### 6.1 From Identity to caller

Resolution is admission's step 4
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)). It
runs in the filesystem service on every call, after the share's state,
flavour and client rules have admitted the call, and before `Authorize`:

1. **Map** the `Identity` to a principal: by the proof's principal if the
   authenticator set it, else by the Kerberos or NTLM name (§4.2), else by the
   `AUTH_SYS` uid (§4.3); `AUTH_NONE` and anonymous sessions map to anonymous.
   Refuse a disabled or deleted principal (§3.7).
2. **Squash** it, if the share's policy says so (§6.2).
3. **Collect** its groups (§6.3).
4. **Resolve** its share access from the grants (§6.4).
5. **Set** `authenticated`, `admin` and the privileged flag (§6.5).

The result is the caller. The service passes it to the metadata store as the
resolved identity [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)
requires:

```go
// Caller is what one call acts as on one share. Every field any check reads
// is here (RFC 7 §7.4), and Key covers all of them (§7.1).
type Caller struct {
	Principal     metadata.Principal   // after squashing
	Groups        []metadata.Principal // the closure, sorted; anonymous has its policy's group only
	Access        metadata.ShareAccess // §6.4
	Authenticated bool                 // §6.5
	Admin         bool                 // §6.5
	Privileged    bool                 // §6.5; for SMB set per handle (RFC 7 §7.4)
	Squashed      bool
	Flavor        Flavor
	Level         KerberosLevel
	Key           CallerKey            // §7.1
}
```

A compound or path that crosses into another share is resolved again there,
since squashing and grants are per share.

### 6.2 Squashing

A share's `ExportPolicy.Squash` is `none`, `root` or `all`
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
Squashing replaces the mapped principal with the policy's `Anonymous`
principal:

| Squash | Replaced |
| --- | --- |
| `none` | nothing |
| `root` | a caller whose mapped principal's uid is 0: an `AUTH_SYS` uid 0, or a Kerberos or SMB principal whose uid is 0 |
| `all` | every caller |

- `AUTH_NONE` callers and anonymous sessions are anonymous whatever the
  setting.
- **Squashing replaces the whole principal.** A squashed caller **MUST**
  carry no group, SID or name of the principal it replaced: its groups are the
  `Anonymous` principal's own (§6.3), its flags all false. A squash that
  replaced the uid and kept the SIDs would leave every SID-keyed ACL entry
  matching.
- **Anonymous is a principal, not a number.** `Anonymous` defaults to the
  well-known anonymous principal (uid and gid 65534). An operator who wants
  the effect of `anonuid=1000,anongid=1000` names the user whose uid is 1000.
  A deleted `Anonymous` principal fails admission closed: calls the policy
  would squash are refused.
- Squashing applies to every protocol alike. `all` over SMB makes every
  session act as anonymous on that share.
- The default for a new share is `root`.

> decision: squashing applies to SMB too, so a share's policy means one thing
> whatever the protocol. Over SMB `root` rarely matters, since only a
> principal whose uid is 0 meets it, and a share whose files must be
> anonymous to everyone — a public drop box — is `all` for both protocols.
> Revisit if an operator needs NFS squashed and SMB not on one share; that is
> two shares today.

### 6.3 Groups

The caller's groups are, in order of source:

| Flavour | Groups |
| --- | --- |
| `AUTH_SYS` | the credential's gid and supplementary gids, each mapped (§4.3); with `ManageGIDs`, the mapped principal's closure instead (§3.2) |
| Kerberos, NTLM | the closure of the principal's store memberships (§2.5), unioned with the groups of the proof's verified SIDs and, for a directory principal, the directory's groups |
| squashed, `AUTH_NONE`, anonymous | the `Anonymous` principal's closure |

- The closure is computed whole or the call fails (§2.5).
- `AUTH_SYS` groups are the client's assertion, like its uid. A share that
  must not trust them sets `ManageGIDs`, or requires Kerberos.

### 6.4 Share access from the grants

A caller's access to a share is read from the share's grants
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
in this order, stopping at the first that matches:

1. a grant naming the caller's own principal — whatever it says, `none`
   included, it is authoritative;
2. the widest grant naming any of the caller's groups;
3. a grant naming **authenticated users**, for an `authenticated` caller;
4. a grant naming **everyone**;
5. otherwise `none`.

- A share's "default access" for callers no other grant names is a grant to
  everyone, or to authenticated users. There is no other default.
- A squashed caller is the `Anonymous` principal; its own grant, if any, then
  everyone's, decide it.
- The access is evaluated on every call with the grant versions in the
  caller's key ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved),
  §7.1), so a changed grant stops a client that still holds handles.
- What `read`, `read-write` and `none` then allow on a file is RFC 7's and
  [RFC 19](rfc-19-authorization.md)'s.

> decision: a principal's own grant overrides its groups', including a `none`.
> That is how an operator blocks one user of a group without leaving the
> group, and it is the only way to express it with four access levels. The
> cost is that a user granted `read` alone misses a group's `read-write`.
> Revisit if operators are shown to expect group grants to widen a user's.

### 6.5 What the flavour proved: authenticated, admin, privileged

[RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)
defines what `authenticated`, `admin` and the privileged flag do. This RFC
decides which callers get them:

| Caller | `authenticated` | `admin` when its grant says `admin` | privileged |
| --- | --- | --- | --- |
| `AUTH_NONE`, anonymous SMB session | no | no | never |
| `AUTH_SYS` | no | no | uid 0, unsquashed |
| Kerberos over RPCSEC_GSS, any level | yes | yes | a principal whose uid is 0, unsquashed; or an `admin` |
| Kerberos or NTLMv2 SMB session | yes | yes | an `admin`, only on a handle opened with backup or restore intent |
| a machine principal | yes | yes | as its flavour's row |
| any squashed caller | no | no | never |

- `admin` **MUST** be set only from the grant, and only on an authenticated
  flavour. A uid, a group name, a SID such as `S-1-5-32-544` in a PAC, or a
  management-API role **MUST NOT** set it.
- An NTLM pass-through session is authenticated exactly as a Kerberos one:
  the domain controller verified it.

## 7. Caches

### 7.1 Every key carries the whole proof

An implementation **MAY** cache a mapped principal, a group closure, a
resolved caller or an admission result. Every key **MUST** carry everything
the value was derived from:

| Cache | Its key **MUST** include |
| --- | --- |
| proof → principal | the flavour, the proof's whole `Name`, the proof's principal if set, every SID in the proof, and for `AUTH_SYS` the uid, gid and every supplementary gid |
| principal → groups | the principal, and for `AUTH_SYS` the credential's gids and the share's `ManageGIDs` |
| caller, per share | all of the above, the share, `ExportPolicy.Version`, the grant versions read, the flavour and Kerberos level, and the principals generation (§7.2) |
| directory answers | the question (§5.2) |

- **Two proofs that share every numeric ID never share an entry.** Two
  Kerberos principals with the same uid, two NTLM users with the same uid, a
  Kerberos user and an `AUTH_SYS` caller sending its uid: each is a different
  key, because the key holds the name and the flavour, not just the numbers.
- Adding a field to `Identity`, `Proof` or `Caller` is an edit to every key
  derived from one. A key that cannot be shown to include the whole value
  **MUST** be replaced by one that does
  ([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)).
- Keys are compared whole. A digest **MAY** stand in for a key only if it is a
  collision-resistant hash of the whole key.
- No cache entry outlives the proof it was built from: an entry built from a
  session or GSS context is not served to a request on another.

### 7.2 Local changes take effect within a second

A change to a `User`, `Group` or `Membership` record, or to any protocol-ID
row, raises the installation's **principals generation**, a counter written in
the same transaction. Every cached mapping and caller carries the generation
it was built under.

- A node **MUST** read the principals generation at least once a second, and
  stop serving entries built under an older one.
- Grant and export-policy changes carry their own versions, already in the
  caller's key, and take effect at the next call.

> ponytail: one generation for all principals, polled once a second. A change
> to one user invalidates every node's whole cache, and an installation with
> constant churn rebuilds constantly. Key entries on per-principal versions,
> pushed rather than polled, when a profile shows resolution misses after
> generation changes at the reference box's call rate.

## 8. Management-API users

The management API has users of its own: operators who create shares, users
and grants. Their sign-in — passwords, tokens, roles, sessions — is planned
RFC 23's. This RFC fixes only where the two meet:

- **A role grants nothing on files.** An API role (administrator, operator)
  **MUST NOT** make a principal `admin`, privileged or granted on any share.
  File access comes only from share grants (§6.4) and flavour (§6.5).
- **An API user is not uid 0.** The API's bootstrap administrator **MUST NOT**
  be given uid 0, a share grant, or any spelling a file client could present,
  unless an operator gives it one.
- An API user and a file principal **MAY** be one `User` record, sharing a
  name and password; the API reads its role, and file access reads its grants.
  A password change updates the NT hash with it while NTLM is on (§3.5).

> decision: the two are kept apart by rule, not by separate records. One
> record per person is what operators expect, and a separate store would
> double every lookup. What must never happen is the API's administrator
> reading every file because it was made root for convenience. Revisit if
> planned RFC 23 needs API users that are not principals at all, such as
> service accounts with tokens only.

## 9. Failure modes

| Failure | Behaviour | Why |
| --- | --- | --- |
| store unreachable during sign-in | `ErrUnavailable`; the sign-in fails | §5.4 |
| store error during resolution | the call fails with `ErrUnavailable`, answered as the protocol's retryable error | §5.4 |
| directory down, entry within grace | served stale, counted | §5.3 |
| directory down, no fresh-enough entry | refused, counted | §5.3 |
| keytab missing or unreadable | Kerberos sign-ins fail `ErrUnavailable`; NTLM and `AUTH_SYS` unaffected | §3.3 |
| PAC signature does not verify | the sign-in fails `ErrLogonFailure` | §3.4 |
| ticket expired mid-session | the protocol's expired-credential answer; the client renews | §3.1 |
| group closure incomplete or over the cap | the resolution fails; never truncated | §2.5 |
| unmapped `AUTH_SYS` uid | anonymous, counted | §4.3 |
| unknown Kerberos user | `ErrLogonFailure` | §3.3 |
| wrong password, unknown NTLM user | `ErrLogonFailure`, never guest | §3.6 |
| `Anonymous` principal deleted | calls that would be squashed are refused | §6.2 |
| owner string or SID with no principal | `NFS4ERR_BADOWNER`; over SMB RFC 19 decides | §4.4, §4.5 |

## 10. Invariants and observability

### 10.1 Invariants

| # | Invariant |
| --- | --- |
| ID1 | Every comparison of identity compares `PrincipalID`s, never a spelling. |
| ID2 | No ID given to a principal is given to another: `PrincipalID`, allocated uid, gid and RID alike. |
| ID3 | No uid or gid is ever a default; every user has a uid, every group a gid, every user and group a SID. |
| ID4 | A SID never changes once given. |
| ID5 | Every cache of identity is keyed by everything its value was derived from, the proof's name and flavour included. |
| ID6 | A store or directory error is never read as "no such principal", and never yields a guest, anonymous or default grant. |
| ID7 | A failed sign-in never yields a session. |
| ID8 | `AUTH_SYS` and `AUTH_NONE` callers are never `authenticated` and never `admin`. |
| ID9 | `admin` is set only from a share grant, on an authenticated flavour. |
| ID10 | A squashed caller carries nothing of the principal it replaced. |
| ID11 | A group closure is used whole or not at all. |
| ID12 | A directory answer is used no longer than the TTL plus the grace after it was given. |
| ID13 | A disabled or deleted local principal is refused on every call within one second of the change. |
| ID14 | Adapters map, squash and grant nothing; every spelling they show or read goes through `Identities`. |

### 10.2 Metrics

| Answers | Metric | Type |
| --- | --- | --- |
| How many sign-ins, by flavour and outcome? | `identity_authentications_total{protocol, flavor, outcome}` | counter |
| How long does a sign-in take? | `identity_authentication_seconds{flavor}` | histogram |
| How often is a spelling unmapped? | `identity_unmapped_total{scheme}` | counter |
| How often is a call refused for an undecidable identity? | `identity_unavailable_total{cause}` | counter |
| How often are stale directory entries used? | `identity_directory_stale_total` | counter |
| How often is a sign-in refused for the directory? | `identity_directory_refusals_total` | counter |
| Directory queries and their latency | `identity_directory_queries_total{question, outcome}`, `identity_directory_seconds` | counter, histogram |
| Resolution cache effectiveness | `identity_cache_hits_total{cache}`, `identity_cache_misses_total{cache}` | counters |
| How many principals were refused over the group cap? | `identity_group_cap_refusals_total` | counter |
| How old is the principals generation each node serves? | `identity_generation_lag_seconds` | gauge |

`identity_unavailable_total` and `identity_directory_refusals_total` rising
together is a directory outage; `identity_unmapped_total{scheme="uid"}` rising
alone is a client sending uids the store does not know.

## 11. Conformance

Each check below fails silently when broken. All run on the composed
single-node harness ([index](rfc-index.md)), against the real metadata store,
with the directory and KDC as local test services.

| Rule | Check | Catches |
| --- | --- | --- |
| §2.3, §7.1 one uid, two proofs | Create two directory users with no uid in the directory, alice in group `Finance`, bob not. Assert their allocated uids differ, and that an operator write giving bob alice's uid fails. On a share admitting `sec=krb5` and `AUTH_SYS`, alice reads over Kerberos a file whose ACL grants `Finance` only: assert granted. From another host, over `AUTH_SYS`, send alice's uid with no supplementary gids and read it again: assert refused, the caller not `authenticated`, and the resolution cache missed. | a defaulted uid; a cache keyed on numbers, which serves the `AUTH_SYS` call alice's groups |
| §2.3 no reuse | Allocate a uid, delete its user, create another: assert the new uid differs. Do the same for a RID. | an allocator that reuses freed IDs |
| §5.3 stale grace | Resolve alice and bob; stop the directory. Within the grace assert both sign in, counted stale; sign in carol, never resolved: assert refused, and refused again after the grace for alice. Set the grace to zero: assert alice refused as soon as her entry passes the TTL. | fail-open on outage, or no grace at all |
| §5.4 error is not "unknown" | Inject a store error on the `uid` index during an `AUTH_SYS` call to a share with an everyone grant of `read-write`. Assert the call fails with a retryable error and creates nothing, rather than succeeding as anonymous. | an error read as "not found" |
| §3.6 no map to guest | Over SMB send a wrong password, an unknown user and, with the store failing, a known user. Assert `STATUS_LOGON_FAILURE` each time and no session. Then an anonymous session: assert it reaches `IPC$` and `TREE_CONNECT` to `profiles` is refused. | a failed login answered as guest |
| §6.2 squash drops SIDs | On a share with `Squash = all`, a Kerberos caller whose PAC carries `Finance` reads a file whose ACL grants `Finance` only: assert refused. On `Squash = none`: assert granted. | a squash that kept the SIDs |
| §6.5 admin needs proof | A share grant names uid 1500 `admin`. Over `AUTH_SYS` as uid 1500, read a mode-0600 file of another user: assert refused. Give the same user's principal a Kerberos name; over `sec=krb5`: assert granted. Put `S-1-5-32-544` in a test PAC for a non-admin: assert not `admin`. | `admin` derived from a uid or a SID |
| §8 roles grant nothing | The management API's administrator, with no grant, mounts each share over every flavour: assert every call refused. | an API role leaking into file access |
| §7.2 disable takes effect | bob holds an open over SMB and a GSS context over NFS; disable bob. Assert every call from both refused within 1 s of the commit. | a cached resolution outliving a disable |
| §2.5 group cap | A principal in `identity.max_groups` + 1 groups: assert refused, never resolved with fewer. A nested group whose read fails mid-closure: assert the call fails. | truncated group lists |
| §4.5 owner strings | Over `sec=krb5`, `chown` to `alice@corp.example` and to `alice@other.example`: assert success then `NFS4ERR_BADOWNER`. Over `AUTH_SYS`, `GETATTR`: assert numeric owners. A file owned by a deleted user: assert `nobody@…` and, over SMB, `S-1-0-0`. | names mapped by guesswork |
| §3.4 PAC | A ticket whose PAC server signature is corrupted: assert the sign-in fails. A ticket with no PAC for a user in a store group: assert the group applies. | an unverified PAC trusted |

## 12. Open questions

1. **Tombstones for deleted principals.** Whether a deleted principal keeps its
   SID and name as index rows that answer principal-to-wire but not
   wire-to-principal, so the security tab shows who owned a file (§4.4).
2. **Trusted domains.** Whether NTLM pass-through and SID resolution follow the
   directory's forest trusts, and how many directories an installation may
   name (§3.5, §5.1).
3. **Unmapped `AUTH_SYS` users.** Whether a share option should mint numeric
   principals on sight, with a cap, for NFS clients whose users the store
   cannot hold (§4.3).
4. **The principals generation in a cluster.** Whether polling one counter per
   second from every `protocol` node is cheap enough at the target scale, or
   needs a pushed change stream (§7.2).
5. **Name case.** Whether case-insensitive uniqueness of names (§4.6) holds for
   every directory a deployment uses, or a directory with case-distinct names
   needs an exception.

## Appendix A — prior art

*Non-normative.*

- **Samba's winbind** maps Windows SIDs to Unix IDs through idmap backends:
  `rid` (a range base plus the RID), `ad` (RFC 2307 attributes), `autorid`
  (ranges allocated per domain) and `tdb` (allocation, stored). §4.3's three
  modes are these, with the rule that a number once written is never
  recomputed. Its `map to guest = bad user` is the behaviour §3.6 refuses.
- **The Linux NFS server** trusts `AUTH_SYS` IDs as sent, applies
  `root_squash`, `all_squash`, `anonuid` and `anongid` per export, and with
  `rpc.mountd --manage-gids` replaces the credential's groups with the
  server's. `rpc.idmapd` and `nfsidmap` map `owner@domain` strings, refusing
  other domains, and since Linux 3.x numeric strings are used on `AUTH_SYS`
  by default.
- **SSSD** caches directory answers with an entry timeout and serves cached
  entries while the directory is unreachable (`offline_credentials_expiration`),
  the model for §5.3's grace.
- **Windows** builds an access token from the PAC at sign-in, so group changes
  apply at the next sign-in; it maps a failed sign-in to no session unless the
  Guest account is enabled, which current Windows releases disable by default.
- **MS-PAC** defines the server and KDC signatures; a file server verifies the
  server signature with its own key and may ask a domain controller to verify
  the KDC signature.
- **NetApp ONTAP** keeps name-mapping rules between Windows and Unix users and
  a default Unix user for unmapped Windows users, and shows unresolvable SIDs
  raw.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.3 | no default uid | the identity resolver defaults a user's missing uid and gid to 1000 (`pkg/adapter/identity.go`); the NFSv3 auth-context cache key had to add the SIDs because of it (`internal/adapter/nfs/v3/handlers/doc.go`) |
| §2.3, §4.4 | a RID from a counter, never from a uid | local SIDs are derived from the uid by the Samba formula, RID = 2·uid + 1000 (groups 2·gid + 1001), under a random machine SID stored as a setting (`pkg/auth/sid/mapper.go`); uid 0 maps to `S-1-5-32-544`; changing a uid changes the SID |
| §4.3 | directory uids written once | foreign SIDs get a durable, never-remapped Unix ID (`models.SIDMapping`); LDAP `rid` mode sets uid = gid = RID with no range base (`pkg/identity/ldap/provider.go`) |
| §2.1, §8 | API roles grant nothing on files | the bootstrap administrator is created with uid 0 so it bypasses file permission checks (`pkg/controlplane/models/admin.go`); roles themselves are read only by API middleware |
| §6.2 | squash none, root, all; anonymous is a principal | five modes — `none`, `root_to_admin`, `root_to_guest`, `all_to_admin`, `all_to_guest` — with anonymous uid and gid numbers per export, default 65534 (`pkg/controlplane/models/permission.go`, `share_adapter_config.go`); squashing runs in the NFS adapter and clears SIDs (`pkg/controlplane/runtime/identity/service.go`) |
| §4.3 | an unmapped uid is anonymous | an unknown uid is a guest granted the share's `default_permission`, or a direct SID grant matched on its uid (`internal/adapter/nfs/auth/share_permission.go`) |
| §6.4 | default access is an everyone grant | a per-share `default_permission` field; per-user grants, per-group grants and SID grants (`models.SIDSharePermission`) combined with the explicit user grant authoritative |
| §3.3 | unknown Kerberos user refused | over RPCSEC_GSS an unmapped principal acts as `nobody` (`internal/adapter/nfs/rpc/gss/framework.go`); over SMB it is refused |
| §2.4 | machine principals minted | no machine principals; service names have their prefix stripped when `StripRealm` is set (`pkg/auth/kerberos/identity.go`) |
| §3.5 | NTLMv2 against the domain as sent | five domain spellings are tried in turn (`internal/adapter/smb/handlers/session_setup.go`); the AUTHENTICATE MIC is checked but not enforced; pass-through via NETLOGON is tried only for users not in the store (`internal/auth/netlogon`) |
| §3.5 | no NT hash with NTLM off | NT hashes are stored hex-encoded on the user record, separate from the bcrypt password hash, whatever the NTLM setting (`pkg/controlplane/models/user.go`) |
| §3.6 | anonymous only, never on failure | a session is guest when no mechanism is recognised, an NTLM message does not parse, or no user store is configured; guest sessions are on by default (`session_setup.go`); an unknown user with a store is refused |
| §4.5 | names on Kerberos, numbers on `AUTH_SYS` | owner strings are always numeric in production, since the name mapper is set only in tests (`internal/adapter/nfs/v4/attrs/encode.go`); decoding accepts any domain for a numeric string, and `nobody`, `root`, `wheel` and `nogroup` by name |
| §4.6 | `lsarpc` name lookups | `LookupSids` answers; there is no `LookupNames`; unknown SIDs give `STATUS_SOME_NOT_MAPPED`, never `NONE_MAPPED` (`internal/adapter/smb/rpc/lsarpc.go`) |
| §5.2 | directory cache, stale grace | one resolver cache with 5 min positive, 30 s negative and 10 s error TTLs, merged concurrent lookups, no stale-serving (`pkg/identity/cache.go`); an LDAP error fails the lookup closed |
| §5.1 | TLS required | `allow_plaintext` permits plain LDAP (`pkg/identity/ldap/config.go`); the bind password is stored in plain text (RFC 13 Appendix A, C5) |
| §7.2 | local changes within a second | the NFSv3 auth-context cache lives 5 min and nothing invalidates it on a user change, so a disabled user keeps NFSv3 access until expiry (`share_permission.go` says so) |
| §3.2 | `ManageGIDs` | not implemented; supplementary gids are taken from the credential |
| §2.2 | names unique case-insensitively | user and group names are unique byte for byte |
