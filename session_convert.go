package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// WA-Web dump format
// ---------------------------------------------------------------------------

type waWebKeyPair struct {
	PubKey  json.RawMessage `json:"pubKey"`
	PrivKey json.RawMessage `json:"privKey"`
}

type waWebSignedPreKey struct {
	KeyID   uint32          `json:"keyId"`
	KeyPair waWebKeyPair    `json:"keyPair"`
	Sig     json.RawMessage `json:"signature"`
}

type waWebAccount struct {
	Details       json.RawMessage `json:"details"`
	AccountSigKey json.RawMessage `json:"accountSignatureKey"`
	AccountSig    json.RawMessage `json:"accountSignature"`
	DeviceSig     json.RawMessage `json:"deviceSignature"`
}

type waWebDevice struct {
	RegistrationID uint32            `json:"registrationId"`
	NoiseKey       waWebKeyPair      `json:"noiseKey"`
	IdentityKey    waWebKeyPair      `json:"identityKey"`
	SignedPreKey   waWebSignedPreKey `json:"signedPreKey"`
	AdvSecretKey   json.RawMessage   `json:"advSecretKey"`
	Account        waWebAccount      `json:"account"`
	MeJID          string            `json:"meJid"`
	MeLID          string            `json:"meLid"`
	MeDisplayName  *string           `json:"meDisplayName"`
	Platform       string            `json:"platform"`
}

type waWebAppStateSyncKey struct {
	KeyID       json.RawMessage        `json:"keyId"`
	KeyData     json.RawMessage        `json:"keyData"`
	Timestamp   int64                  `json:"timestamp"`
	Fingerprint map[string]interface{} `json:"fingerprint"`
}

type waWebAppStateVersion struct {
	Collection    string                     `json:"collection"`
	Version       uint64                     `json:"version"`
	Hash          json.RawMessage            `json:"hash"`
	IndexValueMap map[string]json.RawMessage `json:"indexValueMap"`
}

type waWebPreKey struct {
	KeyID   uint32       `json:"keyId"`
	KeyPair waWebKeyPair `json:"keyPair"`
}

type waWebDump struct {
	Device           waWebDevice            `json:"device"`
	AppStateSyncKeys []waWebAppStateSyncKey `json:"appStateSyncKeys"`
	AppStateVersions []waWebAppStateVersion `json:"appStateVersions"`
	PreKeys          []waWebPreKey          `json:"preKeys"`
}

// ---------------------------------------------------------------------------
// Baileys creds.json format
// ---------------------------------------------------------------------------

type baileysKeyPair struct {
	Public  json.RawMessage `json:"public"`
	Private json.RawMessage `json:"private"`
}

type baileysSignedPreKey struct {
	KeyPair   baileysKeyPair  `json:"keyPair"`
	Signature json.RawMessage `json:"signature"`
	KeyID     uint32          `json:"keyId"`
}

type baileysAccount struct {
	Details       json.RawMessage `json:"details"`
	AccountSigKey json.RawMessage `json:"accountSignatureKey"`
	AccountSig    json.RawMessage `json:"accountSignature"`
	DeviceSig     json.RawMessage `json:"deviceSignature"`
}

type baileysMe struct {
	ID   string `json:"id"`
	LID  string `json:"lid"`
	Name string `json:"name"`
}

type baileysAppStateSyncKeyData struct {
	KeyData     json.RawMessage `json:"keyData"`
	Fingerprint interface{}     `json:"fingerprint"`
	Timestamp   interface{}     `json:"timestamp"`
}

type baileysLTHashState struct {
	Version       uint64                     `json:"version"`
	Hash          json.RawMessage            `json:"hash"`
	IndexValueMap map[string]json.RawMessage `json:"indexValueMap"`
}

type baileysCreds struct {
	NoiseKey          baileysKeyPair      `json:"noiseKey"`
	SignedIdentityKey baileysKeyPair      `json:"signedIdentityKey"`
	SignedPreKey      baileysSignedPreKey `json:"signedPreKey"`
	RegistrationID    uint32              `json:"registrationId"`
	AdvSecretKey      string              `json:"advSecretKey"` // plain base64 string
	Me                *baileysMe          `json:"me"`
	Account           *baileysAccount     `json:"account"`
	Platform          string              `json:"platform"`
}

// ---------------------------------------------------------------------------
// Format detection
// ---------------------------------------------------------------------------

func detectFormat(data []byte) importFormat {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return formatUnknown
	}
	// Full wa-web dump: has "device" top-level key
	if _, ok := m["device"]; ok {
		return formatWaWeb
	}
	// Bare wa-web device object: has "identityKey" or "meJid" (wa-web uses privKey/pubKey fields)
	// Baileys uses "signedIdentityKey" (not "identityKey") and "me" (not "meJid")
	if _, ok := m["identityKey"]; ok {
		return formatWaWeb
	}
	if _, ok := m["meJid"]; ok {
		return formatWaWeb
	}
	// Baileys creds have "signedIdentityKey" or "noiseKey" with public/private fields
	if _, ok := m["signedIdentityKey"]; ok {
		return formatBaileys
	}
	if _, ok := m["noiseKey"]; ok {
		return formatBaileys
	}
	return formatUnknown
}

// ---------------------------------------------------------------------------
// wa-web → SessionCredentials
// ---------------------------------------------------------------------------

func convertWaWebDump(data []byte) (*SessionCredentials, error) {
	var dump waWebDump
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, fmt.Errorf("wa-web dump: %w", err)
	}

	// If "device" wrapper is absent, the data IS the device object directly
	if dump.Device.MeJID == "" && dump.Device.RegistrationID == 0 {
		var dev waWebDevice
		if err := json.Unmarshal(data, &dev); err != nil {
			return nil, fmt.Errorf("wa-web device: %w", err)
		}
		dump.Device = dev
	}

	d := dump.Device

	noisePriv, err := flexBytes(d.NoiseKey.PrivKey, "noiseKey.privKey")
	if err != nil {
		return nil, err
	}
	noisePub, err := flexBytes(d.NoiseKey.PubKey, "noiseKey.pubKey")
	if err != nil {
		return nil, err
	}
	identPriv, err := flexBytes(d.IdentityKey.PrivKey, "identityKey.privKey")
	if err != nil {
		return nil, err
	}
	identPub, err := flexBytes(d.IdentityKey.PubKey, "identityKey.pubKey")
	if err != nil {
		return nil, err
	}
	spkPriv, err := flexBytes(d.SignedPreKey.KeyPair.PrivKey, "signedPreKey.keyPair.privKey")
	if err != nil {
		return nil, err
	}
	spkPub, err := flexBytes(d.SignedPreKey.KeyPair.PubKey, "signedPreKey.keyPair.pubKey")
	if err != nil {
		return nil, err
	}
	spkSig, err := flexBytes(d.SignedPreKey.Sig, "signedPreKey.signature")
	if err != nil {
		return nil, err
	}
	advKey, err := flexBytes(d.AdvSecretKey, "advSecretKey")
	if err != nil {
		return nil, err
	}
	advDetails, err := flexBytes(d.Account.Details, "account.details")
	if err != nil {
		return nil, err
	}
	advAccountSig, err := flexBytes(d.Account.AccountSig, "account.accountSignature")
	if err != nil {
		return nil, err
	}
	advAccountSigKey, err := flexBytes(d.Account.AccountSigKey, "account.accountSignatureKey")
	if err != nil {
		return nil, err
	}
	advDeviceSig, err := flexBytes(d.Account.DeviceSig, "account.deviceSignature")
	if err != nil {
		return nil, err
	}

	if len(noisePriv) == 0 {
		return nil, errors.New("wa-web dump: noiseKey.privKey is empty – noise handshake not possible")
	}

	// Pre-keys
	preKeys := make(map[uint32][]byte, len(dump.PreKeys))
	for _, pk := range dump.PreKeys {
		privBytes, err := flexBytes(pk.KeyPair.PrivKey, fmt.Sprintf("preKey[%d].privKey", pk.KeyID))
		if err != nil || len(privBytes) == 0 {
			continue
		}
		preKeys[pk.KeyID] = privBytes
	}

	// App state sync keys
	appStateKeys := make([]AppStateSyncKeyEntry, 0, len(dump.AppStateSyncKeys))
	for _, k := range dump.AppStateSyncKeys {
		kid, err := flexBytes(k.KeyID, "appStateSyncKey.keyId")
		if err != nil || len(kid) == 0 {
			continue
		}
		kdata, err := flexBytes(k.KeyData, "appStateSyncKey.keyData")
		if err != nil || len(kdata) == 0 {
			continue
		}
		appStateKeys = append(appStateKeys, AppStateSyncKeyEntry{
			KeyID:     kid,
			KeyData:   kdata,
			Timestamp: k.Timestamp,
		})
	}

	// App state versions
	appStateVersions := make([]AppStateVersionEntry, 0, len(dump.AppStateVersions))
	for _, v := range dump.AppStateVersions {
		hashBytes, err := flexBytes(v.Hash, fmt.Sprintf("appStateVersion[%s].hash", v.Collection))
		if err != nil {
			continue
		}
		ivm := make(map[string][]byte, len(v.IndexValueMap))
		for k, raw := range v.IndexValueMap {
			b, err := flexBytes(raw, fmt.Sprintf("indexValueMap[%s]", k))
			if err == nil && len(b) > 0 {
				ivm[k] = b
			}
		}
		appStateVersions = append(appStateVersions, AppStateVersionEntry{
			Collection:    v.Collection,
			Version:       v.Version,
			Hash:          hashBytes,
			IndexValueMap: ivm,
		})
	}

	pushName := ""
	if d.MeDisplayName != nil {
		pushName = *d.MeDisplayName
	}

	return &SessionCredentials{
		JID:              d.MeJID,
		LID:              d.MeLID,
		RegistrationID:   d.RegistrationID,
		Platform:         d.Platform,
		PushName:         pushName,
		NoiseKeyPriv:     noisePriv,
		NoiseKeyPub:      noisePub,
		IdentKeyPriv:     identPriv,
		IdentKeyPub:      identPub,
		SignedPreKeyPriv: spkPriv,
		SignedPreKeyPub:  spkPub,
		SignedPreKeySig:  spkSig,
		SignedPreKeyID:   d.SignedPreKey.KeyID,
		AdvKey:           advKey,
		AdvDetails:       advDetails,
		AdvAccountSig:    advAccountSig,
		AdvAccountSigKey: advAccountSigKey,
		AdvDeviceSig:     advDeviceSig,
		PreKeys:          preKeys,
		AppStateSyncKeys: appStateKeys,
		AppStateVersions: appStateVersions,
		Sessions:         map[string][]byte{},
		SenderKeys:       map[string][]byte{},
		IdentityKeys:     map[string][]byte{},
	}, nil
}

// ---------------------------------------------------------------------------
// Baileys JSON → SessionCredentials
// ---------------------------------------------------------------------------

func convertBaileysJSON(credsData []byte, keysData map[string]json.RawMessage) (*SessionCredentials, error) {
	var creds baileysCreds
	if err := json.Unmarshal(credsData, &creds); err != nil {
		return nil, fmt.Errorf("baileys creds: %w", err)
	}

	noisePriv, err := flexBytes(creds.NoiseKey.Private, "noiseKey.private")
	if err != nil {
		return nil, err
	}
	noisePub, err := flexBytes(creds.NoiseKey.Public, "noiseKey.public")
	if err != nil {
		return nil, err
	}
	identPriv, err := flexBytes(creds.SignedIdentityKey.Private, "signedIdentityKey.private")
	if err != nil {
		return nil, err
	}
	identPub, err := flexBytes(creds.SignedIdentityKey.Public, "signedIdentityKey.public")
	if err != nil {
		return nil, err
	}
	spkPriv, err := flexBytes(creds.SignedPreKey.KeyPair.Private, "signedPreKey.keyPair.private")
	if err != nil {
		return nil, err
	}
	spkPub, err := flexBytes(creds.SignedPreKey.KeyPair.Public, "signedPreKey.keyPair.public")
	if err != nil {
		return nil, err
	}
	spkSig, err := flexBytes(creds.SignedPreKey.Signature, "signedPreKey.signature")
	if err != nil {
		return nil, err
	}

	var advKey []byte
	if creds.AdvSecretKey != "" {
		advKey, err = base64.StdEncoding.DecodeString(creds.AdvSecretKey)
		if err != nil {
			advKey, err = base64.RawStdEncoding.DecodeString(creds.AdvSecretKey)
		}
		if err != nil {
			return nil, fmt.Errorf("advSecretKey: %w", err)
		}
	}

	var advDetails, advAccountSig, advAccountSigKey, advDeviceSig []byte
	if creds.Account != nil {
		advDetails, _ = flexBytes(creds.Account.Details, "account.details")
		advAccountSig, _ = flexBytes(creds.Account.AccountSig, "account.accountSignature")
		advAccountSigKey, _ = flexBytes(creds.Account.AccountSigKey, "account.accountSignatureKey")
		advDeviceSig, _ = flexBytes(creds.Account.DeviceSig, "account.deviceSignature")
	}

	meJID := ""
	meLID := ""
	pushName := ""
	if creds.Me != nil {
		meJID = creds.Me.ID
		meLID = creds.Me.LID
		pushName = creds.Me.Name
	}

	if len(noisePriv) == 0 {
		return nil, errors.New("baileys creds: noiseKey.private is empty")
	}

	// Parse signal keys from the keys map
	preKeys := make(map[uint32][]byte)
	appStateSyncKeys := []AppStateSyncKeyEntry{}
	appStateVersions := []AppStateVersionEntry{}
	identityKeys := map[string][]byte{}

	if keysData != nil {
		// Pre-keys: "pre-key" → map of keyId → { public, private }
		if raw, ok := keysData["pre-key"]; ok {
			var pkMap map[string]baileysKeyPair
			if err := json.Unmarshal(raw, &pkMap); err == nil {
				for idStr, kp := range pkMap {
					var id uint32
					fmt.Sscanf(idStr, "%d", &id)
					privBytes, err := flexBytes(kp.Private, fmt.Sprintf("pre-key[%s].private", idStr))
					if err == nil && len(privBytes) > 0 {
						preKeys[id] = privBytes
					}
				}
			}
		}

		// App state sync keys
		if raw, ok := keysData["app-state-sync-key"]; ok {
			var askMap map[string]baileysAppStateSyncKeyData
			if err := json.Unmarshal(raw, &askMap); err == nil {
				for keyIDB64, v := range askMap {
					kid, err := base64.StdEncoding.DecodeString(keyIDB64)
					if err != nil {
						kid, err = base64.RawStdEncoding.DecodeString(keyIDB64)
					}
					if err != nil || len(kid) == 0 {
						continue
					}
					kdata, err := flexBytes(v.KeyData, fmt.Sprintf("app-state-sync-key[%s].keyData", keyIDB64))
					if err != nil || len(kdata) == 0 {
						continue
					}
					var ts int64
					switch t := v.Timestamp.(type) {
					case float64:
						ts = int64(t)
					case string:
						fmt.Sscanf(t, "%d", &ts)
					}
					appStateSyncKeys = append(appStateSyncKeys, AppStateSyncKeyEntry{
						KeyID:     kid,
						KeyData:   kdata,
						Timestamp: ts,
					})
				}
			}
		}

		// App state versions
		if raw, ok := keysData["app-state-sync-version"]; ok {
			var asvMap map[string]baileysLTHashState
			if err := json.Unmarshal(raw, &asvMap); err == nil {
				for collection, v := range asvMap {
					hashBytes, err := flexBytes(v.Hash, fmt.Sprintf("app-state-sync-version[%s].hash", collection))
					if err != nil {
						continue
					}
					ivm := make(map[string][]byte)
					for k, raw := range v.IndexValueMap {
						// IndexValueMap values in Baileys are { valueMac: BufferJSON }
						var entry struct {
							ValueMac json.RawMessage `json:"valueMac"`
						}
						if err := json.Unmarshal(raw, &entry); err == nil {
							b, err := flexBytes(entry.ValueMac, fmt.Sprintf("indexValueMap[%s].valueMac", k))
							if err == nil && len(b) > 0 {
								ivm[k] = b
							}
						}
					}
					appStateVersions = append(appStateVersions, AppStateVersionEntry{
						Collection:    collection,
						Version:       v.Version,
						Hash:          hashBytes,
						IndexValueMap: ivm,
					})
				}
			}
		}

		// Identity keys
		if raw, ok := keysData["identity-key"]; ok {
			var ikMap map[string]json.RawMessage
			if err := json.Unmarshal(raw, &ikMap); err == nil {
				for addr, raw := range ikMap {
					b, err := flexBytes(raw, fmt.Sprintf("identity-key[%s]", addr))
					if err == nil && len(b) > 0 {
						identityKeys[addr] = b
					}
				}
			}
		}
	}

	return &SessionCredentials{
		JID:              meJID,
		LID:              meLID,
		RegistrationID:   creds.RegistrationID,
		Platform:         creds.Platform,
		PushName:         pushName,
		NoiseKeyPriv:     noisePriv,
		NoiseKeyPub:      noisePub,
		IdentKeyPriv:     identPriv,
		IdentKeyPub:      identPub,
		SignedPreKeyPriv: spkPriv,
		SignedPreKeyPub:  spkPub,
		SignedPreKeySig:  spkSig,
		SignedPreKeyID:   creds.SignedPreKey.KeyID,
		AdvKey:           advKey,
		AdvDetails:       advDetails,
		AdvAccountSig:    advAccountSig,
		AdvAccountSigKey: advAccountSigKey,
		AdvDeviceSig:     advDeviceSig,
		PreKeys:          preKeys,
		AppStateSyncKeys: appStateSyncKeys,
		AppStateVersions: appStateVersions,
		Sessions:         map[string][]byte{},
		SenderKeys:       map[string][]byte{},
		IdentityKeys:     identityKeys,
	}, nil
}

// ---------------------------------------------------------------------------
// ZIP extraction
// ---------------------------------------------------------------------------

// extractZip reads zip content and returns a map of filename → bytes.
func extractZip(data []byte) (map[string][]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}
	files := make(map[string][]byte)
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("zip entry %s read: %w", f.Name, err)
		}
		// Use base filename as key
		name := f.Name
		if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
			name = name[idx+1:]
		}
		files[name] = content
	}
	return files, nil
}

// ---------------------------------------------------------------------------
// Top-level import entry point
// ---------------------------------------------------------------------------

// parseImportFile auto-detects format and returns SessionCredentials.
// data is the raw file bytes (JSON or ZIP).
func parseImportFile(data []byte, filename string) (*SessionCredentials, error) {
	// If ZIP, extract and find a recognizable JSON file
	if isZipData(data) || strings.HasSuffix(strings.ToLower(filename), ".zip") {
		files, err := extractZip(data)
		if err != nil {
			return nil, err
		}
		return parseImportFromZip(files)
	}
	return parseImportJSON(data)
}

func isZipData(data []byte) bool {
	return len(data) >= 4 && data[0] == 0x50 && data[1] == 0x4B && data[2] == 0x03 && data[3] == 0x04
}

func parseImportFromZip(files map[string][]byte) (*SessionCredentials, error) {
	// Look for wa-web-dump.json
	if data, ok := findFile(files, "wa-web-dump.json", "wa-web-dump"); ok {
		return convertWaWebDump(data)
	}

	// Look for Baileys creds.json
	credsData, hasCreds := findFile(files, "creds.json", "creds")
	if !hasCreds {
		return nil, errors.New("zip: no recognizable session file found (expected creds.json or wa-web-dump.json)")
	}

	// Build keys map from sibling files
	keysData := buildBaileysKeysFromZip(files)
	return convertBaileysJSON(credsData, keysData)
}

func findFile(files map[string][]byte, names ...string) ([]byte, bool) {
	for _, name := range names {
		if data, ok := files[name]; ok {
			return data, true
		}
		// case-insensitive search
		lower := strings.ToLower(name)
		for k, v := range files {
			if strings.ToLower(k) == lower {
				return v, true
			}
		}
	}
	return nil, false
}

// buildBaileysKeysFromZip reconstructs the Baileys keys data structure from multi-file layout.
// In Baileys multi-file auth state, key files are named like "pre-key-123.json".
func buildBaileysKeysFromZip(files map[string][]byte) map[string]json.RawMessage {
	preKeys := map[string]json.RawMessage{}
	appStateSyncKeys := map[string]json.RawMessage{}
	appStateSyncVersions := map[string]json.RawMessage{}
	identityKeys := map[string]json.RawMessage{}
	senderKeys := map[string]json.RawMessage{}
	sessions := map[string]json.RawMessage{}

	for name, data := range files {
		if name == "creds.json" {
			continue
		}
		lower := strings.ToLower(name)
		lower = strings.TrimSuffix(lower, ".json")

		switch {
		case strings.HasPrefix(lower, "pre-key-"):
			id := strings.TrimPrefix(lower, "pre-key-")
			id = strings.ReplaceAll(id, "--", ":")
			// Parse key pair from file
			var kp baileysKeyPair
			if err := json.Unmarshal(data, &kp); err == nil {
				preKeys[id] = data
			}
		case strings.HasPrefix(lower, "app-state-sync-key-"):
			kid := strings.TrimPrefix(lower, "app-state-sync-key-")
			kid = strings.ReplaceAll(kid, "--", "/")
			kid = strings.ReplaceAll(kid, "__", "/")
			appStateSyncKeys[kid] = data
		case strings.HasPrefix(lower, "app-state-sync-version-"):
			col := strings.TrimPrefix(lower, "app-state-sync-version-")
			appStateSyncVersions[col] = data
		case strings.HasPrefix(lower, "identity-key-"):
			addr := strings.TrimPrefix(lower, "identity-key-")
			addr = strings.ReplaceAll(addr, "--", ":")
			identityKeys[addr] = data
		case strings.HasPrefix(lower, "sender-key-"):
			key := strings.TrimPrefix(lower, "sender-key-")
			key = strings.ReplaceAll(key, "--", ":")
			senderKeys[key] = data
		case strings.HasPrefix(lower, "session-"):
			addr := strings.TrimPrefix(lower, "session-")
			addr = strings.ReplaceAll(addr, "--", ":")
			sessions[addr] = data
		}
	}

	result := map[string]json.RawMessage{}
	if len(preKeys) > 0 {
		if raw, err := json.Marshal(preKeys); err == nil {
			result["pre-key"] = raw
		}
	}
	if len(appStateSyncKeys) > 0 {
		if raw, err := json.Marshal(appStateSyncKeys); err == nil {
			result["app-state-sync-key"] = raw
		}
	}
	if len(appStateSyncVersions) > 0 {
		if raw, err := json.Marshal(appStateSyncVersions); err == nil {
			result["app-state-sync-version"] = raw
		}
	}
	if len(identityKeys) > 0 {
		if raw, err := json.Marshal(identityKeys); err == nil {
			result["identity-key"] = raw
		}
	}
	if len(senderKeys) > 0 {
		if raw, err := json.Marshal(senderKeys); err == nil {
			result["sender-key"] = raw
		}
	}
	if len(sessions) > 0 {
		if raw, err := json.Marshal(sessions); err == nil {
			result["session"] = raw
		}
	}
	return result
}

func parseImportJSON(data []byte) (*SessionCredentials, error) {
	switch detectFormat(data) {
	case formatWaWeb:
		return convertWaWebDump(data)
	case formatBaileys:
		// Single JSON file: treat as creds only (no keys)
		return convertBaileysJSON(data, nil)
	default:
		return nil, errors.New("unrecognized session format: expected wa-web dump or Baileys creds JSON")
	}
}
