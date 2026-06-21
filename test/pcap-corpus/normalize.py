#!/usr/bin/env python3
"""Normalize a tshark PDML capture into a canonical structural skeleton.

The skeleton is the *shape* of a protocol exchange — which fields and options
appear on the wire, in which nesting — with all volatile / payload-dependent
values stripped. Diffing two skeletons surfaces STRUCTURAL divergence (a field
DittoFS omits or adds versus a reference server) while ignoring legitimate
byte-level differences (timestamps, sequence numbers, nonces, session IDs,
encrypted blobs, addresses).

This is the oracle behind the AD pcap corpus (#1237): the reference captures
come from official servers (Samba AD-DC, knfsd, Windows), and a DittoFS capture
that yields a different skeleton for the same operation is a wire-interop bug.

Usage:
    tshark -r cap.pcap -T pdml [-d tcp.port==N,nbss] > cap.pdml
    normalize.py --proto kerberos < cap.pdml > cap.skel

--proto restricts the skeleton to packets containing that top-level protocol
tree (kerberos, ldap, nfs, smb2, spnego, ...). Repeat --proto to keep several.
"""

import argparse
import sys
import xml.etree.ElementTree as ET

# Field-name prefixes whose VALUES are volatile. We keep the field NAME in the
# skeleton (its presence is structural) but never its value, so these are about
# fields we drop ENTIRELY because even their presence is noise (framing,
# transport, timestamps, crypto material).
DROP_FIELDS = (
    # transport / framing — never part of application-protocol structure
    "frame", "eth", "ip", "ipv6", "tcp", "udp", "nbss", "t'",
    # generic crypto / opaque material whose presence is request-specific
    "kerberos.cipher", "kerberos.etype_info", "kerberos.checksum",
    "kerberos.authenticator", "kerberos.encrypted", "kerberos.ticket.data",
    "gss-api.OID", "spnego.mechToken", "gss-api",
    "smb2.session_id", "smb2.msg_id", "smb2.seqnum", "smb2.credits",
    "smb2.tree_id", "smb2.buffer_code", "smb2.signature",
    "nfs.stateid", "nfs.verifier", "nfs.clientid", "nfs.seqid",
    "nfs.cookie", "nfs.fh", "nfs.fhandle", "ldap.messageID",
)

# Field names whose presence IS structural but value should be kept because the
# value names a wire feature (an enum / flag / type). For these we append the
# `showname`-derived enum label, not the raw bytes.
KEEP_ENUM = (
    "kerberos.msg_type", "kerberos.padata_type", "kerberos.addr_type",
    "kerberos.NameType", "spnego.negResult", "ldap.protocolOp",
    "ldap.resultCode", "smb2.cmd", "smb2.nt_status", "smb2.class",
    "smb2.infolevel", "smb2.security.info", "nfs.opcode", "nfs.status",
    "nfs.attr", "nfs.ace.type", "nfs.ace.flag", "nfs.ace.mask",
    "nfs.fattr4.bitmap", "nfs.secinfo.flavor",
)


def dropped(name: str) -> bool:
    return any(name == d or name.startswith(d + ".") for d in DROP_FIELDS)


def is_enum(name: str) -> bool:
    return name in KEEP_ENUM


def walk(elem, path, out):
    """Collect structural field paths under elem into out (a set)."""
    for field in elem.findall("field"):
        name = field.get("name", "")
        if not name or dropped(name):
            # still descend: a dropped wrapper can contain kept children
            walk(field, path, out)
            continue
        node = name
        if is_enum(name):
            label = field.get("showname") or field.get("show") or ""
            # showname looks like "Type: AS-REQ (10)"; keep the symbolic part
            if ":" in label:
                label = label.split(":", 1)[1].strip()
            node = f"{name}={label}"
        child_path = path + "/" + node
        out.add(child_path)
        walk(field, child_path, out)


def packet_skeleton(packet, protos):
    out = set()
    for proto in packet.findall("proto"):
        pname = proto.get("name", "")
        if protos and pname not in protos:
            continue
        if not protos and pname in ("frame", "eth", "ip", "ipv6", "tcp", "udp"):
            continue
        walk(proto, pname, out)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--proto", action="append", default=[],
                    help="top-level proto tree(s) to keep (repeatable)")
    args = ap.parse_args()
    protos = set(args.proto)

    tree = ET.parse(sys.stdin)
    root = tree.getroot()

    skeleton = set()
    for packet in root.findall("packet"):
        skeleton |= packet_skeleton(packet, protos)

    for line in sorted(skeleton):
        print(line)


if __name__ == "__main__":
    main()
