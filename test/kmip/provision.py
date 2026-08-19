"""Provisions the keys the KMIP interop tests run against.

Creates one AES-256 key per state the provider has to react to and prints
the resulting unique identifiers as `NAME=uid` lines, ready to be fed
straight into the environment the Go tests read.

Destroyed is deliberately absent: PyKMIP's Destroy removes the object from
its database outright, so a subsequent GetAttributes reports Item Not
Found rather than State Destroyed. That state is only reachable against
the in-process fake server.
"""

from kmip.pie.client import ProxyKmipClient
from kmip import enums


def make_key(client, name, activate=True):
    uid = client.create(
        enums.CryptographicAlgorithm.AES,
        256,
        name=name,
        cryptographic_usage_mask=[
            enums.CryptographicUsageMask.ENCRYPT,
            enums.CryptographicUsageMask.DECRYPT,
        ],
    )
    if activate:
        client.activate(uid)
    return uid


def main():
    with ProxyKmipClient(config_file="/work/pykmip.conf") as client:
        current = make_key(client, "dittofs-current")
        retired = make_key(client, "dittofs-retired")

        deactivated = make_key(client, "dittofs-deactivated")
        client.revoke(enums.RevocationReasonCode.CESSATION_OF_OPERATION, deactivated)

        compromised = make_key(client, "dittofs-compromised")
        client.revoke(enums.RevocationReasonCode.KEY_COMPROMISE, compromised)

        preactive = make_key(client, "dittofs-preactive", activate=False)

    for name, uid in (
        ("KEY_UID", current),
        ("RETIRED_KEY_UID", retired),
        ("DEACTIVATED_KEY_UID", deactivated),
        ("COMPROMISED_KEY_UID", compromised),
        ("PREACTIVE_KEY_UID", preactive),
    ):
        print("DITTOFS_TEST_KMIP_%s=%s" % (name, uid))


if __name__ == "__main__":
    main()
