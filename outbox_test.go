package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/jmoiron/sqlx"
)

// openTestOutboxDB abre um SQLite em memória com as tabelas mínimas do outbox.
func openTestOutboxDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL, token TEXT NOT NULL, webhook TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE webhook_outbox (
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
		)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return db
}

func seedOutboxUser(t *testing.T, db *sqlx.DB, userID, webhook string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users (id, name, token, webhook) VALUES (?, ?, ?, ?)`,
		userID, "n-"+userID, "tok-"+userID, webhook); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func outboxCount(t *testing.T, db *sqlx.DB) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM webhook_outbox`); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// makeDue força o item a vencer agora (enqueue agenda +60s por padrão).
func makeDue(t *testing.T, db *sqlx.DB, id int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE webhook_outbox SET next_retry_at = ?, claimed_at = NULL WHERE id = ?`,
		time.Now().Add(-time.Minute), id); err != nil {
		t.Fatalf("makeDue: %v", err)
	}
}

func lastOutboxID(t *testing.T, db *sqlx.DB) int64 {
	t.Helper()
	var id int64
	if err := db.Get(&id, `SELECT MAX(id) FROM webhook_outbox`); err != nil {
		t.Fatalf("lastID: %v", err)
	}
	return id
}

func setTestHTTPClient(t *testing.T, userID string) {
	t.Helper()
	clientManager.SetHTTPClient(userID, resty.New())
	t.Cleanup(func() { clientManager.DeleteHTTPClient(userID) })
}

func testOutboxTick(db *sqlx.DB) {
	cfg := loadOutboxConfig()
	stopCh := make(chan struct{})
	outboxTick(db, cfg, stopCh)
}

func TestEnqueueOutboxPersists(t *testing.T) {
	db := openTestOutboxDB(t)
	seedOutboxUser(t, db, "u1", "http://x/hook")
	data := map[string]string{
		"jsonData":     `{"type":"Message","messageId":"m1"}`,
		"userID":       "u1",
		"instanceName": "inst",
	}
	if err := enqueueOutbox(db, "u1", "http://x/hook", data, "boom"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var it outboxItem
	if err := db.Get(&it, `SELECT * FROM webhook_outbox LIMIT 1`); err != nil {
		t.Fatalf("select: %v", err)
	}
	if it.UserID != "u1" || it.EventType != "Message" || it.MessageID != "m1" {
		t.Errorf("meta = %+v, want user u1 / Message / m1", it)
	}
	if it.Attempts != 0 || it.LastError != "boom" {
		t.Errorf("attempts/lastError = %d/%q", it.Attempts, it.LastError)
	}
}

func TestOutboxBackoffMath(t *testing.T) {
	if d := outboxBackoff(60, 3600, 0); d != 60*time.Second {
		t.Errorf("attempts=0: %v, want 60s", d)
	}
	if d := outboxBackoff(60, 3600, 1); d != 120*time.Second {
		t.Errorf("attempts=1: %v, want 120s", d)
	}
	if d := outboxBackoff(60, 3600, 10); d != 3600*time.Second {
		t.Errorf("attempts=10: %v, want teto 3600s", d)
	}
}

func TestOutboxWorkerDelivers(t *testing.T) {
	db := openTestOutboxDB(t)
	var hits atomic.Int64
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = r.ParseForm()
		gotBody.Store(r.PostFormValue("jsonData"))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	seedOutboxUser(t, db, "u1", srv.URL)
	setTestHTTPClient(t, "u1")
	data := map[string]string{"jsonData": `{"type":"Message","messageId":"m9"}`, "userID": "u1"}
	if err := enqueueOutbox(db, "u1", srv.URL, data, "x"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	makeDue(t, db, lastOutboxID(t, db))

	before := deliveryStats.outboxDelivered.Load()
	testOutboxTick(db)

	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if got, _ := gotBody.Load().(string); got == "" {
		t.Error("corpo sem jsonData")
	}
	if n := outboxCount(t, db); n != 0 {
		t.Errorf("pendentes = %d, want 0 (entregue)", n)
	}
	if d := deliveryStats.outboxDelivered.Load() - before; d != 1 {
		t.Errorf("outboxDelivered delta = %d, want 1", d)
	}
}

func TestOutboxRetryThenDelivers(t *testing.T) {
	db := openTestOutboxDB(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	seedOutboxUser(t, db, "u1", srv.URL)
	setTestHTTPClient(t, "u1")
	data := map[string]string{"jsonData": `{"type":"Message"}`, "userID": "u1"}
	if err := enqueueOutbox(db, "u1", srv.URL, data, "x"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	id := lastOutboxID(t, db)

	makeDue(t, db, id)
	testOutboxTick(db) // fail 1
	makeDue(t, db, id)
	testOutboxTick(db) // fail 2
	var attempts int
	if err := db.Get(&attempts, `SELECT attempts FROM webhook_outbox WHERE id = ?`, id); err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	makeDue(t, db, id)
	testOutboxTick(db) // 200 -> delete

	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", hits.Load())
	}
	if n := outboxCount(t, db); n != 0 {
		t.Errorf("pendentes = %d, want 0", n)
	}
}

func TestOutboxUserGoneDrops(t *testing.T) {
	db := openTestOutboxDB(t)
	// Órfão simulado: desliga FK temporariamente (em produção o cascade apaga
	// junto; o branch do worker cobre a corrida claim-vs-delete).
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO webhook_outbox (user_id, webhook_url, event_type, payload, next_retry_at)
		VALUES (?, ?, ?, ?, ?)`, "ghost", "http://x", "Message", `{"type":"Message"}`,
		time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	before := deliveryStats.outboxDropped.Load()
	testOutboxTick(db)
	if n := outboxCount(t, db); n != 0 {
		t.Errorf("pendentes = %d, want 0 (canal excluído)", n)
	}
	if d := deliveryStats.outboxDropped.Load() - before; d != 1 {
		t.Errorf("outboxDropped delta = %d, want 1", d)
	}
}

func TestOutboxNoWebhookKeeps(t *testing.T) {
	db := openTestOutboxDB(t)
	seedOutboxUser(t, db, "u1", "") // usuário existe, sem webhook
	if _, err := db.Exec(`INSERT INTO webhook_outbox (user_id, webhook_url, event_type, payload, next_retry_at)
		VALUES (?, ?, ?, ?, ?)`, "u1", "", "Message", `{"type":"Message"}`,
		time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	testOutboxTick(db)
	if n := outboxCount(t, db); n != 1 {
		t.Errorf("pendentes = %d, want 1 (mantém sem webhook)", n)
	}
	var attempts int
	_ = db.Get(&attempts, `SELECT attempts FROM webhook_outbox LIMIT 1`)
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0 (sem tentativa HTTP)", attempts)
	}
}

func TestOutboxUsesCurrentURL(t *testing.T) {
	db := openTestOutboxDB(t)
	var hitsOld, hitsNew atomic.Int64
	srvOld := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsOld.Add(1)
		w.WriteHeader(200)
	}))
	defer srvOld.Close()
	srvNew := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsNew.Add(1)
		w.WriteHeader(200)
	}))
	defer srvNew.Close()

	// Enqueue com snapshot da URL antiga, mas usuário já migrou para a nova.
	seedOutboxUser(t, db, "u1", srvNew.URL)
	setTestHTTPClient(t, "u1")
	data := map[string]string{"jsonData": `{"type":"Message"}`, "userID": "u1"}
	if err := enqueueOutbox(db, "u1", srvOld.URL, data, "x"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	makeDue(t, db, lastOutboxID(t, db))
	testOutboxTick(db)

	if hitsOld.Load() != 0 {
		t.Errorf("hits URL antiga = %d, want 0", hitsOld.Load())
	}
	if hitsNew.Load() != 1 {
		t.Errorf("hits URL atual = %d, want 1", hitsNew.Load())
	}
	if n := outboxCount(t, db); n != 0 {
		t.Errorf("pendentes = %d, want 0", n)
	}
}

func TestOutboxExpiredPurged(t *testing.T) {
	db := openTestOutboxDB(t)
	seedOutboxUser(t, db, "u1", "http://x")
	old := time.Now().AddDate(0, 0, -4) // além da retenção de 3d
	if _, err := db.Exec(`INSERT INTO webhook_outbox (user_id, webhook_url, event_type, payload, next_retry_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "u1", "http://x", "Message", `{"type":"Message"}`,
		time.Now().Add(-time.Minute), old); err != nil {
		t.Fatalf("insert: %v", err)
	}
	before := deliveryStats.outboxExpired.Load()
	testOutboxTick(db)
	if n := outboxCount(t, db); n != 0 {
		t.Errorf("pendentes = %d, want 0 (expirado)", n)
	}
	if d := deliveryStats.outboxExpired.Load() - before; d != 1 {
		t.Errorf("outboxExpired delta = %d, want 1", d)
	}
}

func TestOutboxCorruptPayloadDrops(t *testing.T) {
	db := openTestOutboxDB(t)
	seedOutboxUser(t, db, "u1", "http://x")
	setTestHTTPClient(t, "u1")
	if _, err := db.Exec(`INSERT INTO webhook_outbox (user_id, webhook_url, event_type, payload, next_retry_at)
		VALUES (?, ?, ?, ?, ?)`, "u1", "http://x", "Message", `not-json{{{`,
		time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	before := deliveryStats.outboxDropped.Load()
	testOutboxTick(db)
	if n := outboxCount(t, db); n != 0 {
		t.Errorf("pendentes = %d, want 0 (payload inválido)", n)
	}
	if d := deliveryStats.outboxDropped.Load() - before; d != 1 {
		t.Errorf("outboxDropped delta = %d, want 1", d)
	}
}

func TestExtractOutboxMeta(t *testing.T) {
	et, mid := extractOutboxMeta(map[string]string{"jsonData": `{"type":"Message","messageId":"abc"}`})
	if et != "Message" || mid != "abc" {
		t.Errorf("got %q/%q, want Message/abc", et, mid)
	}
	et, mid = extractOutboxMeta(map[string]string{"jsonData": `{"type":"Presence"}`})
	if et != "Presence" || mid != "" {
		t.Errorf("got %q/%q, want Presence/''", et, mid)
	}
	et, _ = extractOutboxMeta(map[string]string{})
	if et != "unknown" {
		t.Errorf("got %q, want unknown", et)
	}
}

func TestOutboxConfigDefaults(t *testing.T) {
	cfg := loadOutboxConfig()
	if !cfg.enabled || cfg.pollSeconds != 30 || cfg.batch != 100 ||
		cfg.baseSeconds != 60 || cfg.maxSeconds != 3600 || cfg.maxAgeDays != 3 {
		t.Errorf("defaults = %+v", cfg)
	}
	_ = fmt.Sprint(cfg)
}

// TestOutboxMigrationSQLite aplica a cadeia real de migrations (1..12) num
// banco fresco e valida que o outbox funciona sobre ele.
func TestOutboxMigrationSQLite(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlx.Open("sqlite", dir+"/mig.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := initializeSchema(db); err != nil {
		t.Fatalf("initializeSchema: %v", err)
	}
	var name string
	if err := db.Get(&name, `SELECT name FROM sqlite_master WHERE type='table' AND name='webhook_outbox'`); err != nil {
		t.Fatalf("tabela webhook_outbox não criada: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, name, token, webhook) VALUES (?, ?, ?, ?)`,
		"u1", "n", "tok", "http://x"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	data := map[string]string{"jsonData": `{"type":"Message"}`, "userID": "u1"}
	if err := enqueueOutbox(db, "u1", "http://x", data, "x"); err != nil {
		t.Fatalf("enqueue pós-migration: %v", err)
	}
	if n := outboxCount(t, db); n != 1 {
		t.Errorf("pendentes = %d, want 1", n)
	}
}

// TestEnsureWebhookOutboxIdempotent: ensure sobre tabela existente é no-op.
func TestEnsureWebhookOutboxIdempotent(t *testing.T) {
	db := openTestOutboxDB(t)
	if err := ensureWebhookOutbox(db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := ensureWebhookOutbox(db); err != nil {
		t.Fatalf("ensure 2x: %v", err)
	}
	seedOutboxUser(t, db, "u1", "http://x")
	data := map[string]string{"jsonData": `{"type":"Message"}`, "userID": "u1"}
	if err := enqueueOutbox(db, "u1", "http://x", data, "x"); err != nil {
		t.Fatalf("enqueue pós-ensure: %v", err)
	}
}

// TestEnsureWebhookOutboxCreates: sem a tabela (caso do PG que só tinha o
// registro da migration), o ensure cria e o worker volta a funcionar.
func TestEnsureWebhookOutboxCreates(t *testing.T) {
	db, err := sqlx.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL, token TEXT NOT NULL, webhook TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("ddl users: %v", err)
	}
	// Estado quebrado: sem webhook_outbox.
	if err := ensureWebhookOutbox(db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var name string
	if err := db.Get(&name, `SELECT name FROM sqlite_master WHERE type='table' AND name='webhook_outbox'`); err != nil {
		t.Fatalf("tabela não criada pelo ensure: %v", err)
	}
}

// TestOutboxMissingTableSelfHeals: ciclo com tabela ausente dispara o ensure
// único e o ciclo seguinte funciona.
func TestOutboxMissingTableSelfHeals(t *testing.T) {
	db, err := sqlx.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL, token TEXT NOT NULL, webhook TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("ddl users: %v", err)
	}
	outboxEnsureDone = false
	t.Cleanup(func() { outboxEnsureDone = false })
	testOutboxTick(db) // tabela ausente -> ensure + retry interno
	var name string
	if err := db.Get(&name, `SELECT name FROM sqlite_master WHERE type='table' AND name='webhook_outbox'`); err != nil {
		t.Fatalf("auto-cura não criou a tabela: %v", err)
	}
	if !outboxEnsureDone {
		t.Error("flag outboxEnsureDone não marcada")
	}
}
