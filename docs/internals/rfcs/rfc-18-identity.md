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
  - "[[rfc-23-control-plane]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-26-catalog-backups]]"
aliases:
  - RFC 18
  - identity and authentication
tags:
  - rfc
---
# RFC 18 — identity and authentication

**Status:** draft. [§13](#13.%20Open%20questions) lists what is known to be undecided.
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
   needs a secret or a directory, the adapter passes the round to
   `vfs.Service`'s `SignIn`, and the server calls the authentication
   interface this RFC defines (§3). The adapter holds no identity interface.
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
2. Every cache of who the caller is keys on what was proved — the proof kind, the
   Kerberos name, the SIDs — never on a uid alone (§7.1). bob's call misses
   alice's entry.
3. The conformance suite signs alice in over Kerberos, sends her uid from
   another host over `AUTH_SYS`, and asserts that the second caller gets none
   of her groups (§11).

![From a credential to the caller: the adapter runs the handshake, calling the authentication interface, which verifies the credential and binds it to a principal, and hands the service an Identity holding only what was proved; the service re-reads that principal, or maps an AUTH_SYS uid through the store's protocol-ID index, admits it to the share, squashes it — or an unlinked machine — if the policy says so, resolves its share grant, and sets authenticated, admin and privileged; the chokepoint reads the caller](img/rfc18-resolution.svg)

**The words you need.**

- **principal** — a user, group, machine or well-known identity, named by a
  minted `PrincipalID` that is never reissued
  ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)).
- **spelling** — one protocol's name for a principal: a uid or gid, a SID, a
  Kerberos name, a user name, an `owner@domain` string. A principal has at most
  one spelling per scheme (§4).
- **flavour** — how a request's sender was identified: `AUTH_NONE`, `AUTH_SYS`,
  Kerberos (over RPCSEC_GSS or SMB), NTLMv2, or an anonymous SMB session.
  `AUTH_SYS` and `AUTH_NONE` prove nothing; the others prove a name (§3). A
  flavour never reaches the service: the adapter turns it into a proof kind and
  a protection.
- **proof kind and protection** — the two neutral, ordered parts of a request's
  `Identity` (§6.1): how the sender was proved — none, claimed, a shared
  secret, a ticket — and how its messages are protected — none, integrity,
  privacy.
- **proof** — what one handshake established: the name, the principal it was
  bound to, the verified group SIDs it carried; the session keys travel beside it, never in
  it. The adapter keeps it in the connection's security context and builds each request's `Identity` from it
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
- Management-API roles grant nothing on files. The API checks a password by
  calling identity directly (§8.1); its tokens and sessions are [RFC 23 §5](rfc-23-control-plane.md#5.%20Authentication%20and%20authorization)'s.

**How the rest is organised.** §1 gives the purpose and non-goals. §2 defines
principals and the rule that keeps their IDs stable. §3 is authentication:
the interface the service calls for an adapter's sign-in, and each flavour's rules. §4 is mapping in both
directions. §5 is the directory, its cache, what a stale entry may grant, and what works without a domain controller.
§6 is resolution per share: squashing, groups, grants and the caller's flags.
§7 is caches. §8 separates management-API users from file principals. §9
collects failure modes. §10 and §11 are invariants, metrics and conformance;
§12 is migration and §13 lists open questions. Appendix A is prior art, Appendix B where the
current code differs.

### Scenarios

**alice signs in on alice-pc and opens her profile container.**

1. alice-pc sends `SESSION_SETUP` with a Kerberos ticket for
   `cifs/files.corp.example`. The SMB adapter hands it to `vfs.Service`'s
   `SignIn`, and that node's server calls `AcceptKerberos` (§3.1), which decrypts the ticket with the key from the installation's keytab,
   checks its times and addresses, and verifies the ticket's PAC — the
   Windows group list the domain controller signed (§3.4).
2. The proof is `alice@CORP.EXAMPLE`, Kerberos, her SID and twelve group SIDs.
   Before returning it, `AcceptKerberos` maps the name by §4.2's order to
   alice's principal and binds the proof to it (§3.1).
   The adapter keeps it with the session's keys.
3. Her first `CREATE` on `profiles` carries an `Identity` built from that
   proof. The service re-reads alice's record — not disabled — and maps her
   group SIDs to their group principals.
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
3. A user the node has never resolved, or resolved too long ago, is refused
   with the protocol's logon failure — `STATUS_LOGON_FAILURE` over SMB, as
   every sign-in error is answered
   ([RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)) — and the refusal is
   counted (`identity_directory_lookups_total{result="refused"}`). Nobody is
   let in as a guest.

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

---

## 1. Purpose

Identity is decided in many places when nothing owns it: each adapter maps
names its own way, each cache keys on the fields its author thought of, and
each outage path picks its own fallback. Each copy is where a gap opens — a
cache key that misses a field, an error read as "not found", a protocol that
lets a disabled user through. This RFC owns identity once:

- the principal kinds and the rule that keeps their IDs stable (§2);
- the authentication interface the service calls for an adapter's sign-in, and each flavour's rules (§3);
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
- the order of admission — share state, proof and protection, client rules, squash — which
  is [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s;
  this RFC defines the squash step and what admission's minimum proof kind means by
  "authenticated";
- the wire form of a handshake — RPCSEC_GSS framing, SPNEGO, NTLM messages,
  signing and encryption — which [RFC 21](rfc-21-nfs.md) and
  [RFC 22](rfc-22-smb.md) own;
- the management API's tokens, sessions, roles and sign-in throttle, which are
  [RFC 23 §5](rfc-23-control-plane.md#5.%20Authentication%20and%20authorization)'s;
  this RFC only checks the password, an OpenID Connect ID token and the
  account (§8.1, §8.2);
- OpenID Connect on a file protocol: neither NFS nor SMB can carry a bearer
  token, so an identity provider reaches file access only as a directory or a
  Kerberos realm it backs (§5);
- which protocol flavour gives which proof kind and protection (§6.1), which
  [RFC 21 §5.7](rfc-21-nfs.md#5.7%20Translation%20to%20open%20state) and [RFC 22 §6.7](rfc-22-smb.md#6.7%20Translation%20to%20open%20state) own;
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
| **directory user** | minted on first resolution from the directory (§5), then a `User` record like any other | the directory's name; its SID from the directory or allocated (§4.4); uid and gid from the directory or allocated (§4.3) | Kerberos; NTLM pass-through; `AUTH_SYS` by uid |
| **group** | a `Group` record, local or minted from the directory | name, gid, SID | never |
| **machine** | minted on first Kerberos authentication of a host or service name (§2.4) | its Kerberos name | Kerberos only |
| **well-known** | fixed IDs, the same in every installation ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)) | the SIDs and NFSv4 names [RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode) lists | never, except **anonymous** (below) |

**Anonymous** is the well-known principal an `AUTH_NONE` caller, an anonymous
SMB session and a squashed caller act as, unless the share's policy names
another (§6.2). Its uid and gid are 65534, reserved to it (§2.3), its SID is
`S-1-5-7` (`ANONYMOUS LOGON`), and its NFSv4 ACL name is `ANONYMOUS@`
([RFC 19 §5.2](rfc-19-authorization.md#5.2%20Principals%20on%20the%20wire)). It is never `authenticated` and never `admin`.

**Administrator is not a kind.** A principal is an administrator of a share
when that share's grant names it `admin`
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
and that counts only for a proof of `Secret` or above (§6.5). No record, uid, group
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
| `uid` | a 32-bit number | users | every user but an API-only account (§8), always (§2.3) |
| `gid` | a 32-bit number | groups | every group, always (§2.3) |
| `sid` | a Windows SID, binary form | users and groups together | every user and group but an API-only account (§8), always (§4.4) |
| `krb` | `name@REALM`, as the KDC spells it | users and machines | users that sign in with Kerberos under a name §4.2's rule does not derive; every machine |
| user or group name | the record's `Name` | users; groups | every user and group |
| `oidc` | (issuer, subject): an ID token's `iss` and `sub`, as the issuer spells them | users | API accounts linked to an issuer (§8.2); no file protocol carries it |

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

**An e-mail address is not a spelling.** A user's record **MAY** hold one,
`User.Email`
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
It is never required and never defaulted.

- It names no principal to any check. Mapping, squashing, grants and ACL
  evaluation never read it, and it is in no cache key and not in the proof key
  (§7.1). It has no scheme, no protocol-ID row and no index, and nothing looks a
  principal up by it.
- It is set through the management API
  ([RFC 23 §3.5](rfc-23-control-plane.md#3.5%20Users%2C%20groups%2C%20grants%20and%20netgroups)),
  or, for a directory principal, taken from the directory entry when it has
  one — its `mail` attribute, for example — and written through
  `Minter.SetEmail` at the mint and at each later answer that changes it
  (§5.1), or from a verified `email` claim of an OpenID Connect issuer the
  account is linked to (§8.2). An address from a directory or an issuer is
  read-only to the API, as a directory principal's other fields are.
- It is stored as given, case preserved. The API refuses a value that is not a
  syntactically valid address. If a lookup is ever added, it compares the
  domain case-insensitively and the local part as stored.
- It is personal data. It **MUST NOT** appear in a log, a metric label, a span
  attribute or a logon event
  ([RFC 25 §5.4](rfc-25-observability.md#5.4%20What%20is%20never%20logged)). The
  API returns it only to `operator`, `admin` and the user itself
  ([RFC 23 §5.2](rfc-23-control-plane.md#5.2%20Roles)), and audits a change to
  it as any user field ([RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit)).

> ponytail: no index and no lookup by e-mail address. Add a domain-folded index
> when a feature must find a principal from an address, such as an API sign-in
> by e-mail; uniqueness then needs deciding, since a directory may give two
> entries one address.

### 2.3 IDs are never defaulted and never reused

**The stable-ID rule.** An ID given to a principal is never given to another:

1. `PrincipalID` is minted at random and never reissued
   ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)).
2. A uid, gid or RID (the last part of a SID, §4.4) that the installation
   allocates is drawn from a counter per scheme that only rises. An allocated
   ID **MUST NOT** be allocated again, even after its principal is deleted.
3. A uid or gid the operator assigns, or the directory supplies, **MAY** be
   any value no current principal holds. 65534 is anonymous's uid and gid
   (§2.1): it is never allocated, and is refused for any other principal. Assigning a deleted principal's uid
   to a new one is the operator's choice; it gives the new principal no file of
   the old one's, because files name principals, never uids.
4. A missing uid or gid **MUST NOT** be filled with a default. A user created
   without one, or resolved from a directory that has none, gets one allocated
   from the range `identity.id_range` (default 1 000 000 to 1 999 999 999) in
   the transaction that creates the record (§4.3). The one exception is an
   API-only account, which holds no spelling at all (§8).

> decision: a missing uid is allocated, never defaulted. A default makes every
> principal without one the same principal to every check that reads a uid —
> `AUTH_SYS` admission, an NFSv3 attribute, a cache key — and that is the bug
> the opening example walks through. The cost is that an operator who creates
> a user without a uid sees a number they did not choose; they may change it
> before the user owns anything. Overturned only by a client protocol that
> must derive a uid from another spelling and cannot be given an allocated one.

**Changing a spelling.** An operator **MAY** change a user's uid or a group's
gid. The change writes the record and its index rows in one transaction and
touches no file. Over NFS the principal's files then show the new number;
`AUTH_SYS` clients that still send the old one act as whoever now holds it, or
as anonymous (§4.3). A SID **MUST NOT** change once the principal has one:
it is what SMB clients cache and store in their own ACLs.

### 2.4 Machine principals

A machine principal is a host or service signing in for itself: the
`host/build01.corp.example@CORP.EXAMPLE` or `nfs/build01…` name an NFS client
uses for its lease and state operations, or an SMB client's machine account,
`BUILD01$@CORP.EXAMPLE`.

- A Kerberos name whose first component is a service name (`host`, `nfs`,
  `cifs`, or any name with a `/`), or whose user part ends in `$`, is a
  machine name.
- The first successful Kerberos authentication of a machine name with no
  record **MUST** mint a machine principal for it, with that `krb` spelling
  and no other. A Kerberos ticket is a KDC's proof, so only names the KDC
  issues can be minted.
- It is held as a `User` record whose principal has kind `Machine`
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)): no name, its `krb` spelling and no
  other, no primary group and no secret.
- A machine principal has two roles, and resolution keeps them apart. It is
  the principal NFSv4.1 state protection records
  ([RFC 21 §3.3](rfc-21-nfs.md#3.3%20Trunking%2C%20server%20owner%20and%20scope),
  RFC 14's client `Owner`, with the calls its `Protect` names), held in the caller's `Mapped` field (§6.1), so two
  machines are two principals. On files it acts as the share's `Anonymous`
  principal (§6.2), with anonymous's groups and every flag false: it is
  **not** `authenticated`, so authenticated-users grants and ACL entries do
  not reach it.
- A share grant **MUST NOT** name a machine principal; the management API
  refuses one. A machine that needs file access is linked to a user — the
  operator moves its `krb` spelling to that user (§4.2) — and then signs in as
  that user, by §3.1's binding at its next authentication.

> decision: machine principals are minted on sight, user principals are not
> (§3.3). A machine needs a principal of its own for state protection to mean
> anything, and minting one grants no file access, since it acts as anonymous
> and no grant can name it.
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
> group a deny entry names. 1 024 sits at the SID limit of a directory-issued
> access token (Appendix A), so a real account rarely meets it. Raise the default, or make
> the cap per share, when a deployment shows accounts above it.

### 2.6 Components, wiring and lifecycle

One component, the **identity server**, holds the authenticator (§3), `Identities` (§4), the directory client
and its cache (§5), resolution with its caches (§6, §7), and the management API's password check (§8.1).
`node.New` ([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)) builds it
once on every node, since every node runs both roles, from one `Deps`. It builds no other component and closes
nothing it was handed. It has two consumers, and the root hands each only the interfaces it calls:

| Interface | Consumer | Handed as |
| --- | --- | --- |
| `Resolver` (§6.1) | `*vfs.Server`, for admission | `vfs.Deps.Identity` ([RFC 17 §2.3](rfc-17-vfs.md#2.3%20Dependencies%20and%20wiring)) |
| `Authenticator` (§3.1) | `*vfs.Server`, behind `vfs.Service`'s `SignIn` | `vfs.Deps.Auth` |
| `Identities` (§4.1) | `*vfs.Server`, behind `vfs.Service`'s `Spell` and `Find` | `vfs.Deps.Names` |
| `APISignIn` (§8.1) | the management API | its `Deps` ([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)) |
| none | the adapters | — |

**Adapters get nothing from identity.** They reach sign-in and naming only through `vfs.Service`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)): `SignIn` passes one handshake round to the server on the node
holding the connection, which calls the `Authenticator` and is never forwarded; `Spell` and `Find` reach
`Identities`. Three narrow fields, not one handle, because each is called from a different place in the
server and faked alone in its tests. The gateway calls neither: it forwards the adapter's `Identity` as a value to the primary of the
file's shard, whose `*vfs.Server` resolves it.

**A forwarded `Identity` is trusted because of the channel, not the value.** The primary re-verifies nothing
the gateway node's sign-in proved; it accepts the `Identity` only because node channels are mutually
authenticated ([RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)), so only a node of the installation can
send one. A node's channel credential therefore asserts any principal, and is as sensitive as the keytab. Each
node resolves locally, with its own caches, which the principals generation invalidates (§7.2): the primary
re-reads the bound principal's record, so a disable reaches calls forwarded from every node.

> decision: identity is one component shared by `*vfs.Server` and the management API, not part of `vfs`.
> The API signs in over no file protocol, so a sign-in path inside `vfs` would have the API depend on the
> filesystem service; a handshake round is per connection, not a call on a file, and acts on no share; and
> identity has its own suite (§11), run without a filesystem. Overturned if the API stops calling identity —
> its accounts checked by a separate service — leaving `*vfs.Server` the only consumer; identity then folds into
> `internal/vfs`.

```go
package identity // internal/identity

type Deps struct {
	Principals metadata.Principals // RFC 16 §3: lookups by spelling, groups, grants
	Records    Records             // the record reads Principals lacks
	Mint       Minter              // the only writes (§2.4, §5.1)
	Secrets    SecretProvider      // RFC 13 §7: keytab, NT hashes, API password hashes, the bind credential, the machine-account password
	Logons     LogonSink           // RFC 25 §4.1's logon stream
	Issuers    IssuerKeys          // fetches each OpenID Connect issuer's signing keys over TLS (§8.2)
	Clock      clock.Clock
	Metrics    *metrics.Registry
	Logger     *slog.Logger
}

// Records reads one principal's record: the spellings Spell shows (§4.1) and
// the Disabled flag and deleted mark resolution checks (§3.7). RFC 16 §3's
// Principals answers a Principal only, so RFC 16 gains these two reads.
type Records interface {
	User(ctx context.Context, p metadata.Principal) (metadata.User, error)
	Group(ctx context.Context, p metadata.Principal) (metadata.Group, error)
}

// Minter never overwrites a record. Each write is one transaction, and fails
// with ErrExist when another principal holds a row it would write (§2.2).
// RFC 16 gains it as a view beside ControlPlane, whose PutUser overwrites whole.
type Minter interface {
	CreateUser(ctx context.Context, u metadata.User) error
	CreateGroup(ctx context.Context, g metadata.Group) error
	Rename(ctx context.Context, p metadata.Principal, name string) error    // the record's Name and its name index only (§5.1)
	SetEmail(ctx context.Context, p metadata.Principal, email string) error // the record's Email only, from the directory (§2.2)
}

// New takes no settings and starts nothing. *Server implements
// Authenticator, Identities, Resolver and APISignIn; only the composition root
// holds it.
func New(d Deps) (*Server, error)
func (s *Server) Apply(gen uint64, cfg config.Identity) error // from the node, first before Start: a principals generation, identity settings, the directory record (§7.2); refuses an invalid setting and keeps the last applied
func (s *Server) Start(ctx context.Context) error              // after the first Apply; loads the keytab's versions; the directory client dials lazily, so a down directory does not fail start
func (s *Server) Close() error                                 // drops every in-memory table
```

**Start and stop.** No settings reach `New`. The node reads the records at start
step 4 and hands identity its first settings and principals generation through
`Apply`, before `Start`. It starts it in build order, with open state, after
the metadata store, the engine and GC and before `*vfs.Server` and the management API
([RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)). It stops it after both, because a drain still
delivers calls that resolve, and the API checks an account on every call. A failed start
closes what it opened, as RFC 23 §2.2 says.

**Reconfiguration** arrives only through `Apply`, from the node's reconcile
([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders)):

| Change | Effect |
| --- | --- |
| a principal, membership or protocol-ID row | a new generation; entries built under an older one are not served (§7.2) |
| a keytab `Secret` version | the new key is added; older versions stay while a ticket can name them (§3.3); no restart |
| the directory record | a new client and an empty cache; lookups in flight finish on the old client |
| `identity.ntlm` turned off | NTLM sign-ins are refused, and calls on established NTLM contexts are refused at resolution; the NT hashes are deleted as §3.5 says |
| `identity.id_range` | applies to the next allocation; no existing ID moves |

> decision: turning NTLM off refuses established NTLM contexts as well, rather than letting them run to their
> end, because an operator turning it off is removing a weaker credential. Overturned if operators are shown to
> need a drain window.

**Restart.** Every table is in memory and is lost: proofs (clients sign in again), the replay cache (§3.3), the
logon throttle (§3.1) and the caches, which start cold.

### 2.7 Package layout

```text
pkg/identity/           the contract, a leaf importing metadata types only: Identity, ProofKind, Protection, Claims, Proof,
                        Keys, SignInStep, SignInResult, Caller, Spelling, Scheme, SID, NTLMAuthenticate, the
                        errors; Authenticator, Identities, Resolver, APISignIn
  identitytest/         Static (each interface, returning fixed values), a fake directory
internal/identity/      the server: one package, one file per concern
  server.go             New, Start, Apply, Close (§2.6)
  authn.go kerberos.go pac.go ntlm.go   the authenticator (§3.1–§3.5)
  throttle.go events.go                 the logon throttle and logon events (§3.1)
  resolve.go cache.go                   resolution and its caches (§6, §7)
  names.go                              Identities: Spell and Find (§4)
  api.go                                APISignIn (§8.1)
  directory/            the LDAP client and its cache (§5): a package, so only it imports LDAP
```

**The contract is `pkg/identity`; the server is `internal/identity`**, the split RFC 17 makes between `pkg/vfs`
and `internal/vfs`. Every interface and value type of this RFC is the contract's; `New`, `Deps`, `Records` and
`Minter` (§2.6) are the server's. `pkg/vfs` aliases `Identity`, `ProofKind`, `Protection`, `Claims`, `Proof`, `SignInStep`, `SignInResult`, `Spelling` and
`Scheme` ([RFC 17 §3.0](rfc-17-vfs.md#3.0%20Value%20types)), so an adapter imports `pkg/vfs` alone, as
[RFC 20 §2.3](rfc-20-adapters.md#2.3%20What%20an%20adapter%20may%20import) requires, and imports neither identity
package; `pkg/identity` imports no package of the service, so the two form no cycle. `internal/vfs` and the
management API import `pkg/identity`; only `internal/node` imports `internal/identity`. The NTLM and SPNEGO
wire code is [RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)'s `internal/adapter/auth`.

## 3. Authentication

### 3.1 The authentication interface

Adapters run handshakes; this interface does the parts that need a secret or
a directory. It is what [RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity) calls
"the authentication interface RFC 18 defines". Its one caller is `*vfs.Server`,
which calls it for `vfs.Service`'s `SignIn`, on the node holding the connection
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)); the adapter passes the
handshake round and receives the `Identity` and `Keys` back. Signatures are
indicative:

```go
package identity // pkg/identity

type Authenticator interface {
	VerifyNTLM(ctx context.Context, m NTLMAuthenticate, challenge [8]byte, client netip.Addr) (Proof, Keys, error)
	AcceptKerberos(ctx context.Context, apreq []byte, service Service, client netip.Addr) (Proof, Keys, error)
}

// Proof is what one handshake established, and nothing more. It holds no
// key, so it is safe to copy into every request's Identity.
type Proof struct {
	Name      string              // canonical: "alice@CORP.EXAMPLE", "CORP\\alice", a local user name
	Principal metadata.Principal  // what sign-in mapped Name to; every call acts as it (below)
	SIDs      []SID               // from a verified PAC or a pass-through reply: user SID first; else empty
	DenyOnly  []SID               // SE_GROUP_USE_FOR_DENY_ONLY: deny entries only (§3.4)
	Machine   bool                // Principal is a machine principal (§2.4)
	Expires   time.Time           // the ticket's end time; zero for NTLM
}

// SignInStep is one verification round as vfs.Service's SignIn passes it
// (RFC 17 §3.1): an AP-REQ, or a decoded NTLM AUTHENTICATE with the challenge
// the adapter sent. The server calls AcceptKerberos or VerifyNTLM with it.
type SignInStep struct {
	Kerberos  []byte            // an AP-REQ, for Service
	Service   Service
	NTLM      *NTLMAuthenticate // or an NTLM response to Challenge
	Challenge [8]byte
	Client    netip.Addr
}

// SignInResult is what SignIn hands back: an Identity with proof kind Secret
// (NTLM) or Ticket (Kerberos) and protection None (§6.1), which the adapter
// raises per request as its protocol's signing or sealing allows, and the Keys
// beside it, never in it.
type SignInResult struct {
	Identity Identity
	Keys     Keys
}

// Keys is what the adapter's mechanism needs for the AP-REP and for signing
// and encryption. It is never copied into an Identity.
type Keys struct{ Session, Subkey, APRep []byte }

type Service uint8 // NFS, CIFS, Host: the keytab service name a ticket is for
type Scheme uint8  // UID, GID, SID, Krb, UserName, GroupName, Owner (name@domain), Qualified (DOMAIN\name), OIDC (issuer and subject; §8.2)
// Spelling is one protocol's name for a principal: Num, SID or Text is set, by Scheme.
type Spelling struct{ Scheme Scheme; Num uint32; SID SID; Text string }
// NTLMAuthenticate is the decoded AUTHENTICATE message the adapter's NTLM mechanism hands over.
type NTLMAuthenticate struct{ User, Domain, Workstation string; NTResponse, EncryptedSessionKey, MIC []byte }
```

- Every error from the interface **MUST** be one of:
  `ErrLogonFailure`, for a wrong credential or an account that cannot sign in
  (the logon event's outcome says which, §10.2); `ErrInvalid`, for a malformed
  message; or `ErrUnavailable`, for an answer that could not be decided (a
  store, keytab or directory fault). The adapter answers each as the protocol's
  logon failure ([RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)), and none
  of them as success.
- An adapter **MUST NOT** turn any of these errors into a guest or anonymous
  session (§3.6).
- The authenticator reads only the `Principals` and `Records` views, the directory cache
  (§5.2) and the secret provider
  ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer),
  [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)), writes only the records
  a first sign-in mints (§2.4, §5.1), through `Minter`, which never overwrites a record,
  and **(cluster)** runs on the node that holds the connection.
- **A proof is bound to one principal for its life.** Before it returns a
  proof, the authenticator **MUST** map it to a principal and set
  `Principal`: a local NTLM user by the record it verified against, a
  Kerberos name by §4.2, an NTLM pass-through proof by its verified user SID
  (§4.4), never by its `DOMAIN\user` name. A proof that maps to no principal
  fails as §3.3 and §3.5 say. Every call on that security context then acts
  as that `PrincipalID`: resolution checks the principal's record on every
  call, through the cache keyed on the principals generation (§7.2), and
  refuses it if disabled or deleted (§3.7), and **MUST NOT** map the name
  again. A name later linked to, or re-used by, another principal reaches the
  new one only through a fresh authentication.
- A proof is valid until the connection's security context ends, or until
  `Expires`. A request on a context past `Expires` **MUST** be refused with
  the protocol's expired-credential answer, so the client renews its ticket.
- **Repeated logon failures are throttled here**, not in an adapter: the
  authenticator sees every attempt from both protocols with the client's
  address ([RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)). Past 10
  `ErrLogonFailure` results in 10 minutes for one (principal name, client
  address), every further attempt from that pair **MUST** fail
  `ErrLogonFailure` without being checked, until the window has passed with
  fewer. The answer is the ordinary logon failure, so it tells a guesser
  nothing, and no account is locked: a local account is never locked by
  failures, and a domain account's lockout is the domain's, which counts the
  pass-through failures itself.
- **Each attempt emits one logon event** to
  [RFC 25](rfc-25-observability.md#4.1%20Four%20streams)'s `logon` stream:
  success or failure with its error, the mechanism (Kerberos or NTLM), the principal name as
  presented and the principal mapped on success, and the client address. It
  **MUST NOT** carry the credential — no response, ticket, hash or session key.

> ponytail: throttling is in memory, per node. A restart forgets it, each
> node counts alone, and a guesser who spreads one name across many
> addresses, or many names across one, stays under it. Keep the counts in the
> metadata store, or add a per-address limit across names, when the logon
> stream shows a spray that this rule did not slow.

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
- An `AUTH_SYS` request reaches the service with proof kind `Claimed`, its ids as
  `Claims` (§6.1); `AUTH_NONE` with `None`. That mapping is
  [RFC 21 §5.7](rfc-21-nfs.md#5.7%20Translation%20to%20open%20state)'s.
- Which hosts may send claimed ids at all is the share's `MinProof` and client
  rules ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)):
  the only thing that makes an asserted uid worth anything is the network it
  came from.

`AUTH_NONE` carries nothing. Its caller acts as the share's anonymous principal
(§6.2), is never `authenticated` and never privileged. `AUTH_DH` and every
other flavour **MUST** be refused as `AUTH_BADCRED` (RFC 21 decides the
wire answer).

**Supplementary groups past 16.** `AUTH_SYS` cannot carry more than 16. A
share whose `ExportPolicy` sets `ManageGIDs` replaces the credential's gids
with the closure of the mapped principal's memberships (§2.5): the principal is found by uid, and the gids sent
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
  for either — for every host name a node answers on. A ticket for
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

> ponytail: the replay cache is in memory, per process. A restart forgets it, so an AP-REQ captured within
> `identity.clock_skew` before a restart can be accepted once after it, and **(cluster)** each node
> keeps its own. A replay yields no session key, so it can sign no SMB request and compute no RPCSEC_GSS
> verifier. Refuse authenticators stamped before the process start, or share the cache between nodes, when a
> replay across a restart or a fail-over is shown to reach anything.
- **The name.** The proof's `Name` is the ticket's client name and realm,
  exactly as the ticket spells them. Mapping it to a principal is §4.2's, done
  once, at sign-in (§3.1).
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

A Kerberos ticket from a directory that issues PACs (MS-PAC) carries one: the
user's SID, its
group SIDs and their attributes, signed by the domain controller.

- The PAC's server signature **MUST** verify under the service key before any
  SID in it is used. A PAC that does not verify **MUST** fail the
  authentication; a ticket without a PAC is accepted, with groups then read
  from the store and the directory (§2.5).

> decision: only the PAC's server signature is verified; the KDC signature is not sent to a domain controller.
> A forged PAC needs the service key, which the keytab's seal protects. Overturned if the keytab is ever readable
> without the `protocol` wrapping key, or a deployment requires KDC-signature checks.
- The proof's `SIDs` are the user SID and every group SID the PAC lists:
  its group memberships, extra SIDs and resource groups, excluding any whose
  attributes mark them disabled and any marked `SE_GROUP_USE_FOR_DENY_ONLY`.
- **A deny-only SID is kept, apart.** Every SID the PAC marks
  `SE_GROUP_USE_FOR_DENY_ONLY` **MUST** be carried in the proof's `DenyOnly`,
  and from there in the caller's (§6.1). It matches an ACL's deny entries and
  **MUST NOT** match an allow entry or a share grant
  ([RFC 19](rfc-19-authorization.md)), nor add the groups it is a member of to
  the caller's groups (§6.3). Dropping it would lose its denies;
  folding it into `SIDs` would let it grant.
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
| the directory's NetBIOS domain, with pass-through configured | **pass-through** | the response is sent to a domain controller over the secure channel of the installation's machine account (MS-NRPC `NetrLogonSamLogonEx`), whose password is a `Secret`; the reply carries the user's SID and group SIDs with their attributes, used as a PAC's are (§3.4); the proof maps to a principal by that user SID (§3.1, §4.4) |
| anything else | — | `ErrLogonFailure` |

- An NT hash **MUST** be stored only while `identity.ntlm` is on, and only for
  a user that holds file spellings: an API-only account (§8) keeps none,
  whatever its password. Turning NTLM
  off **MUST** delete every stored NT hash, since an NT hash is equivalent to
  the password for NTLM. Turning it on again stores a hash only at a user's
  next password set. The deletion runs as an operation in bounded batches
  ([RFC 23 §4.6](rfc-23-control-plane.md#4.6%20Long-running%20operations)). The
  authenticator refuses NTLM from the `Apply` that delivers the setting (§2.6),
  not from its commit. Until that `Apply`, a node may still verify against a
  hash the deletion has not reached: a window of at most one second, the node's
  settings-generation read
  ([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders)),
  the same bound a disable has (ID13).
- A local user with no NT hash **MUST** fail `ErrLogonFailure`; it never falls
  through to pass-through or to anonymous.
- The NTLMv2 session key is weaker than a Kerberos one. An NTLM session
  reaches the service with proof kind `Secret`, a Kerberos one with `Ticket`,
  and each with the protection its signing or sealing gives ([RFC 22 §6.7](rfc-22-smb.md#6.7%20Translation%20to%20open%20state)).
  A share whose `MinProof` is `Ticket` refuses NTLM and admits Kerberos
  ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees));
  `identity.ntlm` off refuses NTLM for the whole installation.

> decision: NTLM is on by default. Workgroup deployments with local users have
> no KDC, and SMB clients outside a domain fall back to NTLM.
> A deployment that has Kerberos everywhere turns it off, which also removes
> every NT hash from the store. Revisit the default once local users are shown
> to be rare.

> ponytail: pass-through reaches only the one directory's domain;
> §5.1's ponytail says when to follow trusts.

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
  **MUST** fail authentication with
  `ErrLogonFailure` (logon outcome `disabled`), and **MUST** be
  refused at resolution on every call, whatever proof its connection holds.
  The second rule is what stops a session or GSS context set up before the
  change; it takes effect within §7.2's bound.
- A disabled directory account is refused by the directory's own answer, at
  the next lookup (§5.3).
- A disabled user's open handles stay open; every call through them is
  refused.

## 4. Identity mapping

![The mapping triangle: a principal's minted ID at the centre, joined to its uid or gid, its SID and its names — user name, Kerberos name, owner@domain — the uid or gid and the SID stored with the record and indexed back, the user name indexed by name, and the Kerberos name and owner@domain derived from the name unless a krb row links one; from wire to principal through the index, from principal to wire through the record; a spelling with no principal behind it maps to anonymous for AUTH_SYS, refusal for a name, and an unknown SID for a SID](img/rfc18-mapping.svg)

### 4.1 Two directions, one mapping

Mapping runs in two directions, and both use the same records:

- **Wire to principal**, on every call and every name a client sends: a
  Kerberos name, a uid, a SID, an `owner@domain` string, a name in an `lsarpc`
  lookup. It reads the protocol-ID index.
- **Principal to wire**, whenever an adapter shows a stored principal: a
  file's owner and group in NFS attributes or an SMB security descriptor, an
  ACL entry's principal, a quota entry. It reads the principal's record.

`*vfs.Server` holds both as `Identities`, and `vfs.Service` exposes them to
adapters as `Spell` and `Find`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), so an adapter asks and never
decides:

```go
package identity // pkg/identity

// Identities maps principals to protocol spellings and back (§4). It acts on
// no share and is not admitted; vfs.Deps.Names holds it (§2.6).
type Identities interface {
	// Spell returns p's spelling in scheme, or ErrUnknownPrincipal.
	Spell(ctx context.Context, p metadata.Principal, scheme Scheme) (Spelling, error)
	// Find returns the principal holding s, or ErrUnknownPrincipal. It mints a
	// directory principal (§4.4) only for an authenticated id.
	Find(ctx context.Context, id Identity, s Spelling) (metadata.Principal, error)
}
```

- An adapter **MUST** encode and decode every principal through these calls,
  and **MUST NOT** derive one spelling from another — a SID from a uid, a uid
  from a RID, a name from a SID.
- A lookup error other than "not found" **MUST** reach the client as a server
  failure, never as "unknown principal": a store outage that made every owner
  `nobody` would be read by a client as data.

RFC 19's translation declares its own narrow `Spelling`
([RFC 19 §5.1](rfc-19-authorization.md#5.1%20One%20translation%2C%20two%20spellings)). The adapter builds one
over `Identities` for each request, binding its `ctx` and `Identity`, and passes `ErrUnknownPrincipal` and every
other error through unchanged.

### 4.2 Kerberos names

A Kerberos name maps to a principal in this order, stopping at the first that
answers:

1. the `krb` index row for the name exactly as the ticket spells it — an
   operator's explicit link, or a minted machine principal;
2. if the realm is one of `identity.local_realms` (default: the realms of the
   keytab's service names), the user whose name equals the name's user part,
   compared as user names are (§4.6), unless it is an API-only account (§8);
3. the directory, by the name's user part, if the realm is the directory's
   realm (§5);
4. otherwise none: a user name is refused (§3.3), a machine name minted
   (§2.4).

A realm in the name **MUST** be compared as the KDC spells it, case and all.
A name with a service part (`nfs/build01`) never matches step 2 or 3.

### 4.3 uid and gid

- A uid maps to the user holding it in the `uid` index; a gid to the group in
  the `gid` index. In `rfc2307` mode, a claimed uid or gid with no row is
  asked of the directory by `uidNumber` or `gidNumber`, and an entry that
  answers is minted as a directory principal (§5.1, §5.5).
- **An `AUTH_SYS` uid with no principal behind it**, after that lookup, acts as the share's
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
  | `rid` | the range's base plus the SID's RID, so every installation using the same base computes the same number; an entry with no SID gets one allocated (§2.3) |
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

- **Every user and group has a SID**, written with its record, but an
  API-only account (§8). A local
  principal's SID is the installation's domain SID followed by a RID drawn
  from the RID counter (§2.3). A directory principal's SID is the directory's
  when its entry has one; an entry with none —
  an RFC 2307 directory that holds no Windows attributes — is given a local
  SID, allocated so, when its principal is minted, and keeps it whatever the
  directory later says (ID4).
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
  [RFC 19 §2.8](rfc-19-authorization.md#2.8%20What%20a%20stored%20ACL%20may%20hold)'s:
  the whole set is refused, until a principal is minted for such a SID.
  A SID of the directory's domain is resolved through the directory first. `Find` mints it as a directory
  principal only for a caller whose `Identity` is authenticated (§6.5); for any other caller it answers
  `ErrUnknownPrincipal` and writes nothing.

> decision: a lookup by an unauthenticated caller never mints. `lsarpc` runs on `IPC$`, which an anonymous
> session reaches ([RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)), so minting there would let a caller that
> proved nothing write one record per directory account. Overturned if an anonymous client is shown to need a
> stable principal for a SID it only reads.

- **A principal shown over SMB with no SID** — only a deleted one — is shown
  as the null SID `S-1-0-0`.

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

- **Encoding.** On a call with proof kind `Secret` or above, a principal with a name
  is `name@domain`, where `domain` is `identity.nfs4_domain` (default: the
  first local realm in lower case, or `localdomain` with none). With `Claimed`
  or `None` (`AUTH_SYS`, `AUTH_NONE`), it is the decimal uid or gid, as RFC 8881 §5.9 allows. A
  principal with no spelling is `nobody@domain`, or `65534`.
- **Decoding.** A string `name@domain` whose domain equals
  `identity.nfs4_domain`, compared case-insensitively, maps by name (§4.6). A
  decimal string maps by uid or gid, whatever the proof. Any other string,
  including a name in another domain, **MUST** be refused `NFS4ERR_BADOWNER`.
- The special names an ACL entry carries — `OWNER@`, `GROUP@`, `EVERYONE@` —
  are not owner strings, nor is the `S-…@domain` form an ACL entry may spell;
  they are [RFC 19 §5.2](rfc-19-authorization.md#5.2%20Principals%20on%20the%20wire)'s.

> decision: numeric owner strings on `AUTH_SYS`. A client using `AUTH_SYS`
> already names users by number, and its idmapper may know none of the
> server's names; numbers keep `ls -l` showing what the client sent. Names on
> Kerberos, where the client and server share a realm and its names. Revisit
> if a client is shown to need names on `AUTH_SYS`.

### 4.6 Names

- User and group names are compared case-insensitively, under simple Unicode
  case folding, and **MUST** be
  unique under that comparison within their kind.
- A user and a group may share a name
  ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer));
  a name lookup always says which kind it wants.
- Over SMB, `lsarpc` name lookups and answers
  ([RFC 22 §5.2](rfc-22-smb.md#5.2%20IPC%24%20and%20the%20RPC%20services))
  spell a local principal as `<NetBIOS name>\<name>` and a directory one as
  `<directory domain>\<name>`. A SID that maps to no principal is answered as
  unknown, with the raw SID.

## 5. Directory integration

### 5.1 What a directory provides

A directory is one external source of users and groups — an LDAP directory
with Windows attributes (MS-ADTS), or one with RFC 2307 attributes — named by the installation's
directory record (`identity.directory`: its address, base, bind credential
reference, realm, NetBIOS domain and §4.3's mode). There is at most one.

A directory answers four questions, each by a bounded number of queries, every
one under `identity.directory_timeout`; a nested-group closure the directory
cannot answer in one query is walked level by level, and one that cannot be
finished fails whole (§2.5):

| Question | Asked when |
| --- | --- |
| who is this name, or this SID? | §4.2 step 3 and §4.4 find no record |
| what are this principal's groups, nested? | resolving a directory principal's groups (§2.5) |
| is this account enabled? | with every lookup of a directory user |
| what is this group's SID and gid? | minting a group a member's lookup names |
| who holds this `uidNumber` or `gidNumber`? | in `rfc2307` mode, a claimed uid or gid with no record (§4.3) |
| what is this user's e-mail address? | with every lookup of a directory user, when the entry has one — its `mail` attribute, for example; read-only to the API, like the entry's other fields (§2.2) |

- A directory principal's **record** is minted once, from the first answer,
  and holds its `PrincipalID`, name, SID, uid and gid from then on (§4.3).
  Later answers update its name through `Minter.Rename` (§2.6), and its
  e-mail address, when the entry gives one, through `Minter.SetEmail` (§2.2). A name that
  another principal holds is refused, and the record keeps its old name. Later answers never change its
  `PrincipalID`, SID, uid or gid. An entry the directory gives no SID or no
  uid gets one allocated as §4.3 and §4.4 say, so a directory without Windows
  or without Unix attributes still yields principals that satisfy ID3.
- A mint that loses the race for a spelling's protocol-ID row, which the store
  refuses to give a second holder (§2.2), reads the principal that won. It
  binds to that principal only if the winner's record holds every spelling the
  directory supplied to the losing mint: its name (§4.6), its SID, and its uid
  and gid in `rfc2307` mode. Otherwise two directory entries claim one spelling, and the
  mint fails `ErrUnavailable`. It binds nothing, writes nothing, and logs both
  `PrincipalID`s. It never retries with a fresh `PrincipalID`.
- A directory principal's **groups and enabled state** are not written to the
  store. They are cached (§5.2), so a change in the directory takes effect at
  the next lookup.
- The bind credential is a `Secret` sealed under the `protocol` key
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)). The connection
  **MUST** use TLS; a directory record that asks for plain LDAP is refused at
  validation.

> ponytail: one directory, read directly over LDAP: no forest trusts, no
> global catalogue, no second directory; NTLM pass-through and SID lookups
> reach only its domain (§3.5). Follow trusts, and route pass-through to the
> trusted domain's controller, when a deployment has users in a second domain
> or forest.

### 5.2 The directory cache

![Directory lookup: a lookup first checks the node's cache; an entry younger than the TTL is used; an older one triggers a directory query; when the directory answers, the entry is refreshed, or a not-found answer is cached briefly; when it fails or times out, an entry younger than the stale grace is used and counted, and anything else is refused](img/rfc18-directory.svg)

The identity server keeps one cache of directory answers, in memory;
**(cluster)** one per node.

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

> ponytail: **(cluster)** one directory cache per node, so N nodes ask N times. Share answers through the
> metadata store when the directory's query rate shows it.

### 5.3 What a stale entry may grant

When the directory fails or times out:

- an entry whose last successful answer is no older than the TTL plus
  `identity.directory_stale_grace` (default 15 minutes) **MAY** be used, and
  each use is counted (`identity_directory_lookups_total{result="stale"}`);
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

### 5.5 A directory without Active Directory or Kerberos

A directory is a source of names, never a proof: it says who holds a name, a
uid or a SID, and which groups they are in, and it verifies no credential. The
proof comes from the sign-in path (§3). With a plain RFC 2307 directory, no
domain controller and no KDC:

| Path | Works? | Why |
| --- | --- | --- |
| NFS `AUTH_SYS` | yes, as a claim | in `rfc2307` mode a claimed uid maps by `uidNumber` to the directory principal, and its groups come from the directory (§4.3, §2.5); the caller is `Claimed`, never `authenticated`, so no `admin` or authenticated-users grant reaches it (§3.2, §6.5) |
| NFS or SMB Kerberos | only with a KDC | any KDC serving the same users will do; a ticket's name resolves through the directory by §4.2 step 3 when its realm is the directory's |
| SMB NTLMv2 | no, for a user only in the directory | NTLMv2 is challenge and response, so no password reaches the server to check against the directory; a local check needs an NT hash, which the store holds for no directory user (§3.5); pass-through needs a domain controller |
| management-API sign-in | local users only | the API checks a stored password hash (§8.1); a directory user has none |

**So an SMB user who exists only in a directory without a domain controller
cannot sign in.** The supported path without one is the directory for
`AUTH_SYS`, and a KDC wherever a proof is needed. Giving such a user a second,
local SMB password is refused: two passwords for one principal drift apart.

> decision: a claimed uid in `rfc2307` mode mints the directory principal that
> holds it, although §4.4 never mints for an unauthenticated `Find`. A mint
> here is bounded by the directory's own entries, not by the 2^32 numbers a
> client may send (§4.3), and only hosts the share's client rules admit can
> trigger one. Without it no directory user is ever minted where nothing signs
> in with a proof. Overturned if `identity_directory_lookups_total` shows a
> client enumerating `uidNumber`s; mints then need a proof, and `AUTH_SYS`
> users a record the operator creates.

> ponytail: NT hashes are never read from the directory (an attribute such as
> a Samba-style schema's NT password). Reading one needs a privileged bind, an
> NT hash is equivalent to the password, and §3.5 lets NT hashes exist only
> while `identity.ntlm` is on. Add it when a deployment needs NTLM for
> directory users without a domain controller, with a sealed bind credential
> that may read only that attribute and the hash held only in memory, dropped
> when `identity.ntlm` turns off.

## 6. Resolution per share

### 6.1 From Identity to caller

Resolution is admission's step 4
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)). It
runs in `*vfs.Server`, at the primary, on every call, after the share's state,
`MinProof`, `MinProtection` and client rules have admitted the call, and before `Authorize`:

1. **Map** the `Identity` to a principal: with proof kind `Secret` or above,
   the principal sign-in bound it to (§3.1), never by its name again; with
   `Claimed`, by the claimed uid (§4.3); with `None`, to anonymous. Refuse
   a disabled or deleted principal (§3.7). The result is the caller's
   `Mapped`.
2. **Squash** it, if the share's policy says so (§6.2). An unlinked machine
   principal is replaced by the share's `Anonymous` here too, whatever the
   setting (§2.4).
3. **Collect** its groups (§6.3).
4. **Resolve** its share access from the grants (§6.4).
5. **Set** `authenticated`, `admin` and the privileged flag (§6.5).

```go
package identity // pkg/identity

// Identity is what the adapter proved for one request, in no protocol's terms;
// pkg/vfs aliases it (RFC 17 §3.0), so the adapter still imports one package.
// It names no flavour, mechanism or protocol: each adapter maps its own to a
// ProofKind and a Protection (RFC 21, RFC 22).
type Identity struct {
	Principal  metadata.Principal // the proven principal, Evidence.Principal; zero below Secret
	Proof      ProofKind
	Protection Protection
	Claims     Claims             // what the client asserted; read only at Claimed, never as proof
	Evidence   Proof              // what sign-in established (§3.1); zero below Secret
	Client     netip.Addr
}

// ProofKind is how the sender was proved, in increasing order. Secret and
// Ticket prove a principal. Prose drops the Proof and Protection prefixes.
type ProofKind uint8

const (
	ProofNone    ProofKind = iota // nothing: an anonymous or guest caller, no principal
	ProofClaimed                  // POSIX ids asserted, nothing verified
	ProofSecret                   // a shared-secret challenge and response, such as NTLMv2
	ProofTicket                   // a third party's mutual proof, such as Kerberos
)

// Protection is how the request's messages are protected, in increasing order.
type Protection uint8

const (
	ProtectionNone      Protection = iota
	ProtectionIntegrity            // every message signed
	ProtectionPrivacy              // every message sealed
)

// Claims are POSIX ids a client asserted. They name no principal until
// resolution maps them (§4.3), and they never make a caller authenticated.
type Claims struct {
	UID, GID uint32
	GIDs     []uint32
}

// Key is the proof key every cache of identity keys on (§7.1): a
// collision-resistant digest of every field but Client, which resolution does
// not read — both Proof and Protection included.
func (id Identity) Key() ProofKey

// Resolver is admission's step 4 (RFC 17 §4.9); *vfs.Server is its only caller.
type Resolver interface {
	Resolve(ctx context.Context, id Identity, share metadata.ShareID) (*Caller, error)
}
```

**The flavour sets the proof kind and protection; this RFC proves the
principal.** Verifying a Kerberos ticket, its PAC, or an NTLMv2 response is
this RFC's (§3.3–§3.5), and the `Identity` it returns has proof kind `Ticket`
or `Secret`. Which pair each protocol flavour and level then carries —
`AUTH_SYS` as `Claimed`, `krb5i` as `Ticket` with `Integrity`, a signed NTLM
session as `Secret` with `Integrity` — is [RFC 21 §5.7](rfc-21-nfs.md#5.7%20Translation%20to%20open%20state)'s and [RFC 22 §6.7](rfc-22-smb.md#6.7%20Translation%20to%20open%20state)'s. A share's
`ExportPolicy` sets a minimum on each part, `MinProof` and `MinProtection`
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)): Kerberos
only is `MinProof` `Ticket`, and a minimum Kerberos level is `MinProtection`.
Resolution reads `Proof`, `Principal`, `Claims` and `Evidence`; it never reads
`Protection`, which only admission compares.

The result is the caller. The service passes it to the metadata store as the
resolved identity [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)
requires:

```go
// Caller is what one call acts as on one share. Every field any check reads
// is here (RFC 7 §7.4).
type Caller struct {
	Principal     metadata.Principal   // after squashing: what Authorize and grants read
	Mapped        metadata.Principal   // step 1's, before squashing: what state protection and a client record's creator compare (RFC 14); Authorize never reads it
	Groups        []metadata.Principal // the closure, sorted; anonymous has its policy's group only
	DenyOnly      []metadata.Principal // deny entries only (§3.4); never a grant or an allow entry; empty when squashed
	Access        metadata.ShareAccess // §6.4
	Authenticated bool                 // §6.5
	Admin         bool                 // §6.5
	Privileged    bool                 // §6.5; for SMB set per handle (RFC 7 §7.4)
}
```

Resolution answers with RFC 17 §4.3's errors, mapped by `*vfs.Server`: `ErrAccess`
for a disabled or deleted principal, a deleted `Anonymous`, or a principal over
the group cap; `ErrUnavailable` for an undecidable lookup or an incomplete
closure. An expired proof never reaches it, because the adapter's `auth` layer
refuses a request on a context past `Expires`
([RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)).

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
| `root` | a caller whose mapped principal's uid is 0: a claimed uid 0, or a verified principal whose uid is 0 |
| `all` | every caller |

- `None` callers (`AUTH_NONE`, anonymous sessions) are anonymous whatever the
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

| Proof kind | Groups |
| --- | --- |
| `Claimed` (`AUTH_SYS`) | the claimed gid and supplementary gids, each mapped (§4.3); with `ManageGIDs`, the mapped principal's closure instead (§3.2) |
| `Secret` or `Ticket` (NTLM, Kerberos) | the closure of the principal's store memberships (§2.5), unioned with the groups of the proof's verified SIDs and, for a directory principal, the directory's groups; the proof's `DenyOnly` SIDs map to `DenyOnly`, not to groups (§3.4) |
| squashed, `None`, an unlinked machine | the `Anonymous` principal's closure |

- The closure is computed whole or the call fails (§2.5).
- Claimed groups are the client's assertion, like its uid. A share that
  must not trust them sets `ManageGIDs`, or requires Kerberos.

### 6.4 Share access from the grants

A caller's access to a share is read from the share's grants
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
in this order, stopping at the first that matches:

1. a grant naming the caller's own principal — whatever it says, `none`
   included, it is authoritative. A grant of `none` **MUST** be stored as a
   grant, distinct from no grant: setting `none` blocks, and only removing the
   grant, a separate operation, lets the caller fall through to its groups;
2. the widest grant naming any of the caller's groups;
3. a grant naming **authenticated users**, for an `authenticated` caller;
4. a grant naming **everyone**;
5. otherwise `none`.

- A share's "default access" for callers no other grant names is a grant to
  everyone, or to authenticated users. There is no other default.
- A squashed caller is the `Anonymous` principal; its own grant, if any, then
  everyone's, decide it.
- The access is evaluated on every call with the share's grant-list version
  in the caller's key (RFC 16 `S‖id‖gv`; [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved),
  §7.1), so a changed or deleted grant stops a client that still holds handles.
- What `read`, `read-write` and `none` then allow on a file is RFC 7's and
  [RFC 19](rfc-19-authorization.md)'s.

> decision: a principal's own grant overrides its groups', including a `none`.
> That is how an operator blocks one user of a group without leaving the
> group, and it is the only way to express it with four access levels. The
> cost is that a user granted `read` alone misses a group's `read-write`.
> Revisit if operators are shown to expect group grants to widen a user's.

> ponytail: step 2 reads one grant per group, up to `identity.max_groups` point reads
> on a cache miss. Read the share's grant list once, under its grant-list version, when a profile shows misses
> dominated by grant reads.

### 6.5 What the proof proved: authenticated, admin, privileged

[RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)
defines what `authenticated`, `admin` and the privileged flag do. This RFC
decides which callers get them:

| Caller | `authenticated` | `admin` when its grant says `admin` | privileged |
| --- | --- | --- | --- |
| `None` | no | no | never |
| `Claimed` | no | no | uid 0, unsquashed |
| `Secret` or `Ticket`, any protection | yes | yes | a principal whose uid is 0, unsquashed; or an `admin` |
| an unlinked machine principal | no | no | never: it acts as `Anonymous` (§2.4) |
| any squashed caller | no | no | never |

- Where a protocol confines privilege to a handle opened with backup or restore
  intent, as SMB does, that confinement is the open's
  ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)),
  not the `Identity`'s: the caller's flag is the same for every protocol.
- `admin` **MUST** be set only from the grant, and only for a proof of
  `Secret` or above. A uid, a group name, a SID such as `S-1-5-32-544` in a PAC, or a
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
| proof → principal | the `Identity`'s proof key, `Key()` (§6.1): its proof kind and protection, principal, whole evidence — `Name`, bound principal, every SID with whether it is deny-only — and every claim |
| principal → groups | the principal, and at `Claimed` the claimed gids and the share's `ManageGIDs` |
| caller, per share | the proof key, the share, `ExportPolicy.Version`, the share's grant-list version, and the principals generation (§7.2); the admission result's key is RFC 17 §4.9's, which carries every field of this row |
| directory answers | the question (§5.2) |

- **Two proofs that share every numeric ID never share an entry.** Two
  Kerberos principals with the same uid, two NTLM users with the same uid, a
  Kerberos user and an `AUTH_SYS` caller sending its uid: each is a different
  key, because the key holds the name and the proof kind, not just the numbers.
- Adding a field to `Identity` or `Proof` is an edit to `Key()`, and adding one
  to `Caller` an edit to every key derived from one. A field that is not
  identity — an account's e-mail address (§2.2) — is in no key. A key that cannot be shown to include the whole value
  **MUST** be replaced by one that does
  ([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)).
- Keys are compared whole. A digest **MAY** stand in for a key only if it is a
  collision-resistant hash of the whole key.
- No cache entry outlives the proof it was built from: an entry built from a
  session or GSS context is not served to a request on another.
- **No entry outlives the directory answers it read.** An entry built from a
  directory answer — a directory principal's groups or enabled state, and so
  any closure, caller or admission result that used them — carries the
  earliest freshness deadline among those answers, the TTL after each
  (§5.2), and **MUST NOT** be served past it. Past it the entry is rebuilt
  through the directory cache, which then answers fresh, stale within the
  grace, or refuses (§5.3). The principals generation cannot do this for
  them: directory groups and enabled state are not written to the store
  (§5.1), and an NTLM proof has no `Expires`.

### 7.2 Local changes take effect within a second

A change to a `User`, `Group` or `Membership` record, or to any protocol-ID
row, raises the installation's **principals generation**, a counter written in
the same transaction: the `PGEN` key, kind 0x3B
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). Every cached mapping and caller carries the generation
it was built under.

- The node reads the principals generation once a second
  ([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders))
  and hands each new value to the identity server (`Apply`, §2.6), which does
  not poll the store itself. An entry built under an older generation is not
  served after the `Apply` that delivers the new one.
- Grant and export-policy changes carry their own versions, already in the
  caller's key, and take effect at the next call.

> ponytail: one generation for all principals, polled once a second. A change
> to one user invalidates every node's whole cache, and an installation with
> constant churn rebuilds constantly. Key entries on per-principal versions,
> pushed rather than polled, when a profile shows resolution misses after
> generation changes at the reference box's call rate.

### 7.3 Bounds

Every table here is keyed on input a client controls, so each is charged to
[RFC 24](rfc-24-resources.md#5.1%20No%20queue%20is%20unbounded)'s connection-state pool, and past its share:

| Table | Past its share |
| --- | --- |
| caller cache | the least recently used entry is evicted, counted |
| directory cache | the least recently used entry is evicted; an evicted entry is a miss, never "not found" |
| replay cache | holds entries younger than `identity.clock_skew`; a sign-in that would need one more fails `ErrUnavailable` |
| logon throttle | the oldest pair below its limit is evicted; a pair at its limit is never evicted early, and while every pair is at its limit a sign-in that would need one more fails `ErrUnavailable` |

> decision: a full throttle refuses new pairs rather than leave them
> untracked, because an untracked pair is an unthrottled guesser. The cost is
> that a spray filling the table with pairs at their limit refuses every new
> pair for up to the 10-minute window. Revisit, with a per-address limit
> (§3.1's ponytail), when the logon stream shows sign-ins refused this way.

## 8. Management-API users

The management API has users of its own: operators who create shares, users
and grants. Their tokens, roles, sessions and sign-in throttle are
[RFC 23 §5](rfc-23-control-plane.md#5.%20Authentication%20and%20authorization)'s. The password and account
check is identity's, which the API calls directly (§8.1). This RFC fixes only where the two meet:

- **A role grants nothing on files.** An API role (`viewer`, `operator`,
  `admin`, [RFC 23 §5.2](rfc-23-control-plane.md#5.2%20Roles)) **MUST NOT** make a principal `admin`, privileged or granted on any share.
  File access comes only from share grants (§6.4) and the proof (§6.5).
- **An API user is not uid 0.** The API's bootstrap administrator **MUST NOT**
  be given uid 0, a share grant, or any spelling a file client could present,
  unless an operator gives it one.
- **An API-only account is no file principal.** A `User` record created with a
  role and no protocol spelling — the bootstrap administrator, a service
  account ([RFC 23 §5.1](rfc-23-control-plane.md#5.1%20API%20accounts%20are%20principals%20with%20a%20role)) — is given none: no uid, gid
  or SID is allocated for it (§2.3), no NT hash is kept, `Find` never returns
  it, and no ACL entry or owner can name it, so it can sign in over no file
  protocol. Giving it any file spelling — any scheme but `oidc` (§8.2) — makes it a file user, and allocates the
  spellings it still lacks in the same transaction; its NT hash is stored at
  its next password set (§3.5).
- An API user and a file principal **MAY** be one `User` record, sharing a
  name and password; the API reads its role, and file access reads its grants.
  A password change updates the NT hash with it while NTLM is on (§3.5);
  which wrapping key seals each is
  [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)'s.

> decision: API users and file principals are one `User` record kept apart by rule: a role is never read by
> resolution, and an account with no spelling cannot sign in over a file protocol. Overturned if an API account
> is needed that is not a `User` record.

### 8.1 The API's sign-in interface

The management API calls identity directly, never through `vfs`: it signs in
over no file protocol and acts on no share. `node.New` hands it this interface
and nothing else of identity
([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)):

```go
package identity // pkg/identity

// APISignIn is the management API's whole use of identity (RFC 23 §5.3).
type APISignIn interface {
	// VerifyPassword checks name's password against its stored hash and returns
	// the account's principal. A wrong password, an unknown name, a disabled or
	// deleted account, and an account with no password all fail ErrLogonFailure,
	// alike; ErrUnavailable when the store cannot answer.
	VerifyPassword(ctx context.Context, name string, password []byte) (metadata.Principal, error)
	// Account returns p's current record for the API's per-call check, through
	// the cache keyed on the principals generation (§7.2); ErrLogonFailure if
	// disabled or deleted.
	Account(ctx context.Context, p metadata.Principal) (metadata.User, error)
	// Hashes derives what a new password is stored as: the password hash and,
	// only while identity.ntlm is on and the account holds file spellings, the
	// NT hash (§3.5). It writes nothing; the API writes both with the record.
	Hashes(ctx context.Context, p metadata.Principal, password []byte) (PasswordHashes, error)
	// VerifyToken verifies an OpenID Connect ID token and returns the principal
	// its (issuer, subject) is linked to, minting an API-only one where the
	// issuer allows it, with the claims the API reads (§8.2). ErrLogonFailure
	// for a token that fails any check or an unlinked subject; ErrUnavailable
	// when the issuer's keys cannot be had.
	VerifyToken(ctx context.Context, raw []byte) (metadata.Principal, TokenClaims, error)
}

// TokenClaims are the claims of a verified ID token the API reads. None of
// them identifies the account.
type TokenClaims struct {
	Issuer string
	Groups []string // the values of the issuer's configured role claim, for RFC 23's role map
	Email  string   // set only when the token says email_verified
}
```

- Tokens, sessions, roles, the not-before time and the API's sign-in throttle
  stay RFC 23's: `Account` returns the record and the API reads its role and
  `NotBefore` itself. Identity reads no role.
- An API sign-in emits no event to the `logon` stream, which carries file
  protocol sign-ins; the API audits its own
  ([RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit)).
- `Hashes` is the one place the NT-hash rule of §3.5 is applied, so the API
  cannot store an NT hash that identity would refuse to keep.

### 8.2 OpenID Connect sign-in

An installation **MAY** configure OpenID Connect issuers (`identity.oidc`),
for management-API sign-in only. The API obtains an ID token
([RFC 23 §5.5](rfc-23-control-plane.md#5.5%20OpenID%20Connect%20sign-in)) and
calls `VerifyToken`, which **MUST** refuse the token unless every check holds
(OpenID Connect Core 1.0 §3.1.3.7):

- its signature verifies under a key of the issuer's key set (RFC 7517), named
  by the token's `kid`, with an algorithm the issuer's configuration allows,
  never `none`;
- `iss` is a configured issuer, exactly as configured;
- `aud` holds the issuer's configured audience, the installation's client ID
  unless set otherwise;
- `exp` is in the future and `nbf` and `iat`, when present, are not, each
  within `identity.oidc_clock_skew`;
- `nonce` matches, where the flow that got the token sent one; the device
  grant sends none.

**The issuer's keys.** Identity fetches each issuer's key set from the
`jwks_uri` its discovery document names, over TLS only, and caches it for
`identity.oidc_jwks_ttl`. A token naming a `kid` the cache lacks refetches
once, at most once a minute per issuer. When a fetch fails, a cached key set
no older than the TTL plus `identity.oidc_jwks_stale_grace` **MAY** be used,
each use counted; past it the sign-in fails `ErrUnavailable`, as §5.3 bounds a
directory answer. An unfetchable key set never yields a session, anonymous or
otherwise.

**From token to principal.** The subject is the `oidc` spelling (issuer,
`sub`) (§2.2), looked up in the protocol-ID index. Nothing else in the token
identifies the account: `email` and `preferred_username` can be reassigned at
the issuer, and **MUST NOT** be used to find or link a principal.

- An operator links an (issuer, subject) to an existing user by writing that
  spelling on its record
  ([RFC 23 §3.5](rfc-23-control-plane.md#3.5%20Users%2C%20groups%2C%20grants%20and%20netgroups)),
  and unlinks it by removing it. The `oidc` spelling is not a file spelling:
  it allocates nothing and gives the account no file access (§8).
- An unlinked subject is refused `ErrLogonFailure`, unless the issuer's `mint`
  is on (default off). Then the first sign-in mints an API-only user through
  `Minter`, holding the `oidc` spelling and no other, with no role of its own
  and a generated name the operator may change — never the token's
  `preferred_username` or `email`. A lost mint race binds to the winner, as
  §5.1 says.
- A verified `email` fills `User.Email` through `Minter.SetEmail` when it
  differs. An account linked to an issuer takes its address from it, read-only
  to the API (§2.2). A changed address at the issuer changes no principal.
- Group or role claims are handed back as `Groups`; the API maps them to roles
  ([RFC 23 §5.5](rfc-23-control-plane.md#5.5%20OpenID%20Connect%20sign-in)).
  A role still grants nothing on files (§8).

> decision: the `oidc` spelling keys on (issuer, subject) only. OpenID Connect
> Core guarantees that pair stable and never reassigned; an address or a user
> name can pass to another person at the issuer, who would then inherit the
> account. Overturned if an issuer is shown to reissue subjects, which no
> conforming issuer does.

> ponytail: SAML is not supported. Its bindings are browser redirects and
> posts, and the installation has no web interface; most identity providers
> that offer SAML also offer OpenID Connect. Revisit when a web console is
> added.

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
| ID token fails a check, or its subject is unlinked and minting is off | `ErrLogonFailure`; no session | §8.2 |
| issuer's key set unfetchable, cached set within grace | the cached keys are used, counted | §8.2 |
| issuer's key set unfetchable past the grace | `ErrUnavailable`; the sign-in fails | §8.2 |

## 10. Invariants and observability

### 10.1 Invariants

| # | Invariant | Checked by |
| --- | --- | --- |
| ID1 | Every comparison of identity compares `PrincipalID`s, never a spelling. | §2.3, §7.1 one uid, two proofs |
| ID2 | No ID given to a principal is given to another: `PrincipalID`, allocated uid, gid and RID alike. | §2.3 no reuse |
| ID3 | No uid or gid is ever a default; every user has a uid, every group a gid, every user and group a SID — an API-only account aside, which has none (§8). 65534 is anonymous's alone. | §2.3, §7.1 one uid, two proofs; §4.4 SID-less directory |
| ID4 | A SID never changes once given. | §4.4 SID-less directory |
| ID5 | Every cache of identity is keyed by everything its value was derived from: the proof key, the proof's name and kind included. | §2.3, §7.1 one uid, two proofs |
| ID6 | A store or directory error is never read as "no such principal", and never yields a guest, anonymous or default grant. | §5.4 error is not "unknown" |
| ID7 | A failed sign-in never yields a session. | §3.6 no map to guest |
| ID8 | `Claimed` and `None` callers (`AUTH_SYS`, `AUTH_NONE`) are never `authenticated` and never `admin`. | §6.5 admin needs proof |
| ID9 | `admin` is set only from a share grant, for a proof of `Secret` or above. | §6.5 admin needs proof; §8 roles grant nothing |
| ID10 | A squashed caller carries nothing of the principal it replaced. | §6.2 squash drops SIDs |
| ID11 | A group closure is used whole or not at all. | §2.5 group cap |
| ID12 | A directory answer is used no longer than the TTL plus the grace after it was given, read directly or through any cache derived from it. | §5.3 stale grace; §7.1 directory deadline |
| ID13 | A disabled or deleted local principal is refused on every call admitted after the first principals-generation read that follows the change; the node reads it at most 1 s apart (§7.2). | §7.2 disable takes effect |
| ID14 | Adapters map, squash and grant nothing; every spelling they show or read goes through `vfs.Service`'s `Spell` and `Find`. | the import test of [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites): no adapter imports either identity package or a store view |
| ID15 | A security context acts as the principal its sign-in bound it to, for its whole life; its name is never mapped again. | §3.1 binding |
| ID16 | An unlinked machine principal acts on files as `Anonymous`: never `authenticated`, and no share grant names it. | §2.4 machines |
| ID17 | A deny-only SID matches deny entries only; it never grants, through an ACL or a share grant. | §3.4 deny-only SIDs |
| ID18 | A grant of `none` is stored, and is distinct from no grant. | §6.4 `none` is stored |

### 10.2 Metrics

| Answers | Metric | Type |
| --- | --- | --- |
| Sign-ins by flavour and outcome | `identity_authentications_total{protocol, flavor, outcome}`: `flavor` = `krb5`, `ntlm`; `outcome` = `ok`, `logon_failure`, `disabled`, `invalid`, `unavailable`, `throttled`, `replay` | counter |
| Sign-in latency | `identity_authentication_seconds{flavor}` | histogram (`request`) |
| Unmapped spellings | `identity_unmapped_total{scheme}`: `scheme` = `uid`, `gid`, `sid`, `owner` | counter |
| Calls refused at resolution | `identity_resolution_refusals_total{reason}`: `reason` = `disabled`, `group_cap`, `store_unavailable`, `directory_unavailable`, `anonymous_missing` | counter |
| Directory lookups by result | `identity_directory_lookups_total{question, result}`: `question` = `name`, `sid`, `groups`, `group`; `result` = `fresh`, `queried`, `not_found`, `stale`, `refused` | counter |
| Directory query latency | `identity_directory_seconds` | histogram (`request`) |
| ID-token verifications by result | `identity_oidc_verifications_total{result}`: `ok`, `invalid`, `unlinked`, `minted`, `unavailable` | counter |
| Key-set fetches that failed, and stale key sets used | `identity_oidc_jwks_fetch_failures_total`, `identity_oidc_jwks_stale_total` | counter |
| Cache effectiveness | `identity_cache_lookups_total{cache, result}`: `cache` = `caller`, `directory`; `result` = `hit`, `miss` | counter |
| Age of the principals generation served | `config_generation_lag_seconds{kind="principals"}`, RFC 13's gauge | gauge |

| Condition | Scope | Raised |
| --- | --- | --- |
| `directory_unreachable` | installation | degraded while lookups are served stale within the grace; unhealthy once one is refused past it; cleared by the first query the directory answers |

No metric label or span attribute carries an issuer's subject, an address or a
user name.

**Spans.** `identity.authenticate` is a child of the adapter's request span. `identity.resolve` is a child of
the service-call span, opened only on a cache miss. Each directory query is a child of whichever opened it.
Attributes are the proof kind, the outcome, the cache result and the principal's ID: never a name, SID, ticket or
hash ([RFC 25 §5.4](rfc-25-observability.md#5.4%20What%20is%20never%20logged)).

`identity_directory_lookups_total{result="refused"}` rising with
`identity_resolution_refusals_total{reason="directory_unavailable"}` is a directory outage;
`identity_unmapped_total{scheme="uid"}` rising alone is a client sending uids the store does not know.

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
| §8 roles grant nothing | [RFC 23 §12](rfc-23-control-plane.md#12.%20Conformance)'s §5.1 check, which also asserts that the account holds no spelling | — |
| §7.2 disable takes effect | bob holds an SMB open and an NFS GSS context; make one call on each and assert a cache hit. Disable bob, and before the generation is applied, assert the next call is still served from the cache. Apply it (fake clock and the node's read); assert every call from both is refused. | a cached resolution outliving a disable |
| §2.5 group cap | A principal in `identity.max_groups` + 1 groups: assert refused, never resolved with fewer. A nested group whose read fails mid-closure: assert the call fails. | truncated group lists |
| §4.5 owner strings | Over `sec=krb5`, `chown` to `alice@corp.example` and to `alice@other.example`: assert success then `NFS4ERR_BADOWNER`. Over `AUTH_SYS`, `GETATTR`: assert numeric owners. A file owned by a deleted user: assert `nobody@…` and, over SMB, `S-1-0-0`. | names mapped by guesswork |
| §3.4 PAC | A ticket whose PAC server signature is corrupted: assert the sign-in fails. A ticket with no PAC for a user in a store group: assert the group applies. | an unverified PAC trusted |
| §3.4 deny-only SIDs | A test PAC marks `Finance` `SE_GROUP_USE_FOR_DENY_ONLY`. A file's ACL allows `Finance` read: assert refused. Another's ACL allows alice and denies `Finance` read: assert refused. A share whose only grant names `Finance` read-write: assert the share refuses alice. | a deny-only SID dropped, or treated as a group |
| §3.1 binding | Over SMB, alice holds a Kerberos session. Delete alice and create a user named alice: assert every call on the old session refused, and a fresh sign-in acts as the new principal. Repeat with an NTLM pass-through session, its user SID re-linked. | a live proof re-mapped by name |
| §3.5 pass-through | A directory user with no `krb` row signs in by NTLM pass-through: assert the caller is the principal holding its verified user SID. | pass-through with no mapping path |
| §2.4 machines | On a share whose only grant is authenticated users read, a machine principal reads a file: assert refused, the caller not `authenticated`. A second machine's state operation on the first's `SP4_MACH_CRED` client record: assert refused. A grant naming a machine: assert refused. | a machine treated as an authenticated file caller |
| §6.4 `none` is stored | Grant `Finance` read-write and alice `none`: assert alice refused. Remove alice's grant: assert alice granted through `Finance`. | `none` stored as no grant |
| §7.1 directory deadline | Resolve bob over NTLM; remove bob from a directory group. Keep the session open past the TTL: assert his next call drops the group, with no generation change. | a derived cache outliving the directory answer |
| §4.4 SID-less directory | An RFC 2307 directory entry with no SID: assert the minted principal holds a local SID from the RID counter, unchanged when the directory later adds one. | a directory principal with no SID |
| §3.1 logon throttle | Send 10 wrong passwords for alice from one address, then an 11th attempt with the right one: assert refused, outcome `throttled`. | a throttle that still checks the credential, or none |
| §7.3 full throttle | Size the throttle's share to hold four pairs, and bring four pairs to their limit under a fake clock. Sign in a fifth pair with the right password: assert refused `ErrUnavailable`, outcome `unavailable`, and the four pairs still throttled. Let one pair's window pass: assert the fifth pair's next sign-in is checked and succeeds. | a full throttle that evicts a pair at its limit, or admits a new pair untracked |
| §3.1 expiry | Over `sec=krb5`, keep a context open past its ticket's `Expires`: assert the next request is refused with the expired-credential answer. | a context served past its ticket |
| §3.3 replay | Send the same AP-REQ twice: assert the second refused, outcome `replay`. | no replay cache |
| §3.2 `ManageGIDs` | On a share with `ManageGIDs`, a user in 20 store groups calls over `AUTH_SYS`: assert a file whose ACL grants only the 20th group is granted. | groups capped at the credential's 16 |
| §3.5 NTLM off | Hold an NTLM session, then turn `identity.ntlm` off and apply it (fake clock and the node's read): assert its next call and a fresh NTLM sign-in refused, and once the deletion operation ends, no NT hash in the store. | an NTLM context outliving the setting; hashes left behind |
| §5.1 TLS | A directory record asking for plain LDAP: assert refused at validation. | a bind credential sent in clear |
| §5.2 one query per key | 100 concurrent lookups of one key against a counting fake directory: assert 1 query. | a sign-in storm multiplied per connection |
| §4.4 anonymous lookup | Over an anonymous session on `IPC$`, `LookupSids` of an unminted directory SID: assert no record written. | an unauthenticated caller minting principals |
| §4.6 name case | Create two users whose names differ only in case: assert the second refused as a duplicate. | names unique byte for byte |
| §5.5 `AUTH_SYS` from the directory | In `rfc2307` mode, an `AUTH_SYS` call sends the `uidNumber` of a directory entry with no record: assert the caller is that entry's minted principal, with its directory groups, and not `authenticated`. | a directory that serves names but never maps a claimed uid |
| §8.2 token checks | Against a test issuer, present ID tokens with the wrong issuer, the wrong audience, an expired `exp`, an `nbf` ahead past the skew, a corrupted signature and `alg` `none`: assert each refused `ErrLogonFailure`. | a check skipped |
| §8.2 key-set outage | Sign in once, then make the issuer's key set unreachable under a fake clock: within the TTL plus the grace assert sign-in succeeds, counted stale; past it assert `ErrUnavailable` and no session. | fail-open on an outage, or no grace |
| §8.2 unlinked subject | Sign in a subject linked to no user: assert refused with minting off; turn `mint` on: assert an API-only user minted with the `oidc` spelling only and no uid, gid or SID. | a subject let in unlinked; a minted file user |
| §8.2 address change | Link alice's subject, sign in; change her `email` at the issuer and give her old address to bob's subject: assert alice's next sign-in is the same principal with the new address, and bob's is refused. | linking by e-mail |
| §5.5 NTLM without a domain controller | With a directory and no pass-through configured, an NTLM sign-in for a user only in the directory: assert `ErrLogonFailure`, and no anonymous or guest session. | a directory-only user let in, or mapped to guest |
| §5.1 mint conflict | In `rfc2307` mode, mint directory user alice, then sign in bob, whose entry has another SID and alice's `uidNumber`: assert the sign-in fails, bob is bound to no principal, and alice's record is unchanged. | a lost mint bound to whichever principal holds the row |

**Fuzz.** `FuzzPAC`, `FuzzAPReq`, `FuzzSID` and `FuzzOwnerString` never panic, never
allocate past the input's size times a constant, and decode∘encode is the identity on what decodes; the NTLM
message decoder is RFC 20's. **Unit.** Resolution's five steps run as one table-driven test over (proof kind ×
squash × grant × machine) against a fake `Principals` and a fake `Records`.

**Who fakes what.** This suite drives identity alone, through its own interfaces, with no filesystem and no
adapter. `*vfs.Server`'s unit tests fake identity with `identitytest.Static`. Adapter tests fake only
`vfs.Service`, sign-in included ([RFC 20 §7](rfc-20-adapters.md#7.%20Tests)), and never identity. The management
API's sign-in is tested against the real identity server, called directly, never through `vfs`
([RFC 23 §12](rfc-23-control-plane.md#12.%20Conformance)).

## 12. Migration

| Slice | What moves | What is deleted |
| --- | --- | --- |
| 0, now | fixed in today's code: no default uid (allocate), the bootstrap admin without uid 0, no guest on a failed or unparsable session setup, plain LDAP refused | the 1000 default; `root_to_admin`, `all_to_admin` |
| 1 | the `pkg/identity` contract (§2.7) and allocation on RFC 16's uid, gid and RID counters; today's uid-derived SIDs written once as stored values, so no SID changes (ID4) | RID derivation from a uid |
| 2 | the authenticator: Kerberos and NTLM behind `Authenticator`, reached by adapters only through `vfs.Service`'s `SignIn`; logon events, the throttle; the API's password check behind `APISignIn` (§8.1) | the five-domain NTLM retry; the RPCSEC_GSS "unmapped → nobody" path |
| 3 | resolution in `*vfs.Server` ([RFC 17 §9](rfc-17-vfs.md#9.%20Migration) slice 2), squash `none`/`root`/`all`, stored `none` grants | the NFS share-permission resolver, the NFSv3 auth-context cache, SID share permissions, `default_permission` |
| 4 | the directory cache with stale grace; `Identities` with owner strings by name | the resolver cache's error TTL |

## 13. Open questions

The deferrals of §3.5, §4.3, §4.4, §5.1, §5.5 and §7.2 are recorded at their markers, with what would overturn each.

1. **Name case.** Whether case-insensitive uniqueness of names (§4.6) holds for
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
- **Windows** caps an access token at 1 024 SIDs, so an account in more groups than
  about 1 015 cannot sign in; §2.5's default sits at that limit.
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
