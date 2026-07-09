package wsd

import (
	"strings"
	"testing"
)

func testEndpoint() Endpoint {
	return Endpoint{
		UUID:            "urn:uuid:0d117f5d-1609-4000-8000-000000001609",
		Types:           TypesComputer,
		XAddrs:          "http://192.168.100.50:5357/0d117f5d-1609-4000-8000-000000001609/",
		MetadataVersion: 1,
		InstanceID:      1700000000,
	}
}

func TestEndpointUUID_StableForSameName(t *testing.T) {
	a := endpointUUIDFor("VM2")
	b := endpointUUIDFor("VM2")
	if a != b {
		t.Fatalf("endpoint UUID not stable: %q vs %q", a, b)
	}
	if endpointUUIDFor("VM2") == endpointUUIDFor("OTHER") {
		t.Fatal("different names should yield different UUIDs")
	}
	if !strings.HasPrefix(a, "urn:uuid:") {
		t.Fatalf("UUID missing urn:uuid: prefix: %q", a)
	}
}

func TestMessageID_FreshAndPrefixed(t *testing.T) {
	if MessageID() == MessageID() {
		t.Fatal("MessageID should be unique per call")
	}
	if !strings.HasPrefix(MessageID(), "urn:uuid:") {
		t.Fatal("MessageID missing urn:uuid: prefix")
	}
}

func TestHello_ContainsRequiredElements(t *testing.T) {
	msg := string(Hello(testEndpoint(), 1))
	for _, want := range []string{
		actionHello,
		"<a:MessageID>",
		"urn:uuid:0d117f5d-1609-4000-8000-000000001609",
		"dp:Device pub:Computer",
		"http://192.168.100.50:5357/",
		"<d:MetadataVersion>1</d:MetadataVersion>",
		`InstanceId="1700000000"`,
		`xmlns:pub="http://schemas.microsoft.com/windows/pub/2005/07"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Hello missing %q\n---\n%s", want, msg)
		}
	}
}

func TestProbeMatch_CorrelatesWithRelatesTo(t *testing.T) {
	relatesTo := "urn:uuid:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	msg := string(ProbeMatch(testEndpoint(), relatesTo, 2))
	if !strings.Contains(msg, actionProbeMatches) {
		t.Error("ProbeMatch missing ProbeMatches action")
	}
	if !strings.Contains(msg, "<a:RelatesTo>"+relatesTo+"</a:RelatesTo>") {
		t.Errorf("ProbeMatch missing RelatesTo correlation\n%s", msg)
	}
	if !strings.Contains(msg, "<d:ProbeMatch>") {
		t.Error("ProbeMatch missing ProbeMatch element")
	}
	if !strings.Contains(msg, toAnonymous) {
		t.Error("ProbeMatch should be addressed to the anonymous role (unicast reply)")
	}
}

func TestResolveMatch_CarriesXAddrs(t *testing.T) {
	msg := string(ResolveMatch(testEndpoint(), "urn:uuid:1234", 3))
	if !strings.Contains(msg, actionResolveMatch) {
		t.Error("ResolveMatch missing ResolveMatches action")
	}
	if !strings.Contains(msg, "<d:XAddrs>http://192.168.100.50:5357/") {
		t.Errorf("ResolveMatch missing XAddrs (needed for the metadata GET)\n%s", msg)
	}
}

func TestParseInbound_Probe(t *testing.T) {
	probe := `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
 xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"
 xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
 <s:Header>
  <a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>
  <a:MessageID>urn:uuid:probe-1</a:MessageID>
 </s:Header>
 <s:Body><d:Probe><d:Types>d:Device</d:Types></d:Probe></s:Body>
</s:Envelope>`
	in, err := parseInbound([]byte(probe))
	if err != nil {
		t.Fatalf("parseInbound: %v", err)
	}
	if in.kind != kindProbe {
		t.Fatalf("kind = %v, want Probe", in.kind)
	}
	if in.messageID != "urn:uuid:probe-1" {
		t.Fatalf("messageID = %q", in.messageID)
	}
	if !probeMatchesTypes(in.types) {
		t.Fatalf("Device probe should match, types=%q", in.types)
	}
}

func TestParseInbound_ResolveAddress(t *testing.T) {
	resolve := `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
 xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"
 xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
 <s:Header>
  <a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Resolve</a:Action>
  <a:MessageID>urn:uuid:resolve-1</a:MessageID>
 </s:Header>
 <s:Body><d:Resolve><a:EndpointReference><a:Address>urn:uuid:target-xyz</a:Address></a:EndpointReference></d:Resolve></s:Body>
</s:Envelope>`
	in, err := parseInbound([]byte(resolve))
	if err != nil {
		t.Fatalf("parseInbound: %v", err)
	}
	if in.kind != kindResolve {
		t.Fatalf("kind = %v, want Resolve", in.kind)
	}
	if in.address != "urn:uuid:target-xyz" {
		t.Fatalf("resolve address = %q", in.address)
	}
}

func TestProbeMatchesTypes(t *testing.T) {
	cases := map[string]bool{
		"":                       true, // empty matches all
		"d:Device":               true,
		"pub:Computer":           true,
		"dp:Device pub:Computer": true,
		"i:PrintDevice":          false,
	}
	for types, want := range cases {
		if got := probeMatchesTypes(types); got != want {
			t.Errorf("probeMatchesTypes(%q) = %v, want %v", types, got, want)
		}
	}
}
