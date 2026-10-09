package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog/log"
)

// ErrRabbitDisabled indica que o broker está fora/desabilitado. Diferente do
// comportamento antigo (nil silencioso), o chamador agora sabe que o evento
// Rabbit foi descartado e pode reagir (métrica, log, spool em disco na DLQ)
// sem afetar o caminho do webhook HTTP, que é independente.
var ErrRabbitDisabled = errors.New("rabbitmq: broker indisponível (desabilitado ou sem conexão)")

// rabbitPublishTimeout limita o tempo de um publish (declare + publish) para
// que um broker travado não segure rabbitMu para sempre. Configurável via
// RABBITMQ_PUBLISH_TIMEOUT_SECONDS (padrão 8s).
func rabbitPublishTimeout() time.Duration {
	if v := os.Getenv("RABBITMQ_PUBLISH_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 120 {
			return time.Duration(n) * time.Second
		}
	}
	return 8 * time.Second
}

var (
	rabbitConn    *amqp091.Connection
	rabbitChannel *amqp091.Channel
	rabbitEnabled bool
	rabbitOnce    sync.Once
	rabbitQueue   string

	// rabbitMu guards rabbitConn/rabbitChannel/rabbitEnabled/rabbitDeclared
	// and serializes every use of the shared channel. An amqp091.Channel
	// must not be used from several goroutines at once: Channel.Publish
	// holds the channel's internal mutex while it writes the method +
	// header + body frames, but Channel.QueueDeclare does not take that
	// mutex at all. Events are published concurrently (one goroutine per
	// client event) and every publish declares the queue first, so a
	// Queue.Declare frame can be written in the middle of another
	// goroutine's content frames. The broker then closes the whole
	// connection with "unexpected_frame: expected content body, got non
	// content body frame instead" and every event still in flight is lost.
	rabbitMu sync.Mutex
	// Filas já declaradas nesta conexão (QueueDeclare é idempotente,
	// mas evitamos o RTT por mensagem).
	rabbitDeclared = make(map[string]bool)
)

const (
	maxRetries    = 10
	retryInterval = 3 * time.Second
)

// setRabbitConnection publishes the RabbitMQ state under rabbitMu. Publishers
// read this state while the reconnection goroutine replaces it, so the swap must
// be atomic with respect to them: without the lock a publisher can observe
// rabbitEnabled==true together with the channel of an already dead connection.
// It also resets the per-connection declare cache, since a new (or dead)
// connection invalidates previous QueueDeclare calls.
func setRabbitConnection(conn *amqp091.Connection, channel *amqp091.Channel, enabled bool) {
	rabbitMu.Lock()
	defer rabbitMu.Unlock()
	rabbitConn = conn
	rabbitChannel = channel
	rabbitEnabled = enabled
	rabbitDeclared = make(map[string]bool)
}

// rabbitIsEnabled reports whether publishing is currently possible.
func rabbitIsEnabled() bool {
	rabbitMu.Lock()
	defer rabbitMu.Unlock()
	return rabbitEnabled
}

// Call this in main() or initialization
func InitRabbitMQ() {
	rabbitURL := os.Getenv("RABBITMQ_URL")
	rabbitQueue = os.Getenv("RABBITMQ_QUEUE")

	if rabbitQueue == "" {
		rabbitQueue = "whatsapp_events" // default queue
	}

	if rabbitURL == "" {
		setRabbitConnection(nil, nil, false)
		log.Info().Msg("RABBITMQ_URL is not set. RabbitMQ publishing disabled.")
		return
	}

	// Attempt to connect with retry
	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Info().
			Int("attempt", attempt).
			Int("max_retries", maxRetries).
			Msg("Attempting to connect to RabbitMQ")

		conn, err := amqp091.Dial(rabbitURL)
		if err != nil {
			log.Warn().
				Err(err).
				Int("attempt", attempt).
				Int("max_retries", maxRetries).
				Msg("Failed to connect to RabbitMQ")

			if attempt < maxRetries {
				log.Info().
					Dur("retry_in", retryInterval).
					Msg("Retrying RabbitMQ connection")
				time.Sleep(retryInterval)
				continue
			}

			// Last attempt failed
			setRabbitConnection(nil, nil, false)
			log.Error().
				Err(err).
				Msg("Could not connect to RabbitMQ after all retries. RabbitMQ disabled.")
			return
		}

		// Connection successful, attempt to open channel
		channel, err := conn.Channel()
		if err != nil {
			conn.Close()
			log.Warn().
				Err(err).
				Int("attempt", attempt).
				Msg("Failed to open RabbitMQ channel")

			if attempt < maxRetries {
				log.Info().
					Dur("retry_in", retryInterval).
					Msg("Retrying RabbitMQ connection")
				time.Sleep(retryInterval)
				continue
			}

			// Last attempt failed
			setRabbitConnection(nil, nil, false)
			log.Error().
				Err(err).
				Msg("Could not open RabbitMQ channel after all retries. RabbitMQ disabled.")
			return
		}

		// Success!
		setRabbitConnection(conn, channel, true)

		log.Info().
			Str("queue", rabbitQueue).
			Int("attempt", attempt).
			Msg("RabbitMQ connection established successfully")

		// Setup handler for automatic reconnection on errors
		go handleConnectionErrors()
		return
	}
}

// Monitor connection AND channel errors and attempt reconnection.
// O código antigo observava só o NotifyClose da conexão: se o broker fecha
// apenas o canal (ex: unexpected_frame), rabbitEnabled seguia true com canal
// morto até o próximo publish falhar. Agora ambos são observados.
func handleConnectionErrors() {
	rabbitMu.Lock()
	currentConn := rabbitConn
	currentChannel := rabbitChannel
	rabbitMu.Unlock()
	if currentConn == nil {
		return
	}
	notifyConnClose := currentConn.NotifyClose(make(chan *amqp091.Error))
	var notifyChanClose chan *amqp091.Error
	if currentChannel != nil {
		notifyChanClose = currentChannel.NotifyClose(make(chan *amqp091.Error))
	}

	// Falha vinda de qualquer um dos dois dispara a reconexão (uma vez).
	var connErr *amqp091.Error
	if notifyChanClose != nil {
		select {
		case connErr = <-notifyConnClose:
		case connErr = <-notifyChanClose:
		}
	} else {
		connErr = <-notifyConnClose
	}
	if connErr != nil {
		log.Error().
			Err(connErr).
			Msg("RabbitMQ connection/channel closed unexpectedly. Attempting reconnection...")

		// Clear the dead connection so no publisher can reach its channel.
		setRabbitConnection(nil, nil, false)

		// Attempt to reconnect
		for attempt := 1; attempt <= maxRetries; attempt++ {
			log.Info().
				Int("attempt", attempt).
				Msg("Reconnecting to RabbitMQ")

			time.Sleep(retryInterval)

			rabbitURL := os.Getenv("RABBITMQ_URL")
			conn, err := amqp091.Dial(rabbitURL)
			if err != nil {
				log.Warn().
					Err(err).
					Int("attempt", attempt).
					Msg("Reconnection failed")
				continue
			}

			channel, err := conn.Channel()
			if err != nil {
				conn.Close()
				log.Warn().
					Err(err).
					Int("attempt", attempt).
					Msg("Failed to open channel on reconnection")
				continue
			}

			// Reconnection successful
			setRabbitConnection(conn, channel, true)

			log.Info().Msg("RabbitMQ reconnected successfully")

			// Restart monitoring
			go handleConnectionErrors()
			return
		}

		log.Error().Msg("Failed to reconnect to RabbitMQ after all retries")
		return
	}
}

// markRabbitDeadLocked invalida a conexão/canal sob rabbitMu (chamador já
// detém o lock — NÃO chama setRabbitConnection aqui para evitar deadlock).
func markRabbitDeadLocked() {
	rabbitConn = nil
	rabbitChannel = nil
	rabbitEnabled = false
	rabbitDeclared = make(map[string]bool)
}

// Optionally, allow overriding the queue per message
func PublishToRabbit(data []byte, queueOverride ...string) error {
	// Both calls below write frames on the same shared AMQP channel and must not
	// interleave with another goroutine's, so the lock covers declare + publish
	// as one unit. See rabbitMu.
	rabbitMu.Lock()
	defer rabbitMu.Unlock()
	if !rabbitEnabled || rabbitChannel == nil {
		deliveryStats.rabbitDisabled.Add(1)
		return ErrRabbitDisabled
	}
	channel := rabbitChannel
	queueName := rabbitQueue
	if len(queueOverride) > 0 && queueOverride[0] != "" {
		queueName = queueOverride[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), rabbitPublishTimeout())
	defer cancel()
	// Declare queue (idempotent, once per connection)
	if !rabbitDeclared[queueName] {
		_, err := channel.QueueDeclare(
			queueName,
			true,  // durable
			false, // auto-delete
			false, // exclusive
			false, // no-wait
			nil,   // arguments
		)
		if err != nil {
			deliveryStats.rabbitFailed.Add(1)
			log.Error().Err(err).Str("queue", queueName).Msg("Could not declare RabbitMQ queue")
			markRabbitDeadLocked()
			return err
		}
		rabbitDeclared[queueName] = true
	}
	err := channel.PublishWithContext(
		ctx,
		"",        // exchange (default)
		queueName, // routing key = queue
		false,     // mandatory
		false,     // immediate
		amqp091.Publishing{
			ContentType:  "application/json",
			Body:         data,
			DeliveryMode: amqp091.Persistent,
		},
	)
	if err != nil {
		deliveryStats.rabbitFailed.Add(1)
		log.Error().Err(err).Str("queue", queueName).Msg("Could not publish to RabbitMQ")
		// Erro de I/O no canal compartilhado provavelmente o invalidou (ou
		// a conexão caiu no meio — TOCTOU residual). Invalida o estado para
		// que o monitor de NotifyClose/reconnect assuma e os próximos
		// publishers recebam ErrRabbitDisabled em vez de reusar canal morto.
		markRabbitDeadLocked()
	} else {
		deliveryStats.rabbitPublished.Add(1)
		log.Debug().Str("queue", queueName).Msg("Published message to RabbitMQ")
	}
	return err
}

// PublishFileErrorToQueue tenta a DLQ no Rabbit; retorna erro para que o
// chamador preserve o payload em disco. Nunca descarta silenciosamente.
func PublishFileErrorToQueue(payload WebhookFileErrorPayload) error {

	queueName := *webhookErrorQueueName

	body, err := json.Marshal(payload)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal file error payload for RabbitMQ")
		return err
	}

	err = PublishToRabbit(body, queueName)
	if err != nil {
		log.Warn().Err(err).Str("queue", queueName).Msg("Failed to publish file error payload to queue")
		return err
	}
	deliveryStats.dlqRabbit.Add(1)
	log.Info().Str("queue", queueName).Msg("File error payload successfully published to queue")
	return nil
}

// PublishDataErrorToQueue tenta a DLQ no Rabbit; retorna erro para que o
// chamador preserve o payload em disco. Nunca descarta silenciosamente.
func PublishDataErrorToQueue(payload WebhookErrorPayload) error {
	queueName := *webhookErrorQueueName
	body, err := json.Marshal(payload)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal data error payload for RabbitMQ")
		return err
	}
	err = PublishToRabbit(body, queueName)
	if err != nil {
		log.Warn().Err(err).Str("queue", queueName).Msg("Failed to publish data error payload to queue")
		return err
	}
	deliveryStats.dlqRabbit.Add(1)
	log.Info().Str("queue", queueName).Msg("Data error payload successfully published to queue")
	return nil
}
