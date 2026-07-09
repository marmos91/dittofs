package wsd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleGet = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
 xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing">
 <s:Header>
  <a:Action>http://schemas.xmlsoap.org/ws/2004/09/transfer/Get</a:Action>
  <a:MessageID>urn:uuid:get-req-1</a:MessageID>
 </s:Header>
 <s:Body/>
</s:Envelope>`

func TestMetadataHandler_RendersComputer(t *testing.T) {
	mb := metadataBuilder{uuid: "urn:uuid:test-uuid", name: "VM2", workgroup: "CUBBIT"}
	srv := httptest.NewServer(mb.metadataHandler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/test-uuid/", "application/soap+xml", strings.NewReader(sampleGet))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{
		actionGetResponse,
		"<a:RelatesTo>urn:uuid:get-req-1</a:RelatesTo>", // correlates with the request
		`Type="http://schemas.xmlsoap.org/ws/2006/02/devprof/host"`,
		"<pub:Computer>VM2/Workgroup:CUBBIT</pub:Computer>", // the element that makes Explorer show a Computer
		"<dp:FriendlyName>VM2</dp:FriendlyName>",
		"<dp:Manufacturer>DittoFS</dp:Manufacturer>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("metadata response missing %q\n---\n%s", want, got)
		}
	}
}

func TestMetadataHandler_DomainMemberLabelsDomain(t *testing.T) {
	mb := metadataBuilder{uuid: "urn:uuid:x", name: "VM2", workgroup: "CUBBIT", isDomain: true}
	got := string(mb.buildGetResponse("urn:uuid:r"))
	if !strings.Contains(got, "<pub:Computer>VM2/Domain:CUBBIT</pub:Computer>") {
		t.Errorf("AD member should be labelled Domain:, got:\n%s", got)
	}
}

func TestMetadataHandler_EscapesName(t *testing.T) {
	mb := metadataBuilder{uuid: "urn:uuid:x", name: "A&B", workgroup: "W<G", isDomain: false}
	got := string(mb.buildGetResponse("urn:uuid:r"))
	if strings.Contains(got, "A&B") || strings.Contains(got, "W<G") {
		t.Errorf("name/workgroup not XML-escaped:\n%s", got)
	}
	if !strings.Contains(got, "A&amp;B/Workgroup:W&lt;G") {
		t.Errorf("expected escaped pub:Computer content:\n%s", got)
	}
}

func TestMetadataHandler_RejectsGET(t *testing.T) {
	mb := metadataBuilder{uuid: "urn:uuid:x", name: "N", workgroup: "W"}
	srv := httptest.NewServer(mb.metadataHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
}

// TestHandleDatagram_NoPanicWithoutSocket ensures the inbound dispatch is safe
// even before the socket is up (send is a no-op on a nil conn).
func TestHandleDatagram_NoPanicWithoutSocket(t *testing.T) {
	r := NewResponder("VM2", "CUBBIT", true, 1)
	r.endpoint = Endpoint{UUID: "urn:uuid:self"}

	probe := []byte(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
 xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"
 xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
 <s:Header><a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>
 <a:MessageID>urn:uuid:p</a:MessageID></s:Header>
 <s:Body><d:Probe><d:Types>d:Device</d:Types></d:Probe></s:Body></s:Envelope>`)
	// Should not panic; send is a no-op because udpConn is nil.
	r.handleDatagram(probe, nil)
}
