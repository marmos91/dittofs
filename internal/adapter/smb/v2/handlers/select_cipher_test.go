package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestSelectCipher covers the NEGOTIATE cipher-selection path that decides
// whether the server accepts AES-256. selectCipher honours the client's
// preference order against the server's allowed set, so a client that lists
// AES-256 first (and the server allows it) must end up on AES-256.
func TestSelectCipher(t *testing.T) {
	tests := []struct {
		name          string
		allowedConfig []uint16 // nil = use built-in default (all four, AES-256 first)
		clientCiphers []uint16
		want          uint16
	}{
		{
			name:          "default allowed, client prefers AES-256-GCM",
			allowedConfig: nil,
			clientCiphers: []uint16{types.CipherAES256GCM, types.CipherAES256CCM, types.CipherAES128GCM, types.CipherAES128CCM},
			want:          types.CipherAES256GCM,
		},
		{
			name:          "default allowed, client prefers AES-256-CCM",
			allowedConfig: nil,
			clientCiphers: []uint16{types.CipherAES256CCM, types.CipherAES128GCM},
			want:          types.CipherAES256CCM,
		},
		{
			name:          "default allowed, client prefers AES-128 first",
			allowedConfig: nil,
			clientCiphers: []uint16{types.CipherAES128GCM, types.CipherAES256GCM},
			want:          types.CipherAES128GCM,
		},
		{
			name:          "default allowed, client offers only AES-256-GCM",
			allowedConfig: nil,
			clientCiphers: []uint16{types.CipherAES256GCM},
			want:          types.CipherAES256GCM,
		},
		{
			name:          "server restricted to AES-128, client offers AES-256 first",
			allowedConfig: []uint16{types.CipherAES128GCM, types.CipherAES128CCM},
			clientCiphers: []uint16{types.CipherAES256GCM, types.CipherAES128GCM},
			want:          types.CipherAES128GCM,
		},
		{
			name:          "no mutual cipher",
			allowedConfig: []uint16{types.CipherAES128CCM},
			clientCiphers: []uint16{types.CipherAES256GCM, types.CipherAES256CCM},
			want:          0,
		},
		{
			name:          "empty client list",
			allowedConfig: nil,
			clientCiphers: nil,
			want:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler()
			h.EncryptionConfig.AllowedCiphers = tt.allowedConfig

			got := h.selectCipher(tt.clientCiphers)
			if got != tt.want {
				t.Errorf("selectCipher(%v) = 0x%04x, want 0x%04x", tt.clientCiphers, got, tt.want)
			}
		})
	}
}
