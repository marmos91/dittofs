package wsd

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// xmlEscaper escapes the five XML metacharacters. The SOAP messages are built by
// literal string substitution (Windows is prefix/order-sensitive, so no
// encoding/xml marshalling), which does no escaping of its own — so any value
// that can contain XML metacharacters (the inbound MessageID echoed into
// RelatesTo, the OS hostname / NetBIOS names) must be run through esc first, or
// a stray & / < makes the envelope non-well-formed and clients discard it.
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func esc(s string) string { return xmlEscaper.Replace(s) }

// WS-Discovery / WS-Addressing constants.
const (
	nsDiscovery = "http://schemas.xmlsoap.org/ws/2005/04/discovery"
	toDiscovery = "urn:schemas-xmlsoap-org:ws:2005:04:discovery"
	toAnonymous = "http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous"

	actionHello        = nsDiscovery + "/Hello"
	actionBye          = nsDiscovery + "/Bye"
	actionProbe        = nsDiscovery + "/Probe"
	actionProbeMatches = nsDiscovery + "/ProbeMatches"
	actionResolve      = nsDiscovery + "/Resolve"
	actionResolveMatch = nsDiscovery + "/ResolveMatches"

	// TypesComputer is what a Windows file server advertises: a Device that is a
	// Computer. The prefixes are declared on the envelope below.
	TypesComputer = "dp:Device pub:Computer"
)

// Endpoint is the advertised identity carried in Hello/Bye/ProbeMatch/
// ResolveMatch and referenced by the metadata endpoint.
type Endpoint struct {
	UUID            string // "urn:uuid:…" — stable endpoint reference
	Types           string // e.g. TypesComputer
	XAddrs          string // metadata transport URL, e.g. "http://<ip>:5357/<uuid>/"
	MetadataVersion int
	InstanceID      uint64 // AppSequence InstanceId (stable per process run)
}

// envelopeOpen is the SOAP envelope with every namespace WS-Discovery + the
// Windows publication schema needs declared up front, so element/Types prefixes
// (dp:, pub:) are always in scope.
const envelopeHeader = `<?xml version="1.0" encoding="utf-8"?>` +
	`<s:Envelope` +
	` xmlns:s="http://www.w3.org/2003/05/soap-envelope"` +
	` xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"` +
	` xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"` +
	` xmlns:dp="http://schemas.xmlsoap.org/ws/2006/02/devprof"` +
	` xmlns:pub="http://schemas.microsoft.com/windows/pub/2005/07">`

// announce builds a Hello or Bye. The body differs only by element name (Hello
// carries the full endpoint; Bye needs only the reference, but including the
// rest is harmless and matches wsdd).
func announce(action, element string, e Endpoint, msgNum uint64) []byte {
	r := strings.NewReplacer(
		"{action}", action,
		"{msgid}", MessageID(),
		"{to}", toDiscovery,
		"{inst}", strconv.FormatUint(e.InstanceID, 10),
		"{num}", strconv.FormatUint(msgNum, 10),
		"{element}", element,
		"{uuid}", esc(e.UUID),
		"{types}", e.Types, // constant QName list — no XML metacharacters
		"{xaddrs}", esc(e.XAddrs),
		"{mv}", strconv.Itoa(e.MetadataVersion),
	)
	return []byte(r.Replace(envelopeHeader +
		`<s:Header>` +
		`<a:Action>{action}</a:Action>` +
		`<a:MessageID>{msgid}</a:MessageID>` +
		`<a:To>{to}</a:To>` +
		`<d:AppSequence InstanceId="{inst}" MessageNumber="{num}"></d:AppSequence>` +
		`</s:Header>` +
		`<s:Body>` +
		`<d:{element}>` +
		`<a:EndpointReference><a:Address>{uuid}</a:Address></a:EndpointReference>` +
		`<d:Types>{types}</d:Types>` +
		`<d:XAddrs>{xaddrs}</d:XAddrs>` +
		`<d:MetadataVersion>{mv}</d:MetadataVersion>` +
		`</d:{element}>` +
		`</s:Body>` +
		`</s:Envelope>`))
}

// Hello announces the host's presence (multicast on start).
func Hello(e Endpoint, msgNum uint64) []byte { return announce(actionHello, "Hello", e, msgNum) }

// Bye announces the host's departure (multicast on stop).
func Bye(e Endpoint, msgNum uint64) []byte { return announce(actionBye, "Bye", e, msgNum) }

// match builds a ProbeMatches or ResolveMatches reply correlated to the inbound
// message via wsa:RelatesTo, sent unicast to the querier.
func match(action, matchesEl, matchEl, relatesTo string, e Endpoint, msgNum uint64) []byte {
	r := strings.NewReplacer(
		"{action}", action,
		"{msgid}", MessageID(),
		"{relates}", esc(relatesTo), // inbound MessageID — untrusted
		"{to}", toAnonymous,
		"{inst}", strconv.FormatUint(e.InstanceID, 10),
		"{num}", strconv.FormatUint(msgNum, 10),
		"{matches}", matchesEl,
		"{match}", matchEl,
		"{uuid}", esc(e.UUID),
		"{types}", e.Types, // constant QName list — no XML metacharacters
		"{xaddrs}", esc(e.XAddrs),
		"{mv}", strconv.Itoa(e.MetadataVersion),
	)
	return []byte(r.Replace(envelopeHeader +
		`<s:Header>` +
		`<a:Action>{action}</a:Action>` +
		`<a:MessageID>{msgid}</a:MessageID>` +
		`<a:RelatesTo>{relates}</a:RelatesTo>` +
		`<a:To>{to}</a:To>` +
		`<d:AppSequence InstanceId="{inst}" MessageNumber="{num}"></d:AppSequence>` +
		`</s:Header>` +
		`<s:Body>` +
		`<d:{matches}>` +
		`<d:{match}>` +
		`<a:EndpointReference><a:Address>{uuid}</a:Address></a:EndpointReference>` +
		`<d:Types>{types}</d:Types>` +
		`<d:XAddrs>{xaddrs}</d:XAddrs>` +
		`<d:MetadataVersion>{mv}</d:MetadataVersion>` +
		`</d:{match}>` +
		`</d:{matches}>` +
		`</s:Body>` +
		`</s:Envelope>`))
}

// ProbeMatch replies to a Probe.
func ProbeMatch(e Endpoint, relatesTo string, msgNum uint64) []byte {
	return match(actionProbeMatches, "ProbeMatches", "ProbeMatch", relatesTo, e, msgNum)
}

// ResolveMatch replies to a Resolve.
func ResolveMatch(e Endpoint, relatesTo string, msgNum uint64) []byte {
	return match(actionResolveMatch, "ResolveMatches", "ResolveMatch", relatesTo, e, msgNum)
}

// --- Inbound parsing ---

// inboundKind classifies a parsed WS-Discovery message.
type inboundKind int

const (
	kindUnknown inboundKind = iota
	kindProbe
	kindResolve
)

// inbound is the parsed subset of a WS-Discovery request we act on.
type inbound struct {
	kind      inboundKind
	messageID string // wsa:MessageID, echoed back in wsa:RelatesTo
	types     string // Probe: requested Types (may be empty = match all)
	address   string // Resolve: the EndpointReference address being resolved
}

// xmlEnvelope mirrors the inbound SOAP we parse. encoding/xml matches by local
// element name regardless of the sender's namespace prefixes, which is what we
// want across heterogeneous Windows clients.
type xmlEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Header  struct {
		Action    string `xml:"Action"`
		MessageID string `xml:"MessageID"`
	} `xml:"Header"`
	Body struct {
		Probe *struct {
			Types string `xml:"Types"`
		} `xml:"Probe"`
		Resolve *struct {
			EndpointReference struct {
				Address string `xml:"Address"`
			} `xml:"EndpointReference"`
		} `xml:"Resolve"`
	} `xml:"Body"`
}

// parseInbound extracts the actionable fields from a WS-Discovery datagram.
func parseInbound(data []byte) (inbound, error) {
	var env xmlEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return inbound{}, err
	}
	in := inbound{messageID: strings.TrimSpace(env.Header.MessageID)}
	action := strings.TrimSpace(env.Header.Action)
	switch {
	case action == actionProbe || env.Body.Probe != nil:
		in.kind = kindProbe
		if env.Body.Probe != nil {
			in.types = strings.TrimSpace(env.Body.Probe.Types)
		}
	case action == actionResolve || env.Body.Resolve != nil:
		in.kind = kindResolve
		if env.Body.Resolve != nil {
			in.address = strings.TrimSpace(env.Body.Resolve.EndpointReference.Address)
		}
	}
	return in, nil
}

// probeMatchesTypes reports whether a Probe with the given requested Types
// should match a Computer/Device host. An empty Types matches everything; a
// non-empty one matches when one of its space-separated QNames has a local part
// of exactly "Device" or "Computer" (prefix-agnostic). This deliberately does
// not match unrelated device types such as "i:PrintDevice".
func probeMatchesTypes(requested string) bool {
	if strings.TrimSpace(requested) == "" {
		return true
	}
	for _, tok := range strings.Fields(requested) {
		local := tok
		if i := strings.LastIndexByte(tok, ':'); i >= 0 {
			local = tok[i+1:]
		}
		if strings.EqualFold(local, "Device") || strings.EqualFold(local, "Computer") {
			return true
		}
	}
	return false
}
