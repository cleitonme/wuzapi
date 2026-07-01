package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	maxImportFileSize = 50 * 1024 * 1024 // 50 MB
)

// importStatusStore holds per-user import state in memory.
var importStatusStore = struct {
	sync.RWMutex
	statuses map[string]importStatus
}{statuses: make(map[string]importStatus)}

func setImportStatus(userID string, status importStatus) {
	importStatusStore.Lock()
	importStatusStore.statuses[userID] = status
	importStatusStore.Unlock()
}

func getImportStatus(userID string) (importStatus, bool) {
	importStatusStore.RLock()
	s, ok := importStatusStore.statuses[userID]
	importStatusStore.RUnlock()
	return s, ok
}

// ---------------------------------------------------------------------------
// POST /session/import  (multipart/form-data)
// Fields: file (json|zip)
// ---------------------------------------------------------------------------

func (s *server) ImportSession() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		// Check if import already running
		if st, ok := getImportStatus(txtid); ok && st.Status == "importing" {
			s.Respond(w, r, http.StatusConflict, errors.New("import already in progress"))
			return
		}

		// Parse multipart
		if err := r.ParseMultipartForm(maxImportFileSize); err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid multipart form: %w", err))
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("missing 'file' field: %w", err))
			return
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, maxImportFileSize))
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("reading file: %w", err))
			return
		}

		setImportStatus(txtid, importStatus{Status: "importing", Message: "Detecting format..."})

		creds, err := parseImportFile(data, header.Filename)
		if err != nil {
			setImportStatus(txtid, importStatus{Status: "error", Message: err.Error()})
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("parse error: %w", err))
			return
		}

		if err := validateCredentials(creds); err != nil {
			setImportStatus(txtid, importStatus{Status: "error", Message: err.Error()})
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid credentials: %w", err))
			return
		}

		setImportStatus(txtid, importStatus{Status: "importing", Message: "Writing session..."})

		if err := s.applyImport(r, txtid, token, creds); err != nil {
			setImportStatus(txtid, importStatus{Status: "error", Message: err.Error()})
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		setImportStatus(txtid, importStatus{Status: "connecting", Message: "Session written. Connecting...", JID: creds.JID})

		s.Respond(w, r, http.StatusOK, fmt.Sprintf(`{"success":true,"jid":%q,"message":"Session imported successfully. Connection starting."}`, creds.JID))
	}
}

// ---------------------------------------------------------------------------
// POST /session/importRaw
// Body: { "creds": {...}, "keys": {...} }
// ---------------------------------------------------------------------------

func (s *server) ImportRawSession() http.HandlerFunc {
	type rawRequest struct {
		Creds json.RawMessage            `json:"creds"`
		Keys  map[string]json.RawMessage `json:"keys"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		if st, ok := getImportStatus(txtid); ok && st.Status == "importing" {
			s.Respond(w, r, http.StatusConflict, errors.New("import already in progress"))
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxImportFileSize))
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("reading body: %w", err))
			return
		}

		setImportStatus(txtid, importStatus{Status: "importing", Message: "Converting credentials..."})

		var creds *SessionCredentials

		// Accept wa-web dump format ({device,...}) directly, in addition to Baileys {creds,keys}
		if detectFormat(body) == formatWaWeb {
			creds, err = convertWaWebDump(body)
			if err != nil {
				setImportStatus(txtid, importStatus{Status: "error", Message: err.Error()})
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("conversion error: %w", err))
				return
			}
		} else {
			var req rawRequest
			if err := json.Unmarshal(body, &req); err != nil {
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
				return
			}

			if len(req.Creds) == 0 {
				s.Respond(w, r, http.StatusBadRequest, errors.New("missing 'creds' field (or unrecognized format)"))
				return
			}

			// creds field may itself be a wa-web dump (e.g. {"creds":{device:...}})
			if detectFormat(req.Creds) == formatWaWeb {
				creds, err = convertWaWebDump(req.Creds)
			} else {
				creds, err = convertBaileysJSON(req.Creds, req.Keys)
			}
			if err != nil {
				setImportStatus(txtid, importStatus{Status: "error", Message: err.Error()})
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("conversion error: %w", err))
				return
			}
		}

		if err := validateCredentials(creds); err != nil {
			setImportStatus(txtid, importStatus{Status: "error", Message: err.Error()})
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid credentials: %w", err))
			return
		}

		setImportStatus(txtid, importStatus{Status: "importing", Message: "Writing session..."})

		if err := s.applyImport(r, txtid, token, creds); err != nil {
			setImportStatus(txtid, importStatus{Status: "error", Message: err.Error()})
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		setImportStatus(txtid, importStatus{Status: "connecting", Message: "Session written. Connecting...", JID: creds.JID})

		s.Respond(w, r, http.StatusOK, fmt.Sprintf(`{"success":true,"jid":%q,"message":"Session imported successfully. Connection starting."}`, creds.JID))
	}
}

// ---------------------------------------------------------------------------
// GET /session/export?format=json|zip
// ---------------------------------------------------------------------------

func (s *server) ExportSession() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		jid := r.Context().Value("userinfo").(Values).Get("Jid")

		format := r.URL.Query().Get("format")
		if format == "" {
			format = "json"
		}

		// Read device credentials from whatsmeow DB
		db := whatsmeowStoreDB
		if db == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("whatsmeow store not available"))
			return
		}

		if jid == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("no session: user has no JID"))
			return
		}

		deviceJID := normalizeJID(jid)
		isPostgres := isPostgresDB(db)
		p1 := ph(isPostgres, 1)

		var row struct {
			RegistrationID   int    `db:"registration_id"`
			NoiseKey         []byte `db:"noise_key"`
			IdentityKey      []byte `db:"identity_key"`
			SignedPreKey     []byte `db:"signed_pre_key"`
			SignedPreKeyID   int    `db:"signed_pre_key_id"`
			SignedPreKeySig  []byte `db:"signed_pre_key_sig"`
			AdvKey           []byte `db:"adv_key"`
			AdvDetails       []byte `db:"adv_details"`
			AdvAccountSig    []byte `db:"adv_account_sig"`
			AdvAccountSigKey []byte `db:"adv_account_sig_key"`
			AdvDeviceSig     []byte `db:"adv_device_sig"`
			Platform         string `db:"platform"`
			PushName         string `db:"push_name"`
			LID              string `db:"lid"`
		}

		q := fmt.Sprintf(`SELECT registration_id, noise_key, identity_key, signed_pre_key,
			signed_pre_key_id, signed_pre_key_sig, adv_key, adv_details,
			adv_account_sig, adv_account_sig_key, adv_device_sig, platform, push_name, lid
			FROM whatsmeow_device WHERE jid = %s`, p1)

		if err := db.QueryRowx(q, deviceJID).StructScan(&row); err != nil {
			s.Respond(w, r, http.StatusNotFound, fmt.Errorf("device not found: %w", err))
			return
		}

		// Build export in Baileys format
		export := buildBaileysExport(deviceJID, row.RegistrationID, row.NoiseKey, row.IdentityKey,
			row.SignedPreKey, row.SignedPreKeyID, row.SignedPreKeySig,
			row.AdvKey, row.AdvDetails, row.AdvAccountSig, row.AdvAccountSigKey, row.AdvDeviceSig,
			row.Platform, row.PushName, row.LID,
		)

		credsJSON, err := json.MarshalIndent(export, "", "  ")
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		switch format {
		case "zip":
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			f, err := zw.Create("creds.json")
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, err)
				return
			}
			f.Write(credsJSON)
			zw.Close()
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="session-%s.zip"`, txtid))
			w.Write(buf.Bytes())
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="creds-%s.json"`, txtid))
			w.Write(credsJSON)
		}
	}
}

// buildBaileysExport constructs the Baileys creds.json structure from raw whatsmeow DB values.
func buildBaileysExport(jid string, regID int, noisePriv, identPriv,
	spkPriv []byte, spkID int, spkSig,
	advKey, advDetails, advAccountSig, advAccountSigKey, advDeviceSig []byte,
	platform, pushName, lid string,
) map[string]interface{} {
	toBufferJSON := func(b []byte) map[string]interface{} {
		if len(b) == 0 {
			return map[string]interface{}{"type": "Buffer", "data": []byte{}}
		}
		data := make([]int, len(b))
		for i, v := range b {
			data[i] = int(v)
		}
		return map[string]interface{}{"type": "Buffer", "data": data}
	}

	return map[string]interface{}{
		"noiseKey": map[string]interface{}{
			"private": toBufferJSON(noisePriv),
			"public":  toBufferJSON(nil), // public not stored, derive if needed
		},
		"signedIdentityKey": map[string]interface{}{
			"private": toBufferJSON(identPriv),
			"public":  toBufferJSON(nil),
		},
		"signedPreKey": map[string]interface{}{
			"keyPair": map[string]interface{}{
				"private": toBufferJSON(spkPriv),
				"public":  toBufferJSON(nil),
			},
			"signature": toBufferJSON(spkSig),
			"keyId":     spkID,
		},
		"registrationId": regID,
		"advSecretKey":   advKey,
		"me": map[string]interface{}{
			"id":  jid,
			"lid": lid,
		},
		"account": map[string]interface{}{
			"details":             toBufferJSON(advDetails),
			"accountSignature":    toBufferJSON(advAccountSig),
			"accountSignatureKey": toBufferJSON(advAccountSigKey),
			"deviceSignature":     toBufferJSON(advDeviceSig),
		},
		"platform": platform,
	}
}

// ---------------------------------------------------------------------------
// GET /session/importStatus
// ---------------------------------------------------------------------------

func (s *server) GetImportStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		jid := r.Context().Value("userinfo").(Values).Get("Jid")

		var connStatus string
		client := clientManager.GetWhatsmeowClient(txtid)
		if client == nil {
			connStatus = "disconnected"
		} else if client.IsConnected() {
			connStatus = "connected"
		} else {
			connStatus = "connecting"
		}

		impSt, _ := getImportStatus(txtid)

		s.Respond(w, r, http.StatusOK, fmt.Sprintf(
			`{"jid":%q,"connectionStatus":%q,"importStatus":%q,"importMessage":%q}`,
			jid, connStatus, impSt.Status, impSt.Message,
		))
	}
}

// ---------------------------------------------------------------------------
// Core import logic
// ---------------------------------------------------------------------------

func (s *server) applyImport(r *http.Request, userID, token string, creds *SessionCredentials) error {
	log.Info().Str("userID", userID).Str("jid", creds.JID).Msg("Applying session import")

	// 1. Stop current session
	stopUserSession(userID)

	// Give whatsmeow a moment to fully disconnect
	time.Sleep(500 * time.Millisecond)

	// 2. Write to whatsmeow DB
	db := whatsmeowStoreDB
	if db == nil {
		return errors.New("whatsmeow store database not initialized")
	}

	if err := writeSessionToWhatsmeow(db, creds); err != nil {
		return fmt.Errorf("writing session: %w", err)
	}

	// 3. Restart session with the new JID
	jid := creds.JID
	if err := s.restartUserSessionAfterImport(r.Context(), userID, jid, token); err != nil {
		return fmt.Errorf("restarting session: %w", err)
	}

	log.Info().Str("userID", userID).Str("jid", jid).Msg("Session import complete")
	return nil
}

// validateCredentials checks that the minimum required fields are present.
func validateCredentials(creds *SessionCredentials) error {
	if len(creds.NoiseKeyPriv) != 32 {
		return fmt.Errorf("noiseKey.private must be 32 bytes, got %d", len(creds.NoiseKeyPriv))
	}
	if len(creds.IdentKeyPriv) != 32 {
		return fmt.Errorf("identityKey.private must be 32 bytes, got %d", len(creds.IdentKeyPriv))
	}
	if len(creds.SignedPreKeyPriv) != 32 {
		return fmt.Errorf("signedPreKey.private must be 32 bytes, got %d", len(creds.SignedPreKeyPriv))
	}
	if len(creds.SignedPreKeySig) != 64 {
		return fmt.Errorf("signedPreKey.signature must be 64 bytes, got %d", len(creds.SignedPreKeySig))
	}
	if creds.RegistrationID == 0 {
		return errors.New("registrationId is 0 or missing")
	}
	return nil
}

// Respond is already defined in handlers.go – don't redefine here.
// The helpers below convert slices to the standard API response format.
func bytesToBufferJSON(b []byte) map[string]interface{} {
	if len(b) == 0 {
		return map[string]interface{}{"type": "Buffer", "data": []byte{}}
	}
	data := make([]int, len(b))
	for i, v := range b {
		data[i] = int(v)
	}
	return map[string]interface{}{"type": "Buffer", "data": data}
}
