package wsd

import (
	"io"
	"net/http"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
)

// MetadataPort is the TCP port for the WS-Transfer metadata exchange. Windows
// fetches device metadata here after a ProbeMatch/ResolveMatch, and it is this
// exchange — specifically the pub:Computer relationship it returns — that makes
// Explorer render the host as a Computer rather than a generic device.
const MetadataPort = 5357

const (
	actionGet         = "http://schemas.xmlsoap.org/ws/2004/09/transfer/Get"
	actionGetResponse = "http://schemas.xmlsoap.org/ws/2004/09/transfer/GetResponse"
)

// metadataBuilder holds the fixed identity used to answer the metadata Get.
type metadataBuilder struct {
	uuid      string // "urn:uuid:…"
	name      string // computer / friendly name
	workgroup string // NetBIOS domain or workgroup label
	isDomain  bool   // true => "Domain:", false => "Workgroup:"
}

// buildGetResponse renders the WS-Transfer GetResponse. It is kept as an
// explicit byte template (not an encoding/xml struct tree) because Windows'
// WS-Transfer/devprof parser is sensitive to namespace prefixes and element
// ordering; the template pins both.
func (b metadataBuilder) buildGetResponse(relatesTo string) []byte {
	membership := "Workgroup"
	if b.isDomain {
		membership = "Domain"
	}
	// Escape every substituted value: relatesTo is the (untrusted) inbound
	// MessageID, and name/workgroup come from the OS hostname / NetBIOS domain —
	// an unescaped & or < would make the whole envelope non-well-formed and the
	// client would silently discard it.
	r := strings.NewReplacer(
		"{action}", actionGetResponse,
		"{msgid}", MessageID(),
		"{relates}", esc(relatesTo),
		"{to}", toAnonymous,
		"{uuid}", esc(b.uuid),
		"{name}", esc(b.name),
		"{membership}", membership,
		"{workgroup}", esc(b.workgroup),
	)
	return []byte(r.Replace(`<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope` +
		` xmlns:s="http://www.w3.org/2003/05/soap-envelope"` +
		` xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"` +
		` xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"` +
		` xmlns:dp="http://schemas.xmlsoap.org/ws/2006/02/devprof"` +
		` xmlns:mex="http://schemas.xmlsoap.org/ws/2004/09/mex"` +
		` xmlns:pub="http://schemas.microsoft.com/windows/pub/2005/07">` +
		`<s:Header>` +
		`<a:Action>{action}</a:Action>` +
		`<a:MessageID>{msgid}</a:MessageID>` +
		`<a:RelatesTo>{relates}</a:RelatesTo>` +
		`<a:To>{to}</a:To>` +
		`</s:Header>` +
		`<s:Body>` +
		`<mex:Metadata>` +
		`<mex:MetadataSection Dialect="http://schemas.xmlsoap.org/ws/2006/02/devprof/ThisModel">` +
		`<dp:ThisModel>` +
		`<dp:Manufacturer>DittoFS</dp:Manufacturer>` +
		`<dp:ModelName>DittoFS Server</dp:ModelName>` +
		`</dp:ThisModel>` +
		`</mex:MetadataSection>` +
		`<mex:MetadataSection Dialect="http://schemas.xmlsoap.org/ws/2006/02/devprof/ThisDevice">` +
		`<dp:ThisDevice>` +
		`<dp:FriendlyName>{name}</dp:FriendlyName>` +
		`<dp:FirmwareVersion>1.0</dp:FirmwareVersion>` +
		`<dp:SerialNumber>{uuid}</dp:SerialNumber>` +
		`</dp:ThisDevice>` +
		`</mex:MetadataSection>` +
		`<mex:MetadataSection Dialect="http://schemas.xmlsoap.org/ws/2006/02/devprof/Relationship">` +
		`<dp:Relationship Type="http://schemas.xmlsoap.org/ws/2006/02/devprof/host">` +
		`<dp:Host>` +
		`<a:EndpointReference><a:Address>{uuid}</a:Address></a:EndpointReference>` +
		`<dp:Types>pub:Computer</dp:Types>` +
		`<dp:ServiceId>{uuid}</dp:ServiceId>` +
		`<pub:Computer>{name}/{membership}:{workgroup}</pub:Computer>` +
		`</dp:Host>` +
		`</dp:Relationship>` +
		`</mex:MetadataSection>` +
		`</mex:Metadata>` +
		`</s:Body>` +
		`</s:Envelope>`))
}

// metadataHandler serves the WS-Transfer Get. It echoes the inbound MessageID
// into the response's RelatesTo (WS-Addressing correlation) and returns the
// device metadata for any POST, since the XAddrs path already scopes the
// request to this endpoint.
func (b metadataBuilder) metadataHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		if err != nil {
			logger.Debug("wsd: metadata request read error", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		relatesTo := ""
		if in, perr := parseInbound(body); perr == nil {
			relatesTo = in.messageID
		}
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		if _, werr := w.Write(b.buildGetResponse(relatesTo)); werr != nil {
			logger.Debug("wsd: metadata response write error", "error", werr)
		}
	}
}
