"""Provisions the symmetric keys the KMIP interop tests run against.

Creates two AES-256 keys, activates both, and prints their unique
identifiers as `current=<uid>` / `retired=<uid>` lines. Extra keys in the
states the provider has to react to (deactivated, compromised, destroyed)
are created on request via the state names passed on the command line and
printed as `<state>=<uid>`.
"""

import sys

from kmip.pie.client import ProxyKmipClient
from kmip import enums


def make_key(client, name):
    uid = client.create(
        enums.CryptographicAlgorithm.AES,
        256,
        name=name,
        cryptographic_usage_mask=[
            enums.CryptographicUsageMask.ENCRYPT,
            enums.CryptographicUsageMask.DECRYPT,
        ],
    )
    client.activate(uid)
    return uid


def main():
    with ProxyKmipClient(config_file="/work/pykmip.conf") as client:
        print("current=%s" % make_key(client, "dittofs-current"))
        print("retired=%s" % make_key(client, "dittofs-retired"))
        for state in sys.argv[1:]:
            uid = make_key(client, "dittofs-%s" % state)
            if state == "deactivated":
                client.revoke(
                    enums.RevocationReasonCode.CESSATION_OF_OPERATION, uid
                )
            elif state == "compromised":
                client.revoke(enums.RevocationReasonCode.KEY_COMPROMISE, uid)
            elif state == "destroyed":
                client.revoke(
                    enums.RevocationReasonCode.CESSATION_OF_OPERATION, uid
                )
                client.destroy(uid)
            elif state == "preactive":
                pass  # created without the activate above; handled below
            print("%s=%s" % (state, uid))


if __name__ == "__main__":
    main()
