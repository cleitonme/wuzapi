package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempSpoolDir redireciona o spool da DLQ para um diretório temporário.
func withTempSpoolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WEBHOOK_ERROR_SPOOL_DIR", dir)
	return dir
}

func TestPublishDisabledReturnsSentinel(t *testing.T) {
	restoreRabbitState(t)

	setRabbitConnection(nil, nil, false)
	if err := PublishToRabbit([]byte(`{"x":1}`)); !errors.Is(err, ErrRabbitDisabled) {
		t.Fatalf("PublishToRabbit while disabled = %v, want ErrRabbitDisabled", err)
	}
	if err := PublishDataErrorToQueue(WebhookErrorPayload{URL: "http://x", UserID: "u"}); !errors.Is(err, ErrRabbitDisabled) {
		t.Fatalf("PublishDataErrorToQueue while disabled = %v, want ErrRabbitDisabled", err)
	}
	if err := PublishFileErrorToQueue(WebhookFileErrorPayload{URL: "http://x", UserID: "u"}); !errors.Is(err, ErrRabbitDisabled) {
		t.Fatalf("PublishFileErrorToQueue while disabled = %v, want ErrRabbitDisabled", err)
	}
}

func TestSpoolDLQToDisk(t *testing.T) {
	dir := withTempSpoolDir(t)

	payload := WebhookErrorPayload{URL: "http://destino/webhook", UserID: "user1", ErrorMessage: "conn refused"}
	path, err := spoolDLQToDisk("data", "user1", payload)
	if err != nil {
		t.Fatalf("spoolDLQToDisk returned error: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("spool file dir = %s, want %s", filepath.Dir(path), dir)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read spool file: %v", err)
	}
	var back WebhookErrorPayload
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("spool file is not valid JSON: %v", err)
	}
	if back.URL != payload.URL || back.UserID != payload.UserID {
		t.Errorf("spool content = %+v, want %+v", back, payload)
	}
}

func TestPreserveDLQFallbackToDisk(t *testing.T) {
	withTempSpoolDir(t)

	// Simula Rabbit fora: publish sempre falha com o sentinel.
	payload := WebhookErrorPayload{URL: "http://destino/webhook", UserID: "user9"}
	dest := preserveDLQ("data", func() error { return ErrRabbitDisabled }, "user9", payload)
	if dest == "rabbit" || dest == "lost" {
		t.Fatalf("preserveDLQ = %q, want disk path", dest)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("spool file missing at %s: %v", dest, err)
	}
}

func TestPreserveDLQPrefersRabbit(t *testing.T) {
	withTempSpoolDir(t)

	payload := WebhookErrorPayload{URL: "http://destino/webhook", UserID: "user9"}
	if dest := preserveDLQ("data", func() error { return nil }, "user9", payload); dest != "rabbit" {
		t.Fatalf("preserveDLQ = %q, want rabbit", dest)
	}
}

func TestWaitBackgroundCompletes(t *testing.T) {
	trackBackground(func() { time.Sleep(50 * time.Millisecond) })
	if !waitBackground(5 * time.Second) {
		t.Error("waitBackground timed out, want completion")
	}
}

func TestWaitBackgroundTimeout(t *testing.T) {
	trackBackground(func() { time.Sleep(500 * time.Millisecond) })
	if waitBackground(20 * time.Millisecond) {
		t.Error("waitBackground returned true, want timeout=false")
	}
	// Não deixa a goroutine vazar para outros testes.
	if !waitBackground(5 * time.Second) {
		t.Error("waitBackground follow-up timed out")
	}
}
