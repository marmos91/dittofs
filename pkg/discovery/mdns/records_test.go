package mdns

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func nfsService() ServiceRecord {
	return ServiceRecord{
		Instance: "DITTOFS",
		Service:  "_nfs._tcp",
		Port:     12049,
		TXT:      []string{"path=/ditto"},
		IPv4:     []net.IP{net.IPv4(192, 168, 100, 50)},
	}
}

func smbService() ServiceRecord {
	return ServiceRecord{
		Instance: "DITTOFS",
		Service:  "_smb._tcp",
		Port:     445,
		IPv4:     []net.IP{net.IPv4(192, 168, 100, 50)},
	}
}

// parseAll unpacks a packed message into its records for assertions.
func parseAll(t *testing.T, msg []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatalf("SkipAllQuestions: %v", err)
	}
	var recs []dnsmessage.Resource
	for {
		r, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			t.Fatalf("AnswerHeader: %v", err)
		}
		body, err := parseBody(&p, r)
		if err != nil {
			t.Fatalf("parse body: %v", err)
		}
		recs = append(recs, dnsmessage.Resource{Header: r, Body: body})
	}
	if err := p.SkipAllAuthorities(); err != nil {
		t.Fatalf("SkipAllAuthorities: %v", err)
	}
	for {
		r, err := p.AdditionalHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			t.Fatalf("AdditionalHeader: %v", err)
		}
		body, err := parseBody(&p, r)
		if err != nil {
			t.Fatalf("parse body: %v", err)
		}
		recs = append(recs, dnsmessage.Resource{Header: r, Body: body})
	}
	return h, recs
}

func parseBody(p *dnsmessage.Parser, h dnsmessage.ResourceHeader) (dnsmessage.ResourceBody, error) {
	switch h.Type {
	case dnsmessage.TypePTR:
		b, err := p.PTRResource()
		return &b, err
	case dnsmessage.TypeSRV:
		b, err := p.SRVResource()
		return &b, err
	case dnsmessage.TypeTXT:
		b, err := p.TXTResource()
		return &b, err
	case dnsmessage.TypeA:
		b, err := p.AResource()
		return &b, err
	case dnsmessage.TypeAAAA:
		b, err := p.AAAAResource()
		return &b, err
	default:
		return nil, p.SkipAnswer()
	}
}

func TestAnnouncement_NFSRecordsRoundTrip(t *testing.T) {
	msg, err := announcement([]ServiceRecord{nfsService()}, false)
	if err != nil {
		t.Fatalf("announcement: %v", err)
	}
	h, recs := parseAll(t, msg)
	if !h.Response || !h.Authoritative {
		t.Fatalf("header Response=%v Authoritative=%v, want both true", h.Response, h.Authoritative)
	}

	var sawPTR, sawMeta, sawSRV, sawTXT, sawA bool
	for _, r := range recs {
		switch b := r.Body.(type) {
		case *dnsmessage.PTRResource:
			switch r.Header.Name.String() {
			case "_nfs._tcp.local.":
				sawPTR = true
				if b.PTR.String() != "DITTOFS._nfs._tcp.local." {
					t.Fatalf("PTR target = %q", b.PTR.String())
				}
			case "_services._dns-sd._udp.local.":
				sawMeta = true
			}
		case *dnsmessage.SRVResource:
			sawSRV = true
			if b.Port != 12049 {
				t.Fatalf("SRV port = %d, want 12049", b.Port)
			}
			if b.Target.String() != "DITTOFS.local." {
				t.Fatalf("SRV target = %q, want DITTOFS.local.", b.Target.String())
			}
		case *dnsmessage.TXTResource:
			sawTXT = true
			if len(b.TXT) != 1 || b.TXT[0] != "path=/ditto" {
				t.Fatalf("TXT = %v, want [path=/ditto]", b.TXT)
			}
		case *dnsmessage.AResource:
			sawA = true
			if net.IP(b.A[:]).String() != "192.168.100.50" {
				t.Fatalf("A = %v", net.IP(b.A[:]))
			}
		}
	}
	if !sawPTR || !sawMeta || !sawSRV || !sawTXT || !sawA {
		t.Fatalf("missing records: ptr=%v meta=%v srv=%v txt=%v a=%v", sawPTR, sawMeta, sawSRV, sawTXT, sawA)
	}
}

func TestGoodbye_TTLZero(t *testing.T) {
	msg, err := announcement([]ServiceRecord{nfsService()}, true)
	if err != nil {
		t.Fatalf("announcement(goodbye): %v", err)
	}
	_, recs := parseAll(t, msg)
	if len(recs) == 0 {
		t.Fatal("no records")
	}
	for _, r := range recs {
		if r.Header.TTL != 0 {
			t.Fatalf("goodbye record %s has TTL %d, want 0", r.Header.Name.String(), r.Header.TTL)
		}
	}
}

func TestEmptyTXT_HasOneEmptyString(t *testing.T) {
	// SMB advertises an empty-but-present TXT record.
	msg, err := announcement([]ServiceRecord{smbService()}, false)
	if err != nil {
		t.Fatalf("announcement: %v", err)
	}
	_, recs := parseAll(t, msg)
	var found bool
	for _, r := range recs {
		if b, ok := r.Body.(*dnsmessage.TXTResource); ok {
			found = true
			if len(b.TXT) != 1 || b.TXT[0] != "" {
				t.Fatalf("empty TXT = %v, want one empty string", b.TXT)
			}
		}
	}
	if !found {
		t.Fatal("no TXT record emitted")
	}
}

func TestCacheFlushBit_OnUniqueRecordsOnly(t *testing.T) {
	msg, err := announcement([]ServiceRecord{nfsService()}, false)
	if err != nil {
		t.Fatalf("announcement: %v", err)
	}
	_, recs := parseAll(t, msg)
	for _, r := range recs {
		flush := uint16(r.Header.Class)&cacheFlushBit != 0
		switch r.Header.Type {
		case dnsmessage.TypePTR:
			if flush {
				t.Fatalf("PTR %s must not set cache-flush", r.Header.Name.String())
			}
		case dnsmessage.TypeSRV, dnsmessage.TypeTXT, dnsmessage.TypeA, dnsmessage.TypeAAAA:
			if !flush {
				t.Fatalf("record type %v must set cache-flush", r.Header.Type)
			}
		}
	}
}
