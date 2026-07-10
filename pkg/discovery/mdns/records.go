package mdns

import (
	"golang.org/x/net/dns/dnsmessage"
)

const (
	// ttlDefault is the TTL for the unique records (SRV/TXT/A/AAAA); ttlPTR is
	// the longer TTL DNS-SD conventionally uses for the shared PTR records.
	ttlDefault uint32 = 120
	ttlPTR     uint32 = 4500

	// cacheFlushBit is the top bit of the record CLASS. Set on unique records
	// so responders replace any cached copy (RFC 6762 §10.2). Shared PTR
	// records leave it clear.
	cacheFlushBit = 0x8000

	// typeANY is the QTYPE for "any record" queries (RFC 1035); dnsmessage does
	// not define a constant for it.
	typeANY dnsmessage.Type = 255
)

// ttlOf returns 0 for a goodbye record (evicts caches immediately) or base
// otherwise.
func ttlOf(base uint32, goodbye bool) uint32 {
	if goodbye {
		return 0
	}
	return base
}

// classOf returns ClassINET, with the cache-flush bit set when flush is true.
func classOf(flush bool) dnsmessage.Class {
	if flush {
		return dnsmessage.Class(cacheFlushBit | uint16(dnsmessage.ClassINET))
	}
	return dnsmessage.ClassINET
}

func (s ServiceRecord) ptrRecord(goodbye bool) (dnsmessage.Resource, error) {
	name, err := dnsmessage.NewName(s.serviceName())
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	target, err := dnsmessage.NewName(s.instanceName())
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypePTR, Class: classOf(false), TTL: ttlOf(ttlPTR, goodbye)},
		Body:   &dnsmessage.PTRResource{PTR: target},
	}, nil
}

func (s ServiceRecord) metaPTRRecord(goodbye bool) (dnsmessage.Resource, error) {
	name, err := dnsmessage.NewName(s.metaName())
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	target, err := dnsmessage.NewName(s.serviceName())
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypePTR, Class: classOf(false), TTL: ttlOf(ttlPTR, goodbye)},
		Body:   &dnsmessage.PTRResource{PTR: target},
	}, nil
}

func (s ServiceRecord) srvRecord(goodbye bool) (dnsmessage.Resource, error) {
	name, err := dnsmessage.NewName(s.instanceName())
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	target, err := dnsmessage.NewName(s.hostName())
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeSRV, Class: classOf(true), TTL: ttlOf(ttlDefault, goodbye)},
		Body:   &dnsmessage.SRVResource{Priority: 0, Weight: 0, Port: s.Port, Target: target},
	}, nil
}

func (s ServiceRecord) txtRecord(goodbye bool) (dnsmessage.Resource, error) {
	name, err := dnsmessage.NewName(s.instanceName())
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: classOf(true), TTL: ttlOf(ttlDefault, goodbye)},
		Body:   &dnsmessage.TXTResource{TXT: s.txtStrings()},
	}, nil
}

// addrRecords returns the A and AAAA records for the service's host name.
func (s ServiceRecord) addrRecords(goodbye bool) ([]dnsmessage.Resource, error) {
	host, err := dnsmessage.NewName(s.hostName())
	if err != nil {
		return nil, err
	}
	var out []dnsmessage.Resource
	for _, ip := range s.IPv4 {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		var a [4]byte
		copy(a[:], v4)
		out = append(out, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: host, Type: dnsmessage.TypeA, Class: classOf(true), TTL: ttlOf(ttlDefault, goodbye)},
			Body:   &dnsmessage.AResource{A: a},
		})
	}
	for _, ip := range s.IPv6 {
		v16 := ip.To16()
		if v16 == nil || ip.To4() != nil {
			continue
		}
		var a [16]byte
		copy(a[:], v16)
		out = append(out, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: host, Type: dnsmessage.TypeAAAA, Class: classOf(true), TTL: ttlOf(ttlDefault, goodbye)},
			Body:   &dnsmessage.AAAAResource{AAAA: a},
		})
	}
	return out, nil
}

// records returns the full record set describing the service, in the order used
// for unsolicited announcements: [sharedPTR, metaPTR, SRV, TXT, A..., AAAA...].
func (s ServiceRecord) records(goodbye bool) ([]dnsmessage.Resource, error) {
	ptr, err := s.ptrRecord(goodbye)
	if err != nil {
		return nil, err
	}
	meta, err := s.metaPTRRecord(goodbye)
	if err != nil {
		return nil, err
	}
	srv, err := s.srvRecord(goodbye)
	if err != nil {
		return nil, err
	}
	txt, err := s.txtRecord(goodbye)
	if err != nil {
		return nil, err
	}
	addrs, err := s.addrRecords(goodbye)
	if err != nil {
		return nil, err
	}
	out := []dnsmessage.Resource{ptr, meta, srv, txt}
	return append(out, addrs...), nil
}

// announcement packs an unsolicited multicast response advertising every
// service. When goodbye is true the records carry TTL 0 (a departure notice).
func announcement(services []ServiceRecord, goodbye bool) ([]byte, error) {
	msg := dnsmessage.Message{
		Header:  dnsmessage.Header{Response: true, Authoritative: true},
		Answers: make([]dnsmessage.Resource, 0, len(services)*5),
	}
	for _, s := range services {
		rs, err := s.records(goodbye)
		if err != nil {
			return nil, err
		}
		msg.Answers = append(msg.Answers, rs...)
	}
	return msg.Pack()
}
