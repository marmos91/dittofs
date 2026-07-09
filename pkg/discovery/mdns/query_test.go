package mdns

import (
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// packQuery builds an mDNS query for name/type, optionally setting the QU
// (unicast-response) bit on the question class.
func packQuery(t *testing.T, name string, qt dnsmessage.Type, qu bool) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatalf("NewName: %v", err)
	}
	class := dnsmessage.ClassINET
	if qu {
		class = dnsmessage.Class(cacheFlushBit | uint16(dnsmessage.ClassINET))
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{},
		Questions: []dnsmessage.Question{{Name: n, Type: qt, Class: class}},
	}
	b, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack query: %v", err)
	}
	return b
}

func TestBuildResponse_PTRBrowseMatchesSMB(t *testing.T) {
	services := []ServiceRecord{smbService(), nfsService()}
	query := packQuery(t, "_smb._tcp.local.", dnsmessage.TypePTR, false)

	resp, unicast, ok, err := buildResponse(services, query)
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if !ok {
		t.Fatal("expected a response for _smb._tcp browse")
	}
	if unicast {
		t.Fatal("QU bit not set; response should be multicast")
	}

	_, recs := parseAll(t, resp)
	// Should include the SMB PTR (answer) but NOT the NFS PTR.
	for _, r := range recs {
		if b, ok := r.Body.(*dnsmessage.PTRResource); ok && r.Header.Name.String() == "_nfs._tcp.local." {
			t.Fatalf("response leaked an unrelated NFS PTR: %s", b.PTR.String())
		}
	}
	// And it should carry the SMB SRV as a supporting record.
	var sawSMBSRV bool
	for _, r := range recs {
		if b, ok := r.Body.(*dnsmessage.SRVResource); ok && b.Port == 445 {
			sawSMBSRV = true
		}
	}
	if !sawSMBSRV {
		t.Fatal("expected SMB SRV (port 445) among supporting records")
	}
}

func TestBuildResponse_QUBitRequestsUnicast(t *testing.T) {
	services := []ServiceRecord{smbService()}
	query := packQuery(t, "_smb._tcp.local.", dnsmessage.TypePTR, true)

	_, unicast, ok, err := buildResponse(services, query)
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if !ok {
		t.Fatal("expected a response")
	}
	if !unicast {
		t.Fatal("QU bit set; response should be unicast")
	}
}

func TestBuildResponse_NoMatchNoResponse(t *testing.T) {
	services := []ServiceRecord{smbService()}
	query := packQuery(t, "_afpovertcp._tcp.local.", dnsmessage.TypePTR, false)

	resp, _, ok, err := buildResponse(services, query)
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if ok || resp != nil {
		t.Fatal("unrelated query should produce no response")
	}
}

func TestBuildResponse_SRVQueryReturnsPortAndAddr(t *testing.T) {
	services := []ServiceRecord{nfsService()}
	query := packQuery(t, "DITTOFS._nfs._tcp.local.", dnsmessage.TypeSRV, false)

	resp, _, ok, err := buildResponse(services, query)
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if !ok {
		t.Fatal("expected a response for the SRV query")
	}
	_, recs := parseAll(t, resp)
	var sawSRV, sawA bool
	for _, r := range recs {
		switch b := r.Body.(type) {
		case *dnsmessage.SRVResource:
			sawSRV = true
			if b.Port != 12049 {
				t.Fatalf("SRV port = %d, want 12049", b.Port)
			}
		case *dnsmessage.AResource:
			sawA = true
		}
	}
	if !sawSRV || !sawA {
		t.Fatalf("SRV query response missing records: srv=%v a=%v", sawSRV, sawA)
	}
}

func TestBuildResponse_ServiceEnumeration(t *testing.T) {
	services := []ServiceRecord{smbService(), nfsService()}
	query := packQuery(t, "_services._dns-sd._udp.local.", dnsmessage.TypePTR, false)

	resp, _, ok, err := buildResponse(services, query)
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if !ok {
		t.Fatal("expected a response for service enumeration")
	}
	_, recs := parseAll(t, resp)
	types := map[string]bool{}
	for _, r := range recs {
		if b, ok := r.Body.(*dnsmessage.PTRResource); ok {
			types[b.PTR.String()] = true
		}
	}
	if !types["_smb._tcp.local."] || !types["_nfs._tcp.local."] {
		t.Fatalf("enumeration missing service types: %v", types)
	}
}
