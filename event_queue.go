package main

import (
	"context"
	"sync"

	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	defaultEventQueueSize = 2048
	defaultBulkQueueSize  = 64
)

type eventJob struct {
	raw interface{}
}

// eventWorkers rastreia os workers de eventos para o shutdown aguardá-los.
var eventWorkers sync.WaitGroup

// isInlineEvent mantém eventos de controle no caminho síncrono: são raros,
// baratos e às vezes precisam reagir antes de uma fila de mensagens.
func isInlineEvent(rawEvt interface{}) bool {
	switch rawEvt.(type) {
	case *events.Connected,
		*events.PairSuccess,
		*events.PairError,
		*events.LoggedOut,
		*events.StreamReplaced,
		*events.StreamError,
		*events.Disconnected,
		*events.ConnectFailure,
		*events.ClientOutdated,
		*events.TemporaryBan,
		*events.AppStateSyncComplete,
		*events.AppStateSyncError:
		return true
	}
	return false
}

// startEventWorkers cria uma fila ordenada (mensagens/recibos) e uma fila
// bulk (HistorySync). Ambas são limitadas; o handler do whatsmeow nunca espera
// pelo processamento pesado (marshal, DB, webhook).
func (mycli *MyClient) startEventWorkers(ctx context.Context) {
	if mycli.eventQueue != nil {
		return
	}
	mycli.eventQueue = make(chan eventJob, defaultEventQueueSize)
	mycli.bulkQueue = make(chan eventJob, defaultBulkQueueSize)

	eventWorkers.Add(2)
	go mycli.runEventWorker(ctx, mycli.eventQueue)
	go mycli.runEventWorker(ctx, mycli.bulkQueue)
}

func (mycli *MyClient) enqueueEvent(rawEvt interface{}) {
	if isInlineEvent(rawEvt) {
		mycli.safeHandleEvent(rawEvt)
		return
	}

	queue := mycli.eventQueue
	if _, isBulk := rawEvt.(*events.HistorySync); isBulk {
		queue = mycli.bulkQueue
	}
	if queue == nil {
		// Workers não iniciados (testes ou evento antes do startClient).
		mycli.safeHandleEvent(rawEvt)
		return
	}

	select {
	case queue <- eventJob{raw: rawEvt}:
		return
	default:
	}

	// Fila cheia: processa inline (backpressure) para não perder o evento.
	if shutdownStarted() {
		deliveryStats.eventQueueSpilled.Add(1)
	} else {
		deliveryStats.eventQueueBackpressure.Add(1)
		log.Warn().
			Str("userID", mycli.userID).
			Int("queue_size", cap(queue)).
			Msg("Event queue full; processing event inline")
	}
	mycli.safeHandleEvent(rawEvt)
}

func (mycli *MyClient) runEventWorker(ctx context.Context, queue chan eventJob) {
	defer eventWorkers.Done()
	for {
		select {
		case job := <-queue:
			mycli.safeHandleEvent(job.raw)
		case <-ctx.Done():
			mycli.drainEvents(queue)
			return
		case <-shutdownCh:
			mycli.drainEvents(queue)
			return
		}
	}
}

func (mycli *MyClient) drainEvents(queue chan eventJob) {
	for {
		select {
		case job := <-queue:
			mycli.safeHandleEvent(job.raw)
		default:
			return
		}
	}
}

func (mycli *MyClient) safeHandleEvent(rawEvt interface{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Error().
				Interface("panic", r).
				Str("userID", mycli.userID).
				Msg("Panic while handling WhatsApp event (recovered)")
		}
	}()
	mycli.handleEvent(rawEvt)
}
