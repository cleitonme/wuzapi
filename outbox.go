package main

// Outbox persistente de webhooks (tabela webhook_outbox, migration 12).
//
// Garantia: toda falha definitiva de entrega do webhook do usuário é gravada
// no banco e o worker tenta reenviar em backoff enquanto o canal (usuário)
// existir. A pendência só morre quando: entregue (2xx), canal excluído, ou
// retenção estourada (OUTBOX_MAX_AGE_DAYS, default 3d).
//
// Decisões do plano aprovado:
//  1. Entrega usa sempre a URL ATUAL do usuário (users.webhook na hora da
//     tentativa); a URL do enqueue é só snapshot/fallback.
//  2. Usuário existe mas sem webhook -> mantém pendente, sem tentativa HTTP.
//  3. Mídia vai inline no payload como no webhook normal.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog/log"
)

type outboxConfig struct {
	enabled     bool
	pollSeconds int
	batch       int
	baseSeconds int
	maxSeconds  int
	maxAgeDays  int
	// workers limita quantas entregas concorrem por tick (só Postgres;
	// SQLite segue sequencial por causa do lock de escrita). 1 = sequencial.
	workers int
	// ordered força entrega sequencial (ordem por next_retry_at) mesmo no
	// Postgres — para consumidores que exigem ordenação global.
	ordered bool
	// httpTimeoutSeconds é o timeout de cada POST do worker (menor que os
	// 30s do caminho ao vivo) para receptor lento não prender o lote.
	httpTimeoutSeconds int
}

// outboxForceConcurrent é seam de teste: permite exercitar o caminho
// concorrente fora do Postgres (ex.: SQLite em teste, com MaxOpenConns(1)).
var outboxForceConcurrent = false

func loadOutboxConfig() outboxConfig {
	cfg := outboxConfig{
		enabled:            true,
		pollSeconds:        30,
		batch:              100,
		baseSeconds:        60,
		maxSeconds:         3600,
		maxAgeDays:         3,
		workers:            4,
		ordered:            false,
		httpTimeoutSeconds: 20,
	}
	if v := os.Getenv("OUTBOX_ENABLED"); v != "" {
		cfg.enabled = !(strings.ToLower(v) == "false" || v == "0")
	}
	if v := os.Getenv("OUTBOX_POLL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 && n <= 3600 {
			cfg.pollSeconds = n
		}
	}
	if v := os.Getenv("OUTBOX_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1000 {
			cfg.batch = n
		}
	}
	if v := os.Getenv("OUTBOX_RETRY_BASE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 && n <= 3600 {
			cfg.baseSeconds = n
		}
	}
	if v := os.Getenv("OUTBOX_RETRY_MAX_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= cfg.baseSeconds && n <= 86400 {
			cfg.maxSeconds = n
		}
	}
	if v := os.Getenv("OUTBOX_MAX_AGE_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 365 {
			cfg.maxAgeDays = n
		}
	}
	if v := os.Getenv("OUTBOX_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 32 {
			cfg.workers = n
		}
	}
	if v := os.Getenv("OUTBOX_ORDERED_DELIVERY"); v != "" {
		cfg.ordered = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("OUTBOX_HTTP_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 && n <= 120 {
			cfg.httpTimeoutSeconds = n
		}
	}
	return cfg
}

// outboxBackoff calcula o atraso da próxima tentativa: base*2^attempts com teto.
func outboxBackoff(baseSeconds, maxSeconds, attempts int) time.Duration {
	d := time.Duration(baseSeconds) * time.Second
	for i := 0; i < attempts; i++ {
		d *= 2
		if d >= time.Duration(maxSeconds)*time.Second {
			return time.Duration(maxSeconds) * time.Second
		}
	}
	return d
}

type outboxItem struct {
	ID        int64  `db:"id"`
	UserID    string `db:"user_id"`
	EventType string `db:"event_type"`
	Payload   string `db:"payload"`
	Attempts  int    `db:"attempts"`
}

// extractOutboxMeta tira eventType/messageID (melhor esforço, só
// observabilidade) do payload do webhook.
func extractOutboxMeta(data map[string]string) (eventType, messageID string) {
	eventType = "unknown"
	raw, ok := data["jsonData"]
	if !ok {
		return eventType, ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return eventType, ""
	}
	if t, ok := m["type"].(string); ok && t != "" {
		eventType = t
	}
	for _, k := range []string{"messageId", "messageID", "message_id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return eventType, v
		}
	}
	if info, ok := m["Info"].(map[string]interface{}); ok {
		if v, ok := info["ID"].(string); ok {
			return eventType, v
		}
	}
	return eventType, ""
}

func truncateOutboxErr(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// enqueueOutbox grava uma entrega falhada no outbox. Retorna erro para que o
// chamador caia no caminho legado (DLQ Rabbit -> spool em disco) se o DB
// estiver fora. Nunca bloqueia o handler: INSERT único e rápido.
func enqueueOutbox(db *sqlx.DB, userID, webhookURL string, data map[string]string, lastErr string) error {
	if db == nil {
		return fmt.Errorf("outbox: db nil")
	}
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload: %w", err)
	}
	eventType, messageID := extractOutboxMeta(data)
	cfg := loadOutboxConfig()
	q := db.Rebind(`INSERT INTO webhook_outbox
		(user_id, webhook_url, event_type, message_id, payload, next_retry_at, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	_, err = db.Exec(q, userID, webhookURL, eventType, messageID, string(body),
		time.Now().Add(time.Duration(cfg.baseSeconds)*time.Second), truncateOutboxErr(lastErr))
	if err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	log.Info().Str("userID", userID).Str("eventType", eventType).Msg("Evento persistido no outbox (retry contínuo pelo worker)")
	return nil
}

// persistFailedWebhook é o destino final de uma entrega que esgotou tudo:
// tenta o outbox (DB); se o DB estiver fora, cai no caminho legado
// (DLQ Rabbit -> spool em disco). Nada é descartado silenciosamente.
func persistFailedWebhook(userID, webhookURL string, data map[string]string, lastErr string, legacyPayload WebhookErrorPayload) {
	if outboxDB != nil {
		if err := enqueueOutbox(outboxDB, userID, webhookURL, data, lastErr); err == nil {
			return
		} else {
			log.Error().Err(err).Str("userID", userID).Msg("Outbox indisponível, caindo para DLQ legada...")
		}
	}
	preserveDLQ("data", func() error { return PublishDataErrorToQueue(legacyPayload) }, userID, legacyPayload)
}

// outboxDB é o handle do banco para o outbox, definido no boot (main.go).
// Separado para que helpers.go não dependa de *server.
var outboxDB *sqlx.DB

var outboxWg sync.WaitGroup

// startOutboxWorker inicia o worker de retry contínuo. Sweep imediato no boot
// retoma pendências de antes do restart.
func startOutboxWorker(db *sqlx.DB, stopCh <-chan struct{}) {
	outboxDB = db
	cfg := loadOutboxConfig()
	if !cfg.enabled {
		log.Info().Msg("Outbox worker desabilitado (OUTBOX_ENABLED=false)")
		return
	}
	outboxWg.Add(1)
	go func() {
		defer outboxWg.Done()
		outboxTick(db, cfg, stopCh)
		t := time.NewTicker(time.Duration(cfg.pollSeconds) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				outboxTick(db, cfg, stopCh)
			}
		}
	}()
	log.Info().
		Int("poll_seconds", cfg.pollSeconds).
		Int("batch", cfg.batch).
		Int("workers", cfg.workers).
		Bool("ordered", cfg.ordered).
		Int("http_timeout_seconds", cfg.httpTimeoutSeconds).
		Int("max_age_days", cfg.maxAgeDays).
		Msg("Outbox worker iniciado (retry contínuo de webhooks falhados)")
}

// stopOutboxWorker aguarda o loop do worker (chamado no shutdown).
func stopOutboxWorker() {
	outboxWg.Wait()
}

// outboxTick: purga expirados + processa lote vencido. Para entre itens se o
// shutdown chegar (leases de 5min devolvem o resto no próximo boot).
// Erros de ciclo (ex.: tabela ausente) disparam UMA tentativa de ensure e têm
// log com throttle para não spammar a cada poll.
func outboxTick(db *sqlx.DB, cfg outboxConfig, stopCh <-chan struct{}) {
	if db == nil {
		return
	}
	if err := outboxCycle(db, cfg, stopCh); err != nil {
		outboxConsecFail++
		if isMissingRelationErr(err) && !outboxEnsureDone {
			outboxEnsureDone = true
			log.Warn().Msg("Outbox: tabela ausente, tentando ensure e repetindo o ciclo...")
			if serr := ensureWebhookOutbox(db); serr != nil {
				log.Error().Err(serr).Msg("Outbox: ensure falhou")
			} else if cerr := outboxCycle(db, cfg, stopCh); cerr != nil {
				outboxConsecFail++
				logOutboxCycleErr(cerr)
			} else {
				outboxConsecFail = 0
			}
			return
		}
		logOutboxCycleErr(err)
		return
	}
	outboxConsecFail = 0
}

// logOutboxCycleErr: primeiro erro e a cada 20º em ERROR, resto em DEBUG.
func logOutboxCycleErr(err error) {
	if outboxConsecFail <= 1 || outboxConsecFail%20 == 0 {
		log.Error().Err(err).Int("falhas_consecutivas", outboxConsecFail).Msg("Outbox: falha no ciclo")
	} else {
		log.Debug().Err(err).Int("falhas_consecutivas", outboxConsecFail).Msg("Outbox: falha no ciclo (throttled)")
	}
}

// outboxConsecFail conta falhas seguidas de ciclo; outboxEnsureDone marca a
// tentativa única de auto-cura por processo.
var (
	outboxConsecFail int
	outboxEnsureDone bool
)

// isMissingRelationErr detecta tabela ausente (pq/sqlite).
func isMissingRelationErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "does not exist") || strings.Contains(s, "no such table")
}

// outboxCycle executa um ciclo completo; erro aborta o ciclo (itens já
// reclamados mantêm o lease e voltam no próximo).
func outboxCycle(db *sqlx.DB, cfg outboxConfig, stopCh <-chan struct{}) error {
	if err := purgeOutboxExpired(db, cfg); err != nil {
		return err
	}
	items, err := claimOutboxDue(db, cfg)
	if err != nil {
		return err
	}
	deliverOutboxBatch(db, cfg, stopCh, items)
	return nil
}

// deliverOutboxBatch entrega o lote reclamado. SQLite e modo ordenado:
// sequencial (ordem por next_retry_at, comportamento histórico). Postgres com
// workers>1: concorrência limitada — os itens já foram reclamados com lease
// de 5min (SKIP LOCKED), então cada worker processa itens distintos; o que
// não chegar a iniciar até o stop mantém o lease e volta no próximo ciclo ou
// boot. Sem duplicatas: cada item é entregue por exatamente um worker.
func deliverOutboxBatch(db *sqlx.DB, cfg outboxConfig, stopCh <-chan struct{}, items []outboxItem) {
	if len(items) == 0 {
		return
	}
	if cfg.workers <= 1 || cfg.ordered || (db.DriverName() != "postgres" && !outboxForceConcurrent) {
		for _, it := range items {
			select {
			case <-stopCh:
				log.Warn().Int("restantes", len(items)).Msg("Outbox: shutdown no meio do lote (leases devolvem o resto)")
				return
			default:
			}
			deliverOutboxItem(db, cfg, it)
		}
		return
	}
	sem := make(chan struct{}, cfg.workers)
	var wg sync.WaitGroup
	stopped := false
	for _, it := range items {
		if !stopped {
			select {
			case <-stopCh:
				stopped = true
				log.Warn().Msg("Outbox: shutdown no meio do lote (leases devolvem o resto)")
			default:
			}
		}
		if stopped {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(item outboxItem) {
			defer wg.Done()
			defer func() { <-sem }()
			deliverOutboxItem(db, cfg, item)
		}(it)
	}
	wg.Wait()
}

// purgeOutboxExpired apaga pendências além da retenção (default 3d).
func purgeOutboxExpired(db *sqlx.DB, cfg outboxConfig) error {
	cutoff := time.Now().AddDate(0, 0, -cfg.maxAgeDays)
	q := db.Rebind(`DELETE FROM webhook_outbox WHERE created_at < ?`)
	res, err := db.Exec(q, cutoff)
	if err != nil {
		return fmt.Errorf("purga expirados: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		deliveryStats.outboxExpired.Add(uint64(n))
		log.Warn().Int64("purgados", n).Int("max_age_days", cfg.maxAgeDays).Msg("Outbox: pendências expiradas purgadas (retenção)")
	}
	return nil
}

// claimOutboxDue reclama até batch itens vencidos com lease de 5min.
// PostgreSQL usa SKIP LOCKED (seguro multi-worker); SQLite, processo único.
func claimOutboxDue(db *sqlx.DB, cfg outboxConfig) ([]outboxItem, error) {
	var items []outboxItem
	now := time.Now()
	leaseCutoff := now.Add(-5 * time.Minute)
	if db.DriverName() == "postgres" {
		q := `UPDATE webhook_outbox SET claimed_at = NOW()
			WHERE id IN (
				SELECT id FROM webhook_outbox
				WHERE next_retry_at <= NOW()
				  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
				ORDER BY next_retry_at ASC
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)
		RETURNING id, user_id, event_type, payload, attempts`
		if err := db.Select(&items, q, cfg.batch); err != nil {
			return nil, fmt.Errorf("claim postgres: %w", err)
		}
		return items, nil
	}
	// SQLite: SELECT + UPDATE otimista (processo único).
	q := db.Rebind(`SELECT id, user_id, event_type, payload, attempts
		FROM webhook_outbox
		WHERE next_retry_at <= ? AND (claimed_at IS NULL OR claimed_at < ?)
		ORDER BY next_retry_at ASC LIMIT ?`)
	if err := db.Select(&items, q, now, leaseCutoff, cfg.batch); err != nil {
		return nil, fmt.Errorf("claim sqlite: %w", err)
	}
	for _, it := range items {
		uq := db.Rebind(`UPDATE webhook_outbox SET claimed_at = ?
			WHERE id = ? AND (claimed_at IS NULL OR claimed_at < ?)`)
		if _, err := db.Exec(uq, now, it.ID, leaseCutoff); err != nil {
			log.Warn().Err(err).Int64("id", it.ID).Msg("Outbox: falha ao marcar claim")
		}
	}
	return items, nil
}

// deliverOutboxItem tenta UMA entrega. Regras (plano aprovado):
// - canal excluído -> DELETE (para de tentar);
// - sem webhook configurado -> reagenda, mantém;
// - 2xx -> DELETE; falha -> backoff exponencial com teto.
func deliverOutboxItem(db *sqlx.DB, cfg outboxConfig, it outboxItem) {
	// Canal ainda existe? (users.webhook atual = URL de entrega.)
	var currentWebhook string
	q := db.Rebind(`SELECT webhook FROM users WHERE id = ?`)
	err := db.Get(&currentWebhook, q, it.UserID)
	if err != nil {
		// Canal apagado (ou DB inconsistente na leitura): apaga a pendência.
		// Distingue "not found" de erro real de banco.
		if isNotFoundErr(err) {
			deleteOutboxItem(db, it.ID)
			deliveryStats.outboxDropped.Add(1)
			log.Info().Str("userID", it.UserID).Str("eventType", it.EventType).Msg("Outbox: canal excluído, pendência descartada")
		} else {
			releaseOutboxItem(db, cfg, it, "falha ao ler usuário: "+truncateOutboxErr(err.Error()))
		}
		return
	}

	targetURL := currentWebhook
	if targetURL == "" {
		// Usuário existe mas sem webhook: mantém pendente, sem tentativa HTTP.
		releaseOutboxItem(db, cfg, it, "sem webhook configurado (aguardando)")
		return
	}

	var data map[string]string
	if err := json.Unmarshal([]byte(it.Payload), &data); err != nil {
		// Payload corrompido: nunca vai entregar, apaga para não girar para sempre.
		deleteOutboxItem(db, it.ID)
		deliveryStats.outboxDropped.Add(1)
		log.Error().Err(err).Int64("id", it.ID).Msg("Outbox: payload inválido, pendência descartada")
		return
	}

	client := clientManager.GetHTTPClient(it.UserID)
	if client == nil {
		// Sessão desconectada: sem HTTP client não há como POSTar. Mantém.
		releaseOutboxItem(db, cfg, it, "sem HTTP client (sessão desconectada)")
		return
	}

	if perr := doHookPostWithTimeout(client, targetURL, data, it.UserID, time.Duration(cfg.httpTimeoutSeconds)*time.Second); perr != nil {
		failOutboxItem(db, cfg, it, perr.Error())
		return
	}

	deleteOutboxItem(db, it.ID)
	deliveryStats.outboxDelivered.Add(1)
	deliveryStats.webhookSent.Add(1)
	log.Info().Str("userID", it.UserID).Str("eventType", it.EventType).Int("attempts", it.Attempts+1).Msg("Outbox: evento entregue")
}

// failOutboxItem reagenda com backoff exponencial (teto configurado).
func failOutboxItem(db *sqlx.DB, cfg outboxConfig, it outboxItem, errMsg string) {
	next := time.Now().Add(outboxBackoff(cfg.baseSeconds, cfg.maxSeconds, it.Attempts))
	q := db.Rebind(`UPDATE webhook_outbox
		SET attempts = attempts + 1, next_retry_at = ?, claimed_at = NULL, last_error = ?
		WHERE id = ?`)
	if _, err := db.Exec(q, next, truncateOutboxErr(errMsg), it.ID); err != nil {
		log.Error().Err(err).Int64("id", it.ID).Msg("Outbox: falha ao reagendar")
		return
	}
	log.Warn().Str("userID", it.UserID).Str("eventType", it.EventType).
		Int("attempts", it.Attempts+1).Time("next_retry", next).Msg("Outbox: tentativa falhou, reagendado")
}

// releaseOutboxItem devolve o item para a fila sem contar tentativa
// (condição transitória: sem webhook, sem client, erro de leitura).
func releaseOutboxItem(db *sqlx.DB, cfg outboxConfig, it outboxItem, reason string) {
	next := time.Now().Add(time.Duration(cfg.pollSeconds) * time.Second)
	q := db.Rebind(`UPDATE webhook_outbox
		SET next_retry_at = ?, claimed_at = NULL, last_error = ?
		WHERE id = ?`)
	if _, err := db.Exec(q, next, truncateOutboxErr(reason), it.ID); err != nil {
		log.Error().Err(err).Int64("id", it.ID).Msg("Outbox: falha ao devolver item")
		return
	}
	log.Debug().Str("userID", it.UserID).Str("reason", reason).Msg("Outbox: item devolvido (sem tentativa HTTP)")
}

func deleteOutboxItem(db *sqlx.DB, id int64) {
	q := db.Rebind(`DELETE FROM webhook_outbox WHERE id = ?`)
	if _, err := db.Exec(q, id); err != nil {
		log.Error().Err(err).Int64("id", id).Msg("Outbox: falha ao deletar item")
	}
}

// isNotFoundErr distingue "sem linhas" de erro real de banco.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no rows") || strings.Contains(s, "not found")
}

// outboxPendingStats retorna pendências e idade da mais antiga (para /health).
func outboxPendingStats(db *sqlx.DB) (pending int64, oldestAgeSeconds int64) {
	if db == nil {
		return 0, 0
	}
	_ = db.Get(&pending, `SELECT COUNT(*) FROM webhook_outbox`)
	var oldest time.Time
	if err := db.Get(&oldest, `SELECT COALESCE(MIN(created_at), CURRENT_TIMESTAMP) FROM webhook_outbox`); err != nil {
		return pending, 0
	}
	if pending > 0 {
		if age := time.Since(oldest); age > 0 {
			oldestAgeSeconds = int64(age.Seconds())
		}
	}
	return pending, oldestAgeSeconds
}
