package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// openDeletionTestDB abre um SQLite em memória com as tabelas mínimas
// envolvidas na exclusão de usuário.
func openDeletionTestDB(t *testing.T, withOutbox bool) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	ddls := []string{
		`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			token TEXT NOT NULL DEFAULT '',
			webhook TEXT NOT NULL DEFAULT '',
			jid TEXT NOT NULL DEFAULT '',
			device_jid TEXT NOT NULL DEFAULT '',
			events TEXT NOT NULL DEFAULT '',
			connected INTEGER,
			history INTEGER DEFAULT 0
		)`,
		`CREATE TABLE message_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			chat_jid TEXT NOT NULL,
			sender_jid TEXT NOT NULL,
			message_id TEXT NOT NULL,
			timestamp DATETIME NOT NULL,
			message_type TEXT NOT NULL,
			text_content TEXT,
			media_link TEXT,
			quoted_message_id TEXT,
			datajson TEXT
		)`,
	}
	if withOutbox {
		ddls = append(ddls, `CREATE TABLE webhook_outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			webhook_url TEXT NOT NULL DEFAULT '',
			event_type TEXT NOT NULL DEFAULT '',
			message_id TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_retry_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			claimed_at DATETIME NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`)
	}
	for _, ddl := range ddls {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return db
}

func seedDeletionUser(t *testing.T, db *sqlx.DB, id, jid, deviceJID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users (id, name, token, jid, device_jid) VALUES (?, ?, ?, ?, ?)`,
		id, "n-"+id, "tok-"+id, jid, deviceJID); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

func seedDeletionDependents(t *testing.T, db *sqlx.DB, userID string, withOutbox bool) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO message_history (user_id, chat_jid, sender_jid, message_id, timestamp, message_type)
		VALUES (?, 'chat', 'me', ?, CURRENT_TIMESTAMP, 'text')`, userID, "msg-"+userID); err != nil {
		t.Fatalf("seed history %s: %v", userID, err)
	}
	if !withOutbox {
		return
	}
	if _, err := db.Exec(`INSERT INTO webhook_outbox (user_id, payload) VALUES (?, '{}')`, userID); err != nil {
		t.Fatalf("seed outbox %s: %v", userID, err)
	}
}

func countRows(t *testing.T, db *sqlx.DB, table, column, value string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, db.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE `+column+`=?`), value); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestDeleteUserAndHistoryRemovesDependents(t *testing.T) {
	db := openDeletionTestDB(t, true)
	s := &server{db: db}

	seedDeletionUser(t, db, "gone", "15551234567@s.whatsapp.net", "")
	seedDeletionUser(t, db, "keep", "15559999999@s.whatsapp.net", "")
	seedDeletionDependents(t, db, "gone", true)
	seedDeletionDependents(t, db, "keep", true)

	affected, err := s.deleteUserAndHistory("gone")
	if err != nil {
		t.Fatalf("deleteUserAndHistory: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d; want 1", affected)
	}

	for _, table := range []string{"users", "message_history", "webhook_outbox"} {
		column := "user_id"
		if table == "users" {
			column = "id"
		}
		if got := countRows(t, db, table, column, "gone"); got != 0 {
			t.Errorf("%s: deleted user rows = %d; want 0", table, got)
		}
		if got := countRows(t, db, table, column, "keep"); got != 1 {
			t.Errorf("%s: other user rows = %d; want 1", table, got)
		}
	}
}

func TestDeleteUserAndHistoryToleratesMissingOutbox(t *testing.T) {
	db := openDeletionTestDB(t, false)
	s := &server{db: db}

	seedDeletionUser(t, db, "gone", "", "")
	seedDeletionDependents(t, db, "gone", false)

	affected, err := s.deleteUserAndHistory("gone")
	if err != nil {
		t.Fatalf("deleteUserAndHistory: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d; want 1", affected)
	}
	if got := countRows(t, db, "users", "id", "gone"); got != 0 {
		t.Errorf("user retained: %d", got)
	}
	if got := countRows(t, db, "message_history", "user_id", "gone"); got != 0 {
		t.Errorf("history retained: %d", got)
	}
}

func TestDeleteUserPreserveDevicesKeepsDeviceLink(t *testing.T) {
	db := openDeletionTestDB(t, true)
	s := &server{db: db, exPath: t.TempDir()}

	const userID = "preserve"
	seedDeletionUser(t, db, userID, "15551234567@s.whatsapp.net", "15551234567:92@s.whatsapp.net")
	seedDeletionDependents(t, db, userID, true)

	// Store indisponível prova que esta rota não tenta resolver/apagar device.
	previous := container
	container = nil
	defer func() { container = previous }()

	req := mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/", nil), map[string]string{"id": userID})
	rec := httptest.NewRecorder()
	s.DeleteUserPreserveDevices()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Preserved bool `json:"devices_preserved"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Data.Preserved {
		t.Fatalf("missing devices_preserved: %s", rec.Body.String())
	}
	if got := countRows(t, db, "users", "id", userID); got != 0 {
		t.Errorf("user retained: %d", got)
	}
	if got := countRows(t, db, "message_history", "user_id", userID); got != 0 {
		t.Errorf("history retained: %d", got)
	}
	if got := countRows(t, db, "webhook_outbox", "user_id", userID); got != 0 {
		t.Errorf("outbox retained: %d", got)
	}
}

func TestFindSessionDeviceAmbiguous(t *testing.T) {
	stores := &mockDeviceStore{devices: make(map[string]*store.Device)}
	stores.devices["15551234567:92@s.whatsapp.net"] = &store.Device{ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 92}}
	stores.devices["15551234567:95@s.whatsapp.net"] = &store.Device{ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 95}}

	device, err := findSessionDevice(context.Background(), stores, "", "15551234567@s.whatsapp.net")
	if device != nil || err == nil {
		t.Fatalf("expected ambiguity error, got device=%v err=%v", device, err)
	}
}
