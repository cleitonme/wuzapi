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
//  4. Esgotadas as tentativas (ou TTL), o pendente é descartado com log.Error
//     rico para rastreio posterior (gave_up).
// O consumidor recebe apenas o UndecryptableMessage original; o resultado
// do retry aparece só nos logs (INFO recupera/envia, ERROR desiste).

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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

	// Contexto capturado do events.UndecryptableMessage para rastreio no gave_up.
	unavailableType string
	decryptFailMode string
	isGroup         bool
	isFromMe        bool
	isUnavailable   bool
	msgTimestamp    time.Time
	msgType         string
	pushName        string
	sourceStr       string

	mu            sync.Mutex
	timers        []*time.Timer
	attempts      int
	requestIDs    []string
	done          atomic.Bool
	firstSeen     time.Time
	lastAttemptAt time.Time
	lastError     string
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
// Recebe o evento completo para preservar o máximo de contexto para o
// log.Error de gave_up (rastreio posterior).
func scheduleUnavailableAutoRetry(mycli *MyClient, evt *events.UndecryptableMessage) {
	if mycli == nil || evt == nil || evt.Info.ID == "" {
		return
	}
	msgID := string(evt.Info.ID)
	unavailableType := string(evt.UnavailableType)
	// status@broadcast (Status/Stories) não é usado pelo projeto: ignorar
	// para evitar pedido peer para JID especial e ERROR gave_up inútil.
	if evt.Info.Chat.Server == types.BroadcastServer || evt.Info.Chat.String() == "status@broadcast" {
		undecryptableStats.skipped.Add(1)
		log.Debug().
			Str("userID", mycli.userID).
			Str("messageID", msgID).
			Str("chat", evt.Info.Chat.String()).
			Str("sender", evt.Info.Sender.String()).
			Str("unavailableType", unavailableType).
			Msg("Auto-retry ignorado: status@broadcast não suportado")
		return
	}
	// view_once é intencionalmente indisponível: reenvio não vai ajudar.
	if unavailableType == "view_once" {
		undecryptableStats.skipped.Add(1)
		log.Debug().
			Str("userID", mycli.userID).
			Str("messageID", msgID).
			Str("chat", evt.Info.Chat.String()).
			Str("sender", evt.Info.Sender.String()).
			Str("unavailableType", unavailableType).
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
		userID:          userID,
		chat:            evt.Info.Chat,
		sender:          evt.Info.Sender,
		msgID:           msgID,
		unavailableType: unavailableType,
		decryptFailMode: string(evt.DecryptFailMode),
		isGroup:         evt.Info.IsGroup,
		isFromMe:        evt.Info.IsFromMe,
		isUnavailable:   evt.IsUnavailable,
		msgTimestamp:    evt.Info.Timestamp,
		msgType:         evt.Info.Type,
		pushName:        evt.Info.PushName,
		sourceStr:       evt.Info.SourceString(),
		firstSeen:       time.Now(),
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
	// Defesa: pendente antigo de status@broadcast nunca deve gerar SendMessage.
	if entry.chat.Server == types.BroadcastServer {
		undecDropSkipped(key, "status-broadcast-skipped")
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
		entry.mu.Lock()
		entry.lastAttemptAt = time.Now()
		entry.lastError = "session_disconnected"
		entry.mu.Unlock()
		log.Warn().
			Str("userID", entry.userID).
			Str("messageID", entry.msgID).
			Str("chat", entry.chat.String()).
			Str("sender", entry.sender.String()).
			Bool("isGroup", entry.isGroup).
			Str("unavailableType", entry.unavailableType).
			Str("decryptFailMode", entry.decryptFailMode).
			Int("attempt", attempt+1).
			Int("maxAttempts", undecryptableAutoRetryMaxAttempts).
			Msg("Auto-retry adiado: sessão desconectada (mantém agendamento)")
		return
	}

	resp, err := sendUnavailableMessageRequest(client, entry.chat, entry.sender, entry.msgID, undecryptableRequestTimeout)
	if err != nil {
		entry.mu.Lock()
		entry.lastAttemptAt = time.Now()
		entry.lastError = err.Error()
		entry.mu.Unlock()
		log.Warn().
			Err(err).
			Str("userID", entry.userID).
			Str("messageID", entry.msgID).
			Str("chat", entry.chat.String()).
			Str("sender", entry.sender.String()).
			Bool("isGroup", entry.isGroup).
			Str("unavailableType", entry.unavailableType).
			Str("decryptFailMode", entry.decryptFailMode).
			Int("attempt", attempt+1).
			Int("maxAttempts", undecryptableAutoRetryMaxAttempts).
			Msg("Pedido peer automático falhou")
		// Se foi a última tentativa agendada, registra Error imediato para
		// rastreio (o TTL ainda fará o gave_up final).
		if attempt+1 >= undecryptableAutoRetryMaxAttempts {
			undecLogGiveUpError(entry, "attempts_exhausted", false)
		}
		return
	}

	undecMu.Lock()
	undecLastSend[entry.userID] = time.Now()
	undecMu.Unlock()

	requestID := string(resp.ID)
	entry.mu.Lock()
	entry.attempts++
	entry.requestIDs = append(entry.requestIDs, requestID)
	entry.lastAttemptAt = time.Now()
	entry.lastError = ""
	entry.mu.Unlock()
	undecReqIndex.Store(requestID, key)
	undecryptableStats.retrySent.Add(1)

	log.Info().
		Str("userID", entry.userID).
		Str("messageID", entry.msgID).
		Str("chat", entry.chat.String()).
		Str("sender", entry.sender.String()).
		Bool("isGroup", entry.isGroup).
		Str("unavailableType", entry.unavailableType).
		Str("decryptFailMode", entry.decryptFailMode).
		Str("requestID", requestID).
		Int("attempt", attempt+1).
		Int("maxAttempts", undecryptableAutoRetryMaxAttempts).
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
		Str("chat", entry.chat.String()).
		Str("sender", entry.sender.String()).
		Bool("isGroup", entry.isGroup).
		Str("unavailableType", entry.unavailableType).
		Str("decryptFailMode", entry.decryptFailMode).
		Str("reason", reason).
		Int("attempts", attempts).
		Strs("requestIDs", reqIDs).
		Dur("elapsed", time.Since(entry.firstSeen)).
		Msg("Mensagem indecifrável recuperada")
}

// undecDropSkipped descarta pendente sem contar como recovered (ex.: broadcast
// antigo que escapou do filtro). Só decrementa por-usuário e conta skipped.
func undecDropSkipped(key, reason string) {
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
	undecryptableStats.skipped.Add(1)

	log.Debug().
		Str("userID", entry.userID).
		Str("messageID", entry.msgID).
		Str("chat", entry.chat.String()).
		Str("reason", reason).
		Msg("Pendente descartado sem retry (broadcast, sem recovery)")
}

// undecLogGiveUpError emite o log.Error rastreável de mensagem não
// recuperada com o máximo de informações. Se countStats for true, incrementa
// o contador gaveUp (caminho final via TTL). Se false, é apenas o sinal
// antecipado de tentativas esgotadas (o TTL ainda fará o gave_up final).
func undecLogGiveUpError(entry *undecryptablePending, reason string, countStats bool) {
	entry.mu.Lock()
	reqIDs := append([]string(nil), entry.requestIDs...)
	attempts := entry.attempts
	lastErr := entry.lastError
	lastAttempt := entry.lastAttemptAt
	entry.mu.Unlock()

	undecMu.Lock()
	pendingForUser := undecPerUser[entry.userID]
	undecMu.Unlock()

	ev := log.Error().
		Str("userID", entry.userID).
		Str("key", undecPendingKey(entry.userID, entry.msgID)).
		Str("messageID", entry.msgID).
		Str("chat", entry.chat.String()).
		Str("sender", entry.sender.String()).
		Str("source", entry.sourceStr).
		Bool("isGroup", entry.isGroup).
		Bool("isFromMe", entry.isFromMe).
		Bool("isUnavailable", entry.isUnavailable).
		Str("unavailableType", entry.unavailableType).
		Str("decryptFailMode", entry.decryptFailMode).
		Str("msgType", entry.msgType).
		Str("pushName", entry.pushName).
		Time("msgTimestamp", entry.msgTimestamp).
		Time("firstSeen", entry.firstSeen).
		Dur("elapsed", time.Since(entry.firstSeen)).
		Int("attempts", attempts).
		Int("maxAttempts", undecryptableAutoRetryMaxAttempts).
		Strs("requestIDs", reqIDs).
		Time("lastAttemptAt", lastAttempt).
		Str("lastError", lastErr).
		Str("reason", reason).
		Int("pendingForUser", pendingForUser).
		Uint64("stat_total", undecryptableStats.total.Load()).
		Uint64("stat_retrySent", undecryptableStats.retrySent.Load()).
		Uint64("stat_recovered", undecryptableStats.recovered.Load()).
		Uint64("stat_gaveUp", undecryptableStats.gaveUp.Load()).
		Uint64("stat_skipped", undecryptableStats.skipped.Load())
	if lastErr != "" {
		ev = ev.Str("error", lastErr)
	}
	if countStats {
		ev.Msg("undecryptable.gave_up: mensagem indecifrável não recuperada após auto-retry")
	} else {
		ev.Msg("undecryptable.attempts_exhausted: última tentativa de auto-retry falhou")
	}
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

	undecLogGiveUpError(entry, reason, true)
}
