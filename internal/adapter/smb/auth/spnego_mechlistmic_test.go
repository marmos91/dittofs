package auth

import "testing"

// TestParsedToken_HasMechListMIC is the #1530 regression guard. A real Windows
// SMB client authenticating with Kerberos presents an optimistic AP-REQ for its
// preferred mechanism and sends NO mechListMIC of its own. Per RFC 4178 §5 the
// acceptor must then answer WITHOUT a mechListMIC; attaching an unsolicited one
// makes Windows reject the completed context and forcibly close the connection
// right after the signed SESSION_SETUP success (System error 1359). The Kerberos
// response builders gate the acceptor MIC on this predicate, so it must report
// true only when the initiator actually supplied a mechListMIC.
func TestParsedToken_HasMechListMIC(t *testing.T) {
	tests := []struct {
		name  string
		token *ParsedToken
		want  bool
	}{
		{
			name:  "nil token",
			token: nil,
			want:  false,
		},
		{
			name:  "no mechListMIC (Windows Kerberos optimistic path)",
			token: &ParsedToken{MechListBytes: []byte{0x30, 0x2e}},
			want:  false,
		},
		{
			name:  "empty mechListMIC",
			token: &ParsedToken{MechListBytes: []byte{0x30, 0x2e}, MechListMIC: []byte{}},
			want:  false,
		},
		{
			name:  "mechListMIC present",
			token: &ParsedToken{MechListBytes: []byte{0x30, 0x2e}, MechListMIC: []byte{0x04, 0x04, 0x01}},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.token.HasMechListMIC(); got != tt.want {
				t.Errorf("HasMechListMIC() = %v, want %v", got, tt.want)
			}
		})
	}
}
