package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow/types"
)

// writeSessionToWhatsmeow writes the imported session credentials into
// whatsmeow's database tables, replacing any existing device for the given JID.
// It uses whatsmeowDB for direct SQL access to whatsmeow's tables.
func writeSessionToWhatsmeow(db *sqlx.DB, creds *SessionCredentials) error {
	if len(creds.NoiseKeyPriv) == 0 {
		return fmt.Errorf("noise key private is empty – cannot restore session")
	}

	jid := normalizeJID(creds.JID)
	if jid == "" {
		return fmt.Errorf("JID is empty – cannot determine which account to restore")
	}

	// Determine placeholder style ($1 for postgres, ? for sqlite)
	isPostgres := isPostgresDB(db)

	// Delete existing device and associated data
	if err := deleteExistingDevice(db, jid, isPostgres); err != nil {
		return fmt.Errorf("deleting existing device: %w", err)
	}

	// Insert device
	if err := insertDevice(db, jid, creds, isPostgres); err != nil {
		return fmt.Errorf("inserting device: %w", err)
	}

	// Insert pre-keys
	if err := insertPreKeys(db, jid, creds.PreKeys, isPostgres); err != nil {
		log.Warn().Err(err).Msg("Failed to insert pre-keys (non-fatal)")
	}

	// Insert app state sync keys
	if err := insertAppStateSyncKeys(db, jid, creds.AppStateSyncKeys, isPostgres); err != nil {
		log.Warn().Err(err).Msg("Failed to insert app state sync keys (non-fatal)")
	}

	// Insert app state versions
	if err := insertAppStateVersions(db, jid, creds.AppStateVersions, isPostgres); err != nil {
		log.Warn().Err(err).Msg("Failed to insert app state versions (non-fatal)")
	}

	// Insert identity keys
	if err := insertIdentityKeys(db, jid, creds.IdentityKeys, isPostgres); err != nil {
		log.Warn().Err(err).Msg("Failed to insert identity keys (non-fatal)")
	}

	log.Info().Str("jid", jid).Msg("Session written to whatsmeow database")
	return nil
}

// normalizeJID converts various JID forms to the canonical format whatsmeow expects.
// e.g. "5567819303355:54@c.us" stays as-is; "5567819303355@s.whatsapp.net" stays as-is.
func normalizeJID(jid string) string {
	return strings.TrimSpace(jid)
}

// isPostgresDB checks if the DB is PostgreSQL by trying to detect dialect.
func isPostgresDB(db *sqlx.DB) bool {
	return db.DriverName() == "postgres"
}

// ph returns a placeholder string for positional parameters.
// PostgreSQL uses $N, SQLite uses ?.
func ph(isPostgres bool, n int) string {
	if isPostgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// phList returns a list of placeholders like "$1,$2,$3" or "?,?,?".
func phList(isPostgres bool, count, startAt int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = ph(isPostgres, startAt+i)
	}
	return strings.Join(parts, ",")
}

func deleteExistingDevice(db *sqlx.DB, jid string, isPostgres bool) error {
	p1 := ph(isPostgres, 1)

	// privacy_tokens has no FK cascade, delete manually
	if _, err := db.Exec(fmt.Sprintf("DELETE FROM whatsmeow_privacy_tokens WHERE our_jid = %s", p1), jid); err != nil {
		log.Debug().Err(err).Msg("Could not delete privacy tokens")
	}

	// All other child tables have ON DELETE CASCADE from whatsmeow_device
	if _, err := db.Exec(fmt.Sprintf("DELETE FROM whatsmeow_device WHERE jid = %s", p1), jid); err != nil {
		log.Debug().Err(err).Msg("Could not delete from whatsmeow_device")
	}

	return nil
}

// deviceColumns returns the actual column list for whatsmeow_device by probing the DB.
// Different whatsmeow versions have different columns (e.g. some lack "initialized", "business_name", "lid").
func deviceColumns(db *sqlx.DB, isPostgres bool) ([]string, error) {
	var rows *sql.Rows
	var err error
	if isPostgres {
		rows, err = db.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = 'whatsmeow_device' ORDER BY ordinal_position`)
	} else {
		rows, err = db.Query(`PRAGMA table_info(whatsmeow_device)`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	if isPostgres {
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err == nil {
				cols = append(cols, col)
			}
		}
	} else {
		for rows.Next() {
			var cid int
			var name, colType string
			var notNull int
			var dflt interface{}
			var pk int
			if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err == nil {
				cols = append(cols, name)
			}
		}
	}
	return cols, rows.Err()
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// padBytes returns b padded/truncated to exactly n bytes.
func padBytes(b []byte, n int) []byte {
	if len(b) == n {
		return b
	}
	out := make([]byte, n)
	copy(out, b)
	return out
}

func insertDevice(db *sqlx.DB, jid string, creds *SessionCredentials, isPostgres bool) error {
	// Enforce byte-length constraints from schema CHECK clauses
	advKey := padBytes(creds.AdvKey, 32)                     // NOT NULL, no length check
	advAccountSig := padBytes(creds.AdvAccountSig, 64)       // CHECK length=64
	advAccountSigKey := padBytes(creds.AdvAccountSigKey, 32) // CHECK length=32
	advDeviceSig := padBytes(creds.AdvDeviceSig, 64)         // CHECK length=64
	advDetails := creds.AdvDetails
	if advDetails == nil {
		advDetails = []byte{}
	}

	platform := creds.Platform
	if platform == "" {
		platform = "web"
	}

	// Probe actual columns so we don't reference columns that don't exist in this schema version.
	existingCols, err := deviceColumns(db, isPostgres)
	if err != nil {
		existingCols = []string{}
	}

	// Build column list and args dynamically.
	// Core columns always present:
	colNames := []string{
		"jid", "registration_id",
		"noise_key", "identity_key",
		"signed_pre_key", "signed_pre_key_id", "signed_pre_key_sig",
		"adv_key", "adv_details", "adv_account_sig", "adv_account_sig_key", "adv_device_sig",
		"platform", "push_name",
	}
	args := []interface{}{
		jid, int64(creds.RegistrationID),
		creds.NoiseKeyPriv, creds.IdentKeyPriv,
		creds.SignedPreKeyPriv, int(creds.SignedPreKeyID), creds.SignedPreKeySig,
		advKey, advDetails, advAccountSig, advAccountSigKey, advDeviceSig,
		platform, creds.PushName,
	}

	// Optional columns — only add if they exist in this schema version
	if len(existingCols) == 0 || contains(existingCols, "business_name") {
		colNames = append(colNames, "business_name")
		args = append(args, "")
	}
	if len(existingCols) == 0 || contains(existingCols, "lid") {
		colNames = append(colNames, "lid")
		args = append(args, creds.LID)
	}
	if len(existingCols) == 0 || contains(existingCols, "initialized") {
		colNames = append(colNames, "initialized")
		args = append(args, true)
	}

	n := len(args)
	colList := strings.Join(colNames, ", ")
	placeholders := phList(isPostgres, n, 1)

	var q string
	if isPostgres {
		setList := make([]string, 0, n)
		for _, c := range colNames {
			if c == "jid" {
				continue
			}
			setList = append(setList, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
		}
		q = fmt.Sprintf(`INSERT INTO whatsmeow_device (%s) VALUES (%s) ON CONFLICT (jid) DO UPDATE SET %s`,
			colList, placeholders, strings.Join(setList, ", "))
	} else {
		q = fmt.Sprintf(`INSERT OR REPLACE INTO whatsmeow_device (%s) VALUES (%s)`, colList, placeholders)
	}

	_, err = db.Exec(q, args...)
	return err
}

func insertPreKeys(db *sqlx.DB, jid string, preKeys map[uint32][]byte, isPostgres bool) error {
	if len(preKeys) == 0 {
		return nil
	}

	for keyID, privKey := range preKeys {
		if len(privKey) == 0 {
			continue
		}
		var q string
		if isPostgres {
			q = `INSERT INTO whatsmeow_pre_keys (jid, key_id, key, uploaded)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT (jid, key_id) DO UPDATE SET key = EXCLUDED.key, uploaded = EXCLUDED.uploaded`
		} else {
			q = `INSERT OR REPLACE INTO whatsmeow_pre_keys (jid, key_id, key, uploaded)
				 VALUES (?, ?, ?, ?)`
		}
		if _, err := db.Exec(q, jid, int(keyID), privKey, false); err != nil {
			return fmt.Errorf("pre-key %d: %w", keyID, err)
		}
	}
	return nil
}

func insertAppStateSyncKeys(db *sqlx.DB, jid string, keys []AppStateSyncKeyEntry, isPostgres bool) error {
	for _, k := range keys {
		if len(k.KeyID) == 0 || len(k.KeyData) == 0 {
			continue
		}
		var q string
		if isPostgres {
			q = `INSERT INTO whatsmeow_app_state_sync_keys (jid, key_id, key_data, timestamp, fingerprint)
				 VALUES ($1, $2, $3, $4, $5)
				 ON CONFLICT (jid, key_id) DO UPDATE SET
				   key_data = EXCLUDED.key_data,
				   timestamp = EXCLUDED.timestamp,
				   fingerprint = EXCLUDED.fingerprint`
		} else {
			q = `INSERT OR REPLACE INTO whatsmeow_app_state_sync_keys (jid, key_id, key_data, timestamp, fingerprint)
				 VALUES (?, ?, ?, ?, ?)`
		}
		if _, err := db.Exec(q, jid, k.KeyID, k.KeyData, k.Timestamp, []byte{}); err != nil {
			return fmt.Errorf("app state sync key: %w", err)
		}
	}
	return nil
}

func insertAppStateVersions(db *sqlx.DB, jid string, versions []AppStateVersionEntry, isPostgres bool) error {
	// Schema: whatsmeow_app_state_version (jid, name, version, hash) — hash must be 128 bytes
	for _, v := range versions {
		if v.Collection == "" {
			continue
		}
		hash := v.Hash
		if len(hash) == 0 {
			hash = make([]byte, 128) // zero hash satisfies NOT NULL + CHECK(length=128)
		} else if len(hash) != 128 {
			// Pad or skip — whatsmeow enforces length=128
			padded := make([]byte, 128)
			copy(padded, hash)
			hash = padded
		}
		var q string
		if isPostgres {
			q = `INSERT INTO whatsmeow_app_state_version (jid, name, version, hash)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT (jid, name) DO UPDATE SET
				   version = EXCLUDED.version,
				   hash = EXCLUDED.hash`
		} else {
			q = `INSERT OR REPLACE INTO whatsmeow_app_state_version (jid, name, version, hash)
				 VALUES (?, ?, ?, ?)`
		}
		if _, err := db.Exec(q, jid, v.Collection, int64(v.Version), hash); err != nil {
			return fmt.Errorf("app state version %s: %w", v.Collection, err)
		}
	}
	return nil
}

func insertIdentityKeys(db *sqlx.DB, jid string, identityKeys map[string][]byte, isPostgres bool) error {
	for addr, key := range identityKeys {
		if len(key) == 0 {
			continue
		}
		// Schema uses column name "identity" not "key"
		var q string
		if isPostgres {
			q = `INSERT INTO whatsmeow_identity_keys (our_jid, their_id, identity)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (our_jid, their_id) DO UPDATE SET identity = EXCLUDED.identity`
		} else {
			q = `INSERT OR REPLACE INTO whatsmeow_identity_keys (our_jid, their_id, identity)
				 VALUES (?, ?, ?)`
		}
		if _, err := db.Exec(q, jid, addr, key); err != nil {
			return fmt.Errorf("identity key %s: %w", addr, err)
		}
	}
	return nil
}

// stopUserSession gracefully stops the whatsmeow client for a user.
func stopUserSession(userID string) {
	client := clientManager.GetWhatsmeowClient(userID)
	if client != nil {
		if client.IsConnected() {
			client.Disconnect()
		}
		clientManager.DeleteWhatsmeowClient(userID)
	}
	clientManager.DeleteMyClient(userID)
	killchannel.Send(userID)
}

// restartUserSessionAfterImport restarts the whatsmeow session for a user
// after credentials have been written to the database.
func (s *server) restartUserSessionAfterImport(ctx context.Context, userID, jid, token string) error {
	// Update the JID in the users table
	if _, err := s.db.ExecContext(ctx, "UPDATE users SET jid=$1, connected=0 WHERE id=$2", jid, userID); err != nil {
		return fmt.Errorf("updating user jid: %w", err)
	}

	// Update cache
	if userinfo, found := userinfocache.Get(token); found {
		v := updateUserInfo(userinfo, "Jid", jid)
		userinfocache.Set(token, v, 0)
	}

	// Get subscribed events for this user
	var events string
	_ = s.db.QueryRowContext(ctx, "SELECT events FROM users WHERE id=$1", userID).Scan(&events)

	var subscribedEvents []string
	for _, e := range strings.Split(events, ",") {
		e = strings.TrimSpace(e)
		if e != "" && Find(supportedEventTypes, e) {
			subscribedEvents = append(subscribedEvents, e)
		}
	}

	// Create new kill channel
	killchannel.Create(userID)

	// Start the client with the new JID
	go s.startClient(userID, jid, token, subscribedEvents)

	return nil
}

// getWhatsmeowJIDFromParsed converts a JID object back to the string
// format used by whatsmeow in the database.
func formatWhatsmeowJID(jidStr string) string {
	// Try to parse and reformat
	parsed, err := types.ParseJID(jidStr)
	if err != nil {
		return jidStr
	}
	return parsed.String()
}
