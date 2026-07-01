package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// BufferJSON is the { "type": "Buffer", "data": "<base64>" } format used in JS dumps.
// Fields can also be raw base64 strings for some Baileys fields.
type BufferJSON struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// Bytes decodes the BufferJSON to raw bytes.
func (b *BufferJSON) Bytes() ([]byte, error) {
	if b == nil || b.Data == "" {
		return nil, nil
	}
	dec, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil {
		// try RawStdEncoding
		dec, err = base64.RawStdEncoding.DecodeString(b.Data)
	}
	return dec, err
}

// flexBytes decodes a JSON value that is either a BufferJSON object or a plain base64 string.
func flexBytes(raw json.RawMessage, field string) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Try BufferJSON object first
	var buf BufferJSON
	if err := json.Unmarshal(raw, &buf); err == nil && buf.Type == "Buffer" {
		return buf.Bytes()
	}
	// Try plain base64 string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		dec, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			dec, err = base64.RawStdEncoding.DecodeString(s)
		}
		if err == nil {
			return dec, nil
		}
	}
	return nil, fmt.Errorf("%s: cannot decode as BufferJSON or base64 string", field)
}

// SessionCredentials holds the cryptographic material needed to restore a WhatsApp session.
type SessionCredentials struct {
	// Device identity
	JID            string // full device JID, e.g. 556781930335:54@c.us
	LID            string // linked device ID
	RegistrationID uint32
	Platform       string
	PushName       string

	// Cryptographic keys (raw bytes)
	NoiseKeyPriv []byte // 32 bytes – Curve25519 private key
	NoiseKeyPub  []byte // 32 bytes – Curve25519 public key
	IdentKeyPriv []byte // 32 bytes – Curve25519 private key
	IdentKeyPub  []byte // 32 bytes – Curve25519 public key

	// Signed pre-key
	SignedPreKeyPriv []byte // 32 bytes
	SignedPreKeyPub  []byte // 32 bytes
	SignedPreKeySig  []byte // 64 bytes
	SignedPreKeyID   uint32

	// ADV (advanced verification)
	AdvKey           []byte // 32 bytes
	AdvDetails       []byte // protobuf DeviceIdentityDetails
	AdvAccountSig    []byte // 64 bytes
	AdvAccountSigKey []byte // 33 bytes
	AdvDeviceSig     []byte // 64 bytes

	// Pre-keys: keyId → private key bytes
	PreKeys map[uint32][]byte

	// App state sync keys: keyId bytes → keyData bytes
	AppStateSyncKeys []AppStateSyncKeyEntry

	// App state versions
	AppStateVersions []AppStateVersionEntry

	// Signal sessions: address → serialized session bytes
	Sessions map[string][]byte

	// Sender keys: "groupId@@senderId" → serialized sender key bytes
	SenderKeys map[string][]byte

	// Identity keys: address → 33-byte public key
	IdentityKeys map[string][]byte
}

type AppStateSyncKeyEntry struct {
	KeyID     []byte
	KeyData   []byte
	Timestamp int64
}

type AppStateVersionEntry struct {
	Collection    string
	Version       uint64
	Hash          []byte
	IndexValueMap map[string][]byte
}

// importFormat identifies the source format of a dump.
type importFormat string

const (
	formatWaWeb   importFormat = "wa-web"
	formatBaileys importFormat = "baileys"
	formatUnknown importFormat = "unknown"
)

// importStatus tracks the current status of an import operation (per user).
type importStatus struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	JID     string `json:"jid,omitempty"`
}
