package main

// Garantia de entrega do webhook HTTP com RabbitMQ fora do ar.
//
// Decisões (sem migração de schema, reconexão limitada — ver plano aprovado):
//  1. O caminho do webhook HTTP nunca depende do RabbitMQ. Falha no Rabbit
//     só descarta o evento Rabbit (com métrica + log), nunca o webhook.
//  2. A DLQ (webhook_errors) tenta o Rabbit; se ele estiver fora, o payload
//     é preservado em spool de disco (dbdata/webhook_errors) + log ERROR.
//     Nada é descartado silenciosamente.
//  3. Entregas em background são rastreadas por WaitGroup para que o
//     graceful shutdown as aguarde até o timeout configurado.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// Métricas de entrega (só memória, expostas em /health).
var deliveryStats = struct {
	webhookSent      atomic.Uint64
	webhookFailed    atomic.Uint64
	webhookShed      atomic.Uint64 // eventos desviados p/ DLQ sem tentar HTTP (semáforo cheio/shutdown)
	historySaveFailed atomic.Uint64 // INSERTs no message_history que falharam (gap history x webhook)
	outboxDelivered  atomic.Uint64 // eventos do outbox entregues pelo worker
	outboxExpired    atomic.Uint64 // pendências purgadas por retenção (3d)
	outboxDropped    atomic.Uint64 // pendências descartadas (canal excluído / payload inválido)
	rabbitPublished  atomic.Uint64
	rabbitFailed     atomic.Uint64
	rabbitDisabled   atomic.Uint64 // eventos Rabbit descartados com broker fora
	dlqRabbit        atomic.Uint64 // DLQs entregues no Rabbit
	dlqDiskFallback  atomic.Uint64 // DLQs preservadas em disco (Rabbit fora)
}{}

// shutdownCh é fechado quando o shutdown começa. Sleeps de backoff e acquires
// de semáforo observam este canal para acordar mais cedo e escoar para a DLQ
// em vez de serem mortos no meio pelo os.Exit.
var shutdownCh = make(chan struct{})

// signalShutdown fecha shutdownCh (idempotente via sync.Once do chamador ou
// comparação com recover). Deve ser chamado uma única vez no início do shutdown.
func signalShutdown() {
	select {
	case <-shutdownCh:
		// já fechado
	default:
		close(shutdownCh)
	}
}

// shutdownStarted informa se o processo já entrou em shutdown.
func shutdownStarted() bool {
	select {
	case <-shutdownCh:
		return true
	default:
		return false
	}
}

// sleepOrShutdown dorme d mas acorda mais cedo se o shutdown começar.
// Retorna false quando interrompido (chamador deve escoar para DLQ, não retentar).
func sleepOrShutdown(d time.Duration) bool {
	if d <= 0 {
		return !shutdownStarted()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-shutdownCh:
		return false
	}
}

// bgDeliveries rastreia goroutines de entrega (webhook/Rabbit global) para o
// graceful shutdown aguardá-las.
var bgDeliveries sync.WaitGroup

// trackBackground executa fn em goroutine rastreada pelo shutdown, com
// recover para que um panic numa entrega não derrube o processo inteiro.
func trackBackground(fn func()) {
	bgDeliveries.Add(1)
	go func() {
		defer bgDeliveries.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Error().
					Interface("panic", r).
					Bytes("stack", debug.Stack()).
					Msg("Panic em entrega background (recuperado, processo preservado)")
			}
		}()
		fn()
	}()
}

// waitBackground aguarda as entregas em voo até o timeout. Retorna true se
// todas concluíram, false se o timeout estourou (nada é cancelado à força,
// só deixamos de esperar).
func waitBackground(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		bgDeliveries.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// dlqSpoolDir resolve o diretório de spool da DLQ.
// Permite override via WEBHOOK_ERROR_SPOOL_DIR; padrão: dbdata/webhook_errors.
func dlqSpoolDir() string {
	if v := os.Getenv("WEBHOOK_ERROR_SPOOL_DIR"); v != "" {
		return v
	}
	return filepath.Join("dbdata", "webhook_errors")
}

// spoolDLQToDisk preserva um payload de DLQ em disco (write atômico via
// temp + rename). Retorna o caminho final. Nunca retorna nil sem erro.
func spoolDLQToDisk(kind string, userID string, payload interface{}) (string, error) {
	dir := dlqSpoolDir()
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", fmt.Errorf("spool DLQ: mkdir %s: %w", dir, err)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("spool DLQ: marshal: %w", err)
	}

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("spool DLQ: rand: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s-%s.json",
		time.Now().UTC().Format("20060102-150405.000000000"),
		kind, sanitizeSpoolID(userID), hex.EncodeToString(rnd[:]))
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"

	if err := os.WriteFile(tmp, body, 0600); err != nil {
		return "", fmt.Errorf("spool DLQ: write tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("spool DLQ: rename: %w", err)
	}

	deliveryStats.dlqDiskFallback.Add(1)
	log.Error().
		Str("spool_file", final).
		Str("kind", kind).
		Str("userID", userID).
		Msg("DLQ preservada em disco (RabbitMQ indisponível) — replay manual a partir do arquivo")
	return final, nil
}

// preserveDLQ tenta a DLQ no Rabbit e, em qualquer falha (inclusive broker
// fora — ErrRabbitDisabled), preserva o payload em spool de disco. O webhook
// HTTP já esgotou seus retries neste ponto; esta é a última linha de defesa
// contra perda silenciosa. Retorna o destino ("rabbit" ou caminho do arquivo).
func preserveDLQ(kind string, publish func() error, userID string, payload interface{}) string {
	if err := publish(); err == nil {
		return "rabbit"
	} else {
		log.Warn().Err(err).Str("kind", kind).Str("userID", userID).
			Msg("DLQ no RabbitMQ falhou, preservando em disco")
	}
	if path, serr := spoolDLQToDisk(kind, userID, payload); serr != nil {
		log.Error().Err(serr).Str("kind", kind).Str("userID", userID).
			Msg("FALHA CRÍTICA: DLQ perdida — nem Rabbit nem disco aceitaram o payload")
		return "lost"
	} else {
		return path
	}
}

func sanitizeSpoolID(s string) string {
	if s == "" {
		return "noid"
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		b[i] = '_'
	}
	if len(b) > 32 {
		b = b[:32]
	}
	return string(b)
}
