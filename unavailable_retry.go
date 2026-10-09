package main

// Auto-retry (em memória) para mensagens que falharam na descriptografia.
//
// Contexto: o whatsmeow já pede automaticamente ao remetente o reenvio quando
// recebe um ciphertext indecifrável, e o handler manual
// POST /chat/request-unavailable-message pede a cópia ao device primário
// (peer). Este arquivo adiciona a camada automática desse segundo pedido,
// com backoff e correlação da resposta — sem persistência em banco
// (decisão do projeto: tudo em memória; um restart limpa os pendentes).
//
// Fluxo (todo silencioso: nenhum webhook novo é emitido):
//  1. myEventHandler recebe events.UndecryptableMessage com campos
//     enriquecidos e chama scheduleUnavailableAutoRetry (não-bloqueante).
//  2. Cada tentativa envia BuildUnavailableMessageRequest via SendMessage
//     com Peer:true e guarda o request ID retornado.
//  3. Quando um events.Message chega com UnavailableRequestID preenchido
//     (ou com o mesmo ID da mensagem original), markUnavailableRecovered
//     cancela os timers restantes.
//  4. Esgotadas as tentativas (ou TTL), o pendente é descartado.
// O consumidor recebe apenas o UndecryptableMessage original; o resultado
// do retry aparece só nos logs (INFO recupera/envia, WARN desiste).

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	// Número total de pedidos peer por mensagem indecifrável (1 imediato + agendados).
	undecryptableAutoRetryMaxAttempts = 3
	// TTL de um pendente: após isso, desiste e emite GaveUp.
	undecryptablePendingTTL = 15 * time.Minute
	// Teto de pendentes por usuário (proteção de memória).
	undecryptableMaxPendingPerUser = 500
	// Intervalo mínimo entre dois pedidos peer do mesmo usuário (evita flood/ban).
	undecryptableMinIntervalPerUser = 2 * time.Second
	// Timeout de cada envio do pedido.
	undecryptableRequestTimeout = 15 * time.Second
	// Teto do jitter adicionado a cada atraso agendado.
	undecryptableRetryJitterMax = 5 * time.Second
)

var undecryptableRetryDelays = []time.Duration{
	0,                 // tentativa 1: imediata
	30 * time.Second,  // tentativa 2
	120 * time.Second, // tentativa 3
}

// Contadores globais (só memória, para logs/métricas futuras).
var undecryptableStats = struct {
	total     atomic.Uint64
	retrySent atomic.Uint64
	recovered atomic.Uint64
	gaveUp    atomic.Uint64
	skipped   atomic.Uint64 // ex.: view_once, intencionalmente indisponível
}{}

type undecryptablePending struct {
	userID string
	chat   types.JID
	sender types.JID
	msgID  string

	mu         sync.Mutex
	timers     []*time.Timer
	attempts   int
	requestIDs []string
	done       atomic.Bool
	firstSeen  time.Time
}

var (
	// key = userID + "|" + messageID -> *undecryptablePending
	undecPending sync.Map
	// requestID (do pedido peer enviado) -> key do pendente
	undecReqIndex sync.Map

	undecMu       sync.Mutex
	undecPerUser  = make(map[string]int)
	undecLastSend = make(map[string]time.Time)
)

func undecPendingKey(userID, msgID string) string {
	return userID + "|" + msgID
}

// sendUnavailableMessageRequest é o caminho único (manual via API e automático)
// para pedir ao device primário a cópia de uma mensagem indecifrável.
func sendUnavailableMessageRequest(client *whatsmeow.Client, chatJID, senderJID types.JID, id string, timeout time.Duration) (whatsmeow.SendResponse, error) {
	unavailableMessage := client.BuildUnavailableMessageRequest(chatJID, senderJID, id)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.SendMessage(ctx, chatJID, unavailableMessage, whatsmeow.SendRequestExtra{Peer: true})
}

// scheduleUnavailableAutoRetry agenda os pedidos peer para uma mensagem
// indecifrável. Não-bloqueante: nunca dorme na goroutine do chamador.
func scheduleUnavailableAutoRetry(mycli *MyClient, chat, sender types.JID, msgID, unavailableType string) {
	if mycli == nil || msgID == "" {
		return
	}
	// view_once é intencionalmente indisponível: reenvio não vai ajudar.
	if unavailableType == "view_once" {
		undecryptableStats.skipped.Add(1)
		log.Debug().
			Str("userID", mycli.userID).
			Str("messageID", msgID).
			Msg("Auto-retry ignorado: mensagem view_once intencionalmente indisponível")
		return
	}

	userID := mycli.userID
	key := undecPendingKey(userID, msgID)
	if _, exists := undecPending.Load(key); exists {
		log.Debug().Str("key", key).Msg("Auto-retry já agendado (duplicata ignorada)")
		return
	}

	undecMu.Lock()
	if undecPerUser[userID] >= undecryptableMaxPendingPerUser {
		undecMu.Unlock()
		undecryptableStats.skipped.Add(1)
		log.Warn().
			Str("userID", userID).
			Int("pending", undecryptableMaxPendingPerUser).
			Msg("Teto de pendentes atingido, auto-retry recusado (só memória)")
		return
	}
	undecPerUser[userID]++
	undecMu.Unlock()

	entry := &undecryptablePending{
		userID:    userID,
		chat:      chat,
		sender:    sender,
		msgID:     msgID,
		firstSeen: time.Now(),
	}
	undecPending.Store(key, entry)
	undecryptableStats.total.Add(1)

	// Tentativa imediata em background.
	go undecDoAttempt(key, 0)

	// Tentativas agendadas + watchdog de TTL.
	entry.mu.Lock()
	defer entry.mu.Unlock()
	for attempt := 1; attempt < undecryptableAutoRetryMaxAttempts && attempt < len(undecryptableRetryDelays); attempt++ {
		delay := undecryptableRetryDelays[attempt] + time.Duration(rand.Int63n(int64(undecryptableRetryJitterMax)))
		a := attempt // captura para o closure
		t := time.AfterFunc(delay, func() { undecDoAttempt(key, a) })
		entry.timers = append(entry.timers, t)
	}
	ttl := time.AfterFunc(undecryptablePendingTTL, func() { undecGiveUp(key, "ttl") })
	entry.timers = append(entry.timers, ttl)
}

// undecDoAttempt executa uma tentativa de pedido peer. Roda em goroutine
// própria (timer ou imediata), nunca na thread do whatsmeow.
func undecDoAttempt(key string, attempt int) {
	v, ok := undecPending.Load(key)
	if !ok {
		return
	}
	entry := v.(*undecryptablePending)
	if entry.done.Load() {
		return
	}

	// Throttle por usuário.
	undecMu.Lock()
	wait := undecryptableMinIntervalPerUser - time.Since(undecLastSend[entry.userID])
	undecMu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
	if entry.done.Load() {
		return
	}

	client := clientManager.GetWhatsmeowClient(entry.userID)
	if client == nil {
		log.Warn().
			Str("userID", entry.userID).
			Str("messageID", entry.msgID).
			Int("attempt", attempt+1).
			Msg("Auto-retry adiado: sessão desconectada (mantém agendamento)")
		return
	}

	resp, err := sendUnavailableMessageRequest(client, entry.chat, entry.sender, entry.msgID, undecryptableRequestTimeout)
	if err != nil {
		log.Warn().
			Err(err).
			Str("userID", entry.userID).
			Str("messageID", entry.msgID).
			Int("attempt", attempt+1).
			Msg("Pedido peer automático falhou")
		return
	}

	undecMu.Lock()
	undecLastSend[entry.userID] = time.Now()
	undecMu.Unlock()

	requestID := string(resp.ID)
	entry.mu.Lock()
	entry.attempts++
	entry.requestIDs = append(entry.requestIDs, requestID)
	entry.mu.Unlock()
	undecReqIndex.Store(requestID, key)
	undecryptableStats.retrySent.Add(1)

	log.Info().
		Str("userID", entry.userID).
		Str("messageID", entry.msgID).
		Str("requestID", requestID).
		Int("attempt", attempt+1).
		Msg("Pedido peer automático enviado")
}

// markUnavailableRecovered resolve um pendente quando a cópia chega.
// Chamado pelo handler de Message com o ID original e/ou o request ID.
func markUnavailableRecovered(userID, originalMsgID, requestID string) {
	if userID == "" {
		return
	}
	// 1) Correlação pelo request ID (resposta ao nosso pedido peer).
	if requestID != "" {
		if v, ok := undecReqIndex.Load(requestID); ok {
			undecResolve(v.(string), "request-id")
		}
	}
	// 2) Correlação pelo ID original (reenvio do remetente com mesmo ID).
	if originalMsgID != "" {
		if _, ok := undecPending.Load(undecPendingKey(userID, originalMsgID)); ok {
			undecResolve(undecPendingKey(userID, originalMsgID), "same-id")
		}
	}
}

func undecResolve(key, reason string) {
	v, ok := undecPending.Load(key)
	if !ok {
		return
	}
	entry := v.(*undecryptablePending)
	if !entry.done.CompareAndSwap(false, true) {
		return // já resolvido por outra correlação
	}
	entry.mu.Lock()
	timers := entry.timers
	entry.timers = nil
	reqIDs := append([]string(nil), entry.requestIDs...)
	attempts := entry.attempts
	entry.mu.Unlock()
	for _, t := range timers {
		t.Stop()
	}
	undecPending.Delete(key)
	for _, id := range reqIDs {
		undecReqIndex.Delete(id)
	}
	undecMu.Lock()
	if undecPerUser[entry.userID] > 0 {
		undecPerUser[entry.userID]--
	}
	undecMu.Unlock()
	undecryptableStats.recovered.Add(1)

	log.Info().
		Str("userID", entry.userID).
		Str("messageID", entry.msgID).
		Str("reason", reason).
		Int("attempts", attempts).
		Msg("Mensagem indecifrável recuperada")
}

// undecGiveUp encerra um pendente sem recuperação (tentativas esgotadas/TTL).
func undecGiveUp(key, reason string) {
	v, ok := undecPending.Load(key)
	if !ok {
		return
	}
	entry := v.(*undecryptablePending)
	if !entry.done.CompareAndSwap(false, true) {
		return
	}
	entry.mu.Lock()
	timers := entry.timers
	entry.timers = nil
	reqIDs := append([]string(nil), entry.requestIDs...)
	attempts := entry.attempts
	entry.mu.Unlock()
	for _, t := range timers {
		t.Stop()
	}
	undecPending.Delete(key)
	for _, id := range reqIDs {
		undecReqIndex.Delete(id)
	}
	undecMu.Lock()
	if undecPerUser[entry.userID] > 0 {
		undecPerUser[entry.userID]--
	}
	undecMu.Unlock()
	undecryptableStats.gaveUp.Add(1)

	log.Warn().
		Str("userID", entry.userID).
		Str("messageID", entry.msgID).
		Str("reason", reason).
		Int("attempts", attempts).
		Msg("Mensagem indecifrável não recuperada após auto-retry")
}
