package mdns

import (
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// buildResponse parses an inbound mDNS query and packs a response advertising
// any registered services it matches. It is a pure function — the whole
// network-facing behavior of the responder reduces to it, so it is unit-tested
// without a socket.
//
//   - ok is false when no question matched a registered record (the caller sends
//     nothing).
//   - unicast is true when the querier set the QU (unicast-response) bit on a
//     matched question, so the caller should reply directly to the sender rather
//     than to the multicast group.
//
// The directly-queried record goes in the Answer section; supporting records
// (SRV/TXT/address for a PTR query, addresses for an SRV query) go in
// Additional. A shared seen-set dedupes records that several questions would
// otherwise repeat.
func buildResponse(services []ServiceRecord, query []byte) (resp []byte, unicast bool, ok bool, err error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, false, false, err
	}
	// Ignore responses (including other responders' announcements) — we answer
	// only queries.
	if hdr.Response {
		return nil, false, false, nil
	}
	questions, err := p.AllQuestions()
	if err != nil {
		return nil, false, false, err
	}
	if len(questions) == 0 {
		return nil, false, false, nil
	}

	acc := &answerSet{seen: make(map[string]bool)}
	for _, q := range questions {
		qname := q.Name.String()
		qt := q.Type
		qu := uint16(q.Class)&cacheFlushBit != 0 // QU bit reuses the top class bit

		for i := range services {
			s := services[i]
			matched := acc.matchQuestion(s, qname, qt)
			if matched && qu {
				unicast = true
			}
		}
	}

	if acc.err != nil {
		return nil, false, false, acc.err
	}
	if len(acc.answers) == 0 {
		return nil, false, false, nil
	}

	msg := dnsmessage.Message{
		Header:      dnsmessage.Header{Response: true, Authoritative: true},
		Answers:     acc.answers,
		Additionals: acc.additionals,
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil, false, false, err
	}
	return packed, unicast, true, nil
}

// answerSet accumulates the answer and additional records for a response,
// deduplicating by a caller-supplied key shared across both sections.
type answerSet struct {
	seen        map[string]bool
	answers     []dnsmessage.Resource
	additionals []dnsmessage.Resource
	err         error
}

// rec adapts a record builder's (Resource, error) result for the answer/
// additional helpers: it latches any build error into a.err and returns the
// resource (zero on error, which the latched a.err then suppresses). This lets
// call sites stay one line — a.answer(key, a.rec(s.srvRecord(false))).
func (a *answerSet) rec(r dnsmessage.Resource, err error) dnsmessage.Resource {
	if err != nil && a.err == nil {
		a.err = err
	}
	return r
}

// answer adds a record to the Answer section unless its key was already emitted
// or a build error has been latched.
func (a *answerSet) answer(key string, r dnsmessage.Resource) { a.appendTo(&a.answers, key, r) }

// additional adds a record to the Additional section under the same dedupe set.
func (a *answerSet) additional(key string, r dnsmessage.Resource) {
	a.appendTo(&a.additionals, key, r)
}

func (a *answerSet) appendTo(dst *[]dnsmessage.Resource, key string, r dnsmessage.Resource) {
	if a.err != nil || a.seen[key] {
		return
	}
	a.seen[key] = true
	*dst = append(*dst, r)
}

// matchQuestion adds the records service s owes for question (qname, qt) and
// reports whether anything matched. qt==ANY matches every record type, so the
// checks are independent (not a switch).
func (a *answerSet) matchQuestion(s ServiceRecord, qname string, qt dnsmessage.Type) bool {
	isPTR := qt == dnsmessage.TypePTR || qt == typeANY
	isSRV := qt == dnsmessage.TypeSRV || qt == typeANY
	isTXT := qt == dnsmessage.TypeTXT || qt == typeANY
	isA := qt == dnsmessage.TypeA || qt == typeANY
	isAAAA := qt == dnsmessage.TypeAAAA || qt == typeANY

	matched := false

	// PTR browse: "_smb._tcp.local." -> instance, plus supporting records.
	if isPTR && equalName(qname, s.serviceName()) {
		a.answer("ptr:"+s.serviceName(), a.rec(s.ptrRecord(false)))
		a.additional("srv:"+s.instanceName(), a.rec(s.srvRecord(false)))
		a.additional("txt:"+s.instanceName(), a.rec(s.txtRecord(false)))
		a.addAddrs(s, true, true, false)
		matched = true
	}
	// Service-type enumeration.
	if isPTR && equalName(qname, s.metaName()) {
		a.answer("meta:"+s.metaName()+"/"+s.serviceName(), a.rec(s.metaPTRRecord(false)))
		matched = true
	}
	// Direct SRV/TXT for the instance.
	if isSRV && equalName(qname, s.instanceName()) {
		a.answer("srv:"+s.instanceName(), a.rec(s.srvRecord(false)))
		a.addAddrs(s, true, true, false)
		matched = true
	}
	if isTXT && equalName(qname, s.instanceName()) {
		a.answer("txt:"+s.instanceName(), a.rec(s.txtRecord(false)))
		matched = true
	}
	// Host address records.
	if (isA || isAAAA) && equalName(qname, s.hostName()) {
		a.addAddrs(s, isA, isAAAA, true)
		matched = true
	}
	return matched
}

// addAddrs adds the host A/AAAA records (as answers when toAnswer, else as
// additionals), filtered to the requested families.
func (a *answerSet) addAddrs(s ServiceRecord, wantA, wantAAAA, toAnswer bool) {
	recs, err := s.addrRecords(false)
	if err != nil {
		if a.err == nil {
			a.err = err
		}
		return
	}
	for _, r := range recs {
		var key string
		switch r.Header.Type {
		case dnsmessage.TypeA:
			if !wantA {
				continue
			}
			key = "a:" + s.hostName() + ":" + addrKey(r)
		case dnsmessage.TypeAAAA:
			if !wantAAAA {
				continue
			}
			key = "aaaa:" + s.hostName() + ":" + addrKey(r)
		default:
			continue
		}
		if toAnswer {
			a.answer(key, r)
		} else {
			a.additional(key, r)
		}
	}
}

// addrKey returns a stable string for an A/AAAA record's address bytes.
func addrKey(r dnsmessage.Resource) string {
	switch b := r.Body.(type) {
	case *dnsmessage.AResource:
		return string(b.A[:])
	case *dnsmessage.AAAAResource:
		return string(b.AAAA[:])
	default:
		return ""
	}
}

// isQuery reports whether data is a DNS query (not a response) worth answering.
// It parses only the header, so the responder can drop the many response packets
// on the multicast group (other hosts' and our own announcements) before the
// more expensive record snapshot and full parse in buildResponse.
func isQuery(data []byte) bool {
	var p dnsmessage.Parser
	hdr, err := p.Start(data)
	return err == nil && !hdr.Response
}

// equalName compares two DNS names case-insensitively (mDNS names are
// case-insensitive; both sides are fully qualified with a trailing dot).
func equalName(a, b string) bool { return strings.EqualFold(a, b) }
