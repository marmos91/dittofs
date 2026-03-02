// Package encryption provides AEAD encryption and decryption for SMB3 messages.
//
// SMB3 encryption wraps SMB2 messages in a Transform Header (protocol ID 0xFD 'S' 'M' 'B')
// followed by AEAD-encrypted ciphertext. The encryption uses either AES-GCM (preferred for
// SMB 3.1.1) or AES-CCM (required for SMB 3.0/3.0.2 compatibility), supporting both 128-bit
// and 256-bit key lengths.
//
// The package provides an Encryptor interface (mirroring the signing.Signer pattern) with
// GCMEncryptor and CCMEncryptor implementations. The NewEncryptor factory dispatches by cipher
// ID constant from the types package.
//
// # Key Direction Convention
//
// SMB key names use the CLIENT perspective:
//   - EncryptionKey = "ServerIn" = client-to-server key. Server uses this for DECRYPTION.
//   - DecryptionKey = "ServerOut" = server-to-client key. Server uses this for ENCRYPTION.
//
// # Cipher Selection
//
//   - AES-128-CCM: SMB 3.0/3.0.2 always use this (no cipher negotiation)
//   - AES-128-GCM: SMB 3.1.1 default, preferred over CCM for performance
//   - AES-256-CCM: SMB 3.1.1 with 256-bit negotiation
//   - AES-256-GCM: SMB 3.1.1 with 256-bit negotiation, highest priority
//
// # Nonce Generation
//
// Each Encrypt call generates a fresh random nonce via crypto/rand. GCM uses 12-byte nonces;
// CCM uses 11-byte nonces (per MS-SMB2 specification). Nonces MUST NOT be reused with the
// same key.
//
// Reference: [MS-SMB2] Section 3.1.4.3 (Encrypting the Message)
package encryption
