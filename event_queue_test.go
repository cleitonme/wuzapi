package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// TestWebhookRetryZeroStillDelivers garante que retrycount=0 (ou negativo)
// não anula a primeira tentativa de entrega.
func TestWebhookRetryZeroStillDelivers(t *testing.T) {
	saveRestoreRetryFlags(t)
	*webhookRetryEnabled = true
	*webhookRetryCount = 0
	*webhookRetryDelaySeconds = 0

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	userID := "retry-zero-user"
	setTestHTTPClient(t, userID)
	payload := map[string]string{"jsonData": `{"type":"Message"}`, "userID": userID}
	callHook(srv.URL, payload, userID)

	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (retrycount=0 não pode descartar a entrega)", hits.Load())
	}
}

func TestEventQueueFullFallsBackInline(t *testing.T) {
	if shutdownStarted() {
		t.Skip("shutdown já sinalizado por outro teste")
	}
	mycli := &MyClient{userID: "queue-backpressure-test"}
	mycli.eventQueue = make(chan eventJob, 1)
	mycli.eventQueue <- eventJob{raw: &events.AppState{}}

	before := deliveryStats.eventQueueBackpressure.Load()
	mycli.enqueueEvent(&events.AppState{})

	if got := deliveryStats.eventQueueBackpressure.Load() - before; got != 1 {
		t.Fatalf("eventQueueBackpressure delta = %d, want 1", got)
	}
	if len(mycli.eventQueue) != 1 {
		t.Fatalf("fila não deveria crescer além do limite: len=%d", len(mycli.eventQueue))
	}
}

func TestEventWorkersDrainQueue(t *testing.T) {
	mycli := &MyClient{userID: "worker-drain-test"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mycli.startEventWorkers(ctx)

	mycli.enqueueEvent(&events.AppState{})

	deadline := time.Now().Add(3 * time.Second)
	for len(mycli.eventQueue) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker não consumiu o evento enfileirado")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSubscriptionFallbackDeliversWhenLookupFails: erro ao ler subscriptions
// não pode descartar o evento; usa a última lista conhecida e entrega.
func TestSubscriptionFallbackDeliversWhenLookupFails(t *testing.T) {
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, token TEXT, webhook TEXT)`); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	userID, token := "sub-fallback-user", "sub-fallback-token"
	if _, err := db.Exec(`INSERT INTO users (id, token, webhook) VALUES (?, ?, ?)`, userID, token, srv.URL); err != nil {
		t.Fatal(err)
	}
	setTestHTTPClient(t, userID)

	mycli := &MyClient{
		userID:        userID,
		token:         token,
		db:            db,
		subscriptions: []string{"Message"},
	}

	before := deliveryStats.subscriptionFallback.Load()
	sendEventWithWebHook(mycli, map[string]interface{}{
		"type":  "Message",
		"event": &events.Message{},
	}, "")
	if got := deliveryStats.subscriptionFallback.Load() - before; got != 1 {
		t.Fatalf("subscriptionFallback delta = %d, want 1", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("webhook não entregue após fallback de subscriptions")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGroupReceiptsIgnoredByDefault(t *testing.T) {
	old := *forwardGroupReceipts
	*forwardGroupReceipts = false
	t.Cleanup(func() { *forwardGroupReceipts = old })

	mycli := &MyClient{userID: "group-receipt-default"}
	mycli.handleEvent(&events.Receipt{
		MessageSource: types.MessageSource{IsGroup: true},
		MessageIDs:    []types.MessageID{"3EB0GROUP"},
		Type:          types.ReceiptTypeRead,
	})
}

func TestStartSocketRecoveryRegistersSession(t *testing.T) {
	old := *socketRecoveryEnabled
	*socketRecoveryEnabled = true
	t.Cleanup(func() { *socketRecoveryEnabled = old })

	userID := "socket-recovery-wiring"
	client := &whatsmeow.Client{}
	clientManager.SetWhatsmeowClient(userID, client)
	ctx, cancel := context.WithCancel(context.Background())

	recovery := startSocketRecovery(ctx, cancel, userID, client)
	if recovery == nil {
		t.Fatal("startSocketRecovery não registrou a sessão")
	}
	t.Cleanup(func() {
		clientManager.removeSocketRecovery(userID, recovery)
		clientManager.DeleteWhatsmeowClient(userID)
		cancel()
	})

	clientManager.RLock()
	got := clientManager.socketRecoveries[userID]
	clientManager.RUnlock()
	if got != recovery {
		t.Fatal("recovery não ficou registrada no clientManager")
	}
}
