package main

// Testes anti-perda de rajada em grupo.
//
// Cobrem o pacote: slot liberado durante backoff, worker concorrente sem
// duplicata, timeout próprio do worker, contadores de shed e default do
// shutdown acima do timeout do POST (30s).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func saveRestoreRetryFlags(t *testing.T) {
	t.Helper()
	oldEnabled, oldCount, oldDelay := *webhookRetryEnabled, *webhookRetryCount, *webhookRetryDelaySeconds
	t.Cleanup(func() {
		*webhookRetryEnabled, *webhookRetryCount, *webhookRetryDelaySeconds = oldEnabled, oldCount, oldDelay
	})
}

func TestBurstConfigDefaults(t *testing.T) {
	cfg := loadOutboxConfig()
	if cfg.workers != 4 {
		t.Errorf("workers = %d, want 4", cfg.workers)
	}
	if cfg.ordered {
		t.Error("ordered = true, want false (default)")
	}
	if cfg.httpTimeoutSeconds != 20 {
		t.Errorf("httpTimeoutSeconds = %d, want 20", cfg.httpTimeoutSeconds)
	}

	t.Setenv("OUTBOX_WORKERS", "8")
	t.Setenv("OUTBOX_ORDERED_DELIVERY", "true")
	t.Setenv("OUTBOX_HTTP_TIMEOUT_SECONDS", "10")
	cfg = loadOutboxConfig()
	if cfg.workers != 8 || !cfg.ordered || cfg.httpTimeoutSeconds != 10 {
		t.Errorf("env override = %+v, want workers=8 ordered=true timeout=10", cfg)
	}

	t.Setenv("OUTBOX_WORKERS", "0") // fora da faixa: mantém o anterior? não — recarrega do zero
	cfg = loadOutboxConfig()
	if cfg.workers != 4 {
		t.Errorf("workers inválido: got %d, want default 4", cfg.workers)
	}
}

// TestOutboxConcurrentNoDuplicates: 20 itens vencidos, receptor 200 imediato,
// caminho concorrente forçado — tudo entregue exatamente 1 vez, nada pendente.
func TestOutboxConcurrentNoDuplicates(t *testing.T) {
	db := openTestOutboxDB(t)
	db.SetMaxOpenConns(1) // serializa no driver; o fan-out de goroutines é o que está sob teste
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	seedOutboxUser(t, db, "u1", srv.URL)
	setTestHTTPClient(t, "u1")

	const n = 20
	for i := 0; i < n; i++ {
		data := map[string]string{
			"jsonData": fmt.Sprintf(`{"type":"Message","messageId":"m-%d"}`, i),
			"userID":   "u1",
		}
		if err := enqueueOutbox(db, "u1", srv.URL, data, "x"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`UPDATE webhook_outbox SET next_retry_at = ?, claimed_at = NULL`,
		time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("makeDue all: %v", err)
	}

	outboxForceConcurrent = true
	t.Cleanup(func() { outboxForceConcurrent = false })

	cfg := loadOutboxConfig()
	cfg.workers = 4
	before := deliveryStats.outboxDelivered.Load()
	outboxTick(db, cfg, make(chan struct{}))

	if hits.Load() != n {
		t.Fatalf("hits = %d, want %d (perda ou duplicata)", hits.Load(), n)
	}
	if p := outboxCount(t, db); p != 0 {
		t.Errorf("pendentes = %d, want 0", p)
	}
	if d := deliveryStats.outboxDelivered.Load() - before; d != n {
		t.Errorf("outboxDelivered delta = %d, want %d", d, n)
	}
}

// TestOutboxWorkerTimeoutEnforced: receptor mais lento que o timeout do worker
// falha rápido (sem prender o lote) e o item é reagendado, não perdido.
func TestOutboxWorkerTimeoutEnforced(t *testing.T) {
	db := openTestOutboxDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	seedOutboxUser(t, db, "u1", srv.URL)
	setTestHTTPClient(t, "u1")
	data := map[string]string{"jsonData": `{"type":"Message"}`, "userID": "u1"}
	if err := enqueueOutbox(db, "u1", srv.URL, data, "x"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	makeDue(t, db, lastOutboxID(t, db))

	cfg := loadOutboxConfig()
	cfg.httpTimeoutSeconds = 1
	cfg.workers = 1
	start := time.Now()
	outboxTick(db, cfg, make(chan struct{}))
	elapsed := time.Since(start)

	if elapsed > 2500*time.Millisecond {
		t.Errorf("tick demorou %v, want <2.5s (timeout do worker não aplicado?)", elapsed)
	}
	if p := outboxCount(t, db); p != 1 {
		t.Errorf("pendentes = %d, want 1 (reagendado, não perdido)", p)
	}
	var attempts int
	_ = db.Get(&attempts, `SELECT attempts FROM webhook_outbox LIMIT 1`)
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// TestCallHookFreesSlotDuringBackoff: A falha a 1ª tentativa e dorme o
// backoff; B deve terminar ANTES de A (slot liberado). No código antigo B
// travava até A concluir.
func TestCallHookFreesSlotDuringBackoff(t *testing.T) {
	if shutdownStarted() {
		t.Skip("shutdown já sinalizado por outro teste")
	}
	saveRestoreRetryFlags(t)
	*webhookRetryEnabled, *webhookRetryCount, *webhookRetryDelaySeconds = true, 3, 3

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	userID := "burst-slot-user"
	setTestHTTPClient(t, userID)
	db := openTestOutboxDB(t)
	seedOutboxUser(t, db, userID, srv.URL)
	oldOutboxDB := outboxDB
	outboxDB = db
	t.Cleanup(func() { outboxDB = oldOutboxDB })

	payload := map[string]string{"jsonData": `{"type":"Message"}`, "userID": userID}
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		callHook(srv.URL, payload, userID)
	}()

	// Aguarda A entrar no backoff (slot liberado) de forma determinística:
	// semáforo vazio + 1ª tentativa já bateu no receptor.
	sema := webhookSemaFor(userID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if hits.Load() >= 1 && len(sema) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("A não entrou em backoff (hits=%d, slots ocupados=%d)", hits.Load(), len(sema))
		}
		time.Sleep(10 * time.Millisecond)
	}

	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		callHook(srv.URL, payload, userID)
	}()

	select {
	case <-doneB:
		// esperado: B pegou o slot livre e concluiu durante o backoff de A
	case <-doneA:
		t.Fatal("B terminou DEPOIS de A: slot ficou preso durante o backoff (regressão)")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout aguardando B")
	}

	select {
	case <-doneA:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout aguardando A")
	}
}

// TestShedCountsFullNotShutdown: shed com processo saudável conta como
// semáforo cheio e persiste no outbox (nada perdido).
func TestShedCountsFullNotShutdown(t *testing.T) {
	if shutdownStarted() {
		t.Skip("shutdown já sinalizado por outro teste")
	}
	db := openTestOutboxDB(t)
	seedOutboxUser(t, db, "u9", "http://destino/hook")
	oldOutboxDB := outboxDB
	outboxDB = db
	t.Cleanup(func() { outboxDB = oldOutboxDB })

	beforeShed, beforeFull := deliveryStats.webhookShed.Load(), deliveryStats.webhookShedFull.Load()
	shedToDLQ("http://destino/hook", map[string]string{"jsonData": `{"type":"Message"}`}, "u9", "semáforo de webhook cheio (receptor lento?)")
	if d := deliveryStats.webhookShed.Load() - beforeShed; d != 1 {
		t.Errorf("webhookShed delta = %d, want 1", d)
	}
	if d := deliveryStats.webhookShedFull.Load() - beforeFull; d != 1 {
		t.Errorf("webhookShedFull delta = %d, want 1", d)
	}
	if p := outboxCount(t, db); p != 1 {
		t.Errorf("pendentes = %d, want 1 (shed preservado no outbox)", p)
	}
}

// TestShutdownTimeoutDefault: default precisa ficar acima do timeout do POST
// (30s), senão o shutdown mata POSTs em voo de rajadas.
func TestShutdownTimeoutDefault(t *testing.T) {
	if *shutdownTimeoutSeconds < 60 {
		t.Errorf("shutdownTimeoutSeconds = %d, want >= 60 (acima do POST 30s)", *shutdownTimeoutSeconds)
	}
}

// TestSplitIDsChunks: 30 IDs com chunk 10 viram 3 lotes de 10;
// 7 IDs com chunk 10 viram 1 lote (sem fatiar à toa).
func TestSplitIDsChunks(t *testing.T) {
	mk := func(n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("3EB0%04d", i)
		}
		return ids
	}
	chunks := splitIDs(mk(30), 10)
	if len(chunks) != 3 {
		t.Fatalf("lotes = %d, want 3", len(chunks))
	}
	for i, c := range chunks {
		if len(c) != 10 {
			t.Fatalf("lote %d len = %d, want 10", i, len(c))
		}
	}
	if got := splitIDs(mk(7), 10); len(got) != 1 || len(got[0]) != 7 {
		t.Fatalf("7 ids: lotes = %d, want 1x7", len(got))
	}
	if got := splitIDs(mk(25), 10); len(got) != 3 || len(got[2]) != 5 {
		t.Fatalf("25 ids: lotes = %d, want 3 (10,10,5)", len(got))
	}
	if got := splitIDs(nil, 10); len(got) != 0 {
		t.Fatalf("nil: lotes = %d, want 0", len(got))
	}
}

// TestReceiptChunkSizeBounds: flag fora de [1,50] cai no default 10.
func TestReceiptChunkSizeBounds(t *testing.T) {
	old := *webhookReceiptChunkSize
	t.Cleanup(func() { *webhookReceiptChunkSize = old })
	*webhookReceiptChunkSize = 10
	if got := receiptChunkSize(); got != 10 {
		t.Errorf("chunk = %d, want 10", got)
	}
	*webhookReceiptChunkSize = 0
	if got := receiptChunkSize(); got != 10 {
		t.Errorf("chunk inválido: got %d, want default 10", got)
	}
	*webhookReceiptChunkSize = 99
	if got := receiptChunkSize(); got != 10 {
		t.Errorf("chunk inválido: got %d, want default 10", got)
	}
}

// TestInflightGaugeTracks: trackBackground incrementa/decrementa o gauge.
func TestInflightGaugeTracks(t *testing.T) {
	before := inflightDeliveries.Load()
	release := make(chan struct{})
	done := make(chan struct{})
	trackBackground(func() {
		close(done)
		<-release
	})
	<-done
	if got := inflightDeliveries.Load(); got != before+1 {
		t.Errorf("inflight durante = %d, want %d", got, before+1)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for inflightDeliveries.Load() != before {
		if time.Now().After(deadline) {
			t.Fatalf("inflight não voltou (got %d, want %d)", inflightDeliveries.Load(), before)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
