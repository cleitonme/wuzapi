package main

import (
	"testing"
	"time"
)

// O sleep curto e sem shutdown deve completar normalmente.
func TestSleepOrShutdownCompletes(t *testing.T) {
	if shutdownStarted() {
		t.Skip("shutdown já sinalizado por outro teste")
	}
	if !sleepOrShutdown(20 * time.Millisecond) {
		t.Error("sleepOrShutdown retornou false sem shutdown, want true")
	}
	if sleepOrShutdown(0) != true {
		t.Error("sleepOrShutdown(0) = false, want true (sem shutdown)")
	}
}

// Semáforo cheio: acquire com timeout curto deve falhar rápido (nunca travar)
// para que o chamador escoe para DLQ.
func TestWebhookAcquireTimeoutSheds(t *testing.T) {
	userID := "test-shed-user"
	sema := webhookSemaFor(userID)
	// Preenche todos os slots.
	for i := 0; i < webhookMaxInflightPerUser; i++ {
		sema <- struct{}{}
	}
	// Drena no fim para não vazar estado.
	defer func() {
		for i := 0; i < webhookMaxInflightPerUser; i++ {
			<-sema
		}
	}()

	start := time.Now()
	_, ok := webhookAcquireTimeout(userID, 50*time.Millisecond)
	elapsed := time.Since(start)
	if ok {
		t.Fatal("webhookAcquireTimeout com semáforo cheio = ok, want shed (false)")
	}
	if elapsed > 5*time.Second {
		t.Errorf("webhookAcquireTimeout demorou %v, want ~50ms (travou?)", elapsed)
	}
}

// Semáforo livre: acquire deve ter sucesso e release deve devolver a vaga.
func TestWebhookAcquireTimeoutSuccess(t *testing.T) {
	userID := "test-acquire-user"
	release, ok := webhookAcquireTimeout(userID, time.Second)
	if !ok {
		t.Fatal("webhookAcquireTimeout com semáforo livre = shed, want ok")
	}
	release()
	// Segunda aquisição imediata prova que a vaga voltou.
	release2, ok := webhookAcquireTimeout(userID, time.Second)
	if !ok {
		t.Fatal("vaga não devolvida após release")
	}
	release2()
}

// stringMapToInterface deve preservar todas as chaves.
func TestStringMapToInterface(t *testing.T) {
	in := map[string]string{"a": "1", "b": "2"}
	out := stringMapToInterface(in)
	if len(out) != 2 || out["a"] != "1" || out["b"] != "2" {
		t.Errorf("stringMapToInterface = %v, want %v", out, in)
	}
}
