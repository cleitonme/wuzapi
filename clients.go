package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types"
)

type recoverableSocket interface {
	IsConnected() bool
	ConnectContext(context.Context) error
}

type socketRecovery struct {
	client        recoverableSocket
	ctx           context.Context
	cancelSession context.CancelFunc
	requests      chan struct{}
	stopped       chan struct{}
	stopOnce      sync.Once
	mu            sync.Mutex
	pending       bool
	lastAttempt   time.Time
	operation     sync.Mutex
}

type messageSender interface {
	recoverableSocket
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

type chatPresenceSender interface {
	recoverableSocket
	SendChatPresence(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error
}

type ClientManager struct {
	sync.RWMutex
	whatsmeowClients  map[string]*whatsmeow.Client
	httpClients       map[string]*resty.Client
	myClients         map[string]*MyClient
	socketRecoveries  map[string]*socketRecovery
	pollOptions       map[string]map[string][]string
}

func NewClientManager() *ClientManager {
	return &ClientManager{
		whatsmeowClients: make(map[string]*whatsmeow.Client),
		httpClients:      make(map[string]*resty.Client),
		myClients:        make(map[string]*MyClient),
		socketRecoveries: make(map[string]*socketRecovery),
		pollOptions:      make(map[string]map[string][]string),
	}
}

func (cm *ClientManager) SetWhatsmeowClient(userID string, client *whatsmeow.Client) {
	cm.Lock()
	defer cm.Unlock()
	if recovery := cm.socketRecoveries[userID]; recovery != nil && recovery.client != client {
		recovery.stop()
		recovery.cancelSession()
	}
	cm.whatsmeowClients[userID] = client
}

func (cm *ClientManager) GetWhatsmeowClient(userID string) *whatsmeow.Client {
	cm.RLock()
	defer cm.RUnlock()
	return cm.whatsmeowClients[userID]
}

func (cm *ClientManager) DeleteWhatsmeowClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	if recovery := cm.socketRecoveries[userID]; recovery != nil {
		recovery.stop()
	}
	delete(cm.whatsmeowClients, userID)
}

func (cm *ClientManager) SetHTTPClient(userID string, client *resty.Client) {
	cm.Lock()
	defer cm.Unlock()
	cm.httpClients[userID] = client
}

func (cm *ClientManager) GetHTTPClient(userID string) *resty.Client {
	cm.RLock()
	defer cm.RUnlock()
	return cm.httpClients[userID]
}

func (cm *ClientManager) DeleteHTTPClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.httpClients, userID)
}

func (cm *ClientManager) SetMyClient(userID string, client *MyClient) {
	cm.Lock()
	defer cm.Unlock()
	cm.myClients[userID] = client
}

func (cm *ClientManager) GetMyClient(userID string) *MyClient {
	cm.RLock()
	defer cm.RUnlock()
	return cm.myClients[userID]
}

func (cm *ClientManager) DeleteMyClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.myClients, userID)
	delete(cm.pollOptions, userID)
}

// UpdateMyClientSubscriptions updates the event subscriptions of a client without reconnecting
func (cm *ClientManager) UpdateMyClientSubscriptions(userID string, subscriptions []string) {
	cm.Lock()
	defer cm.Unlock()
	if client, exists := cm.myClients[userID]; exists {
		client.subscriptions = subscriptions
	}
}

func (cm *ClientManager) stopSocketRecovery(userID string, cancelSession bool) {
	cm.RLock()
	recovery := cm.socketRecoveries[userID]
	cm.RUnlock()
	if recovery == nil {
		return
	}
	recovery.stop()
	if cancelSession {
		recovery.cancelSession()
	} else {
		recovery.operation.Lock()
		recovery.operation.Unlock()
	}
}

func (cm *ClientManager) requestSocketRecovery(userID string, client recoverableSocket, err error) {
	if !*socketRecoveryEnabled || !errors.Is(err, socket.ErrSocketClosed) {
		return
	}
	cm.RLock()
	recovery := cm.socketRecoveries[userID]
	current := cm.whatsmeowClients[userID]
	cm.RUnlock()
	if recovery != nil && current == client && recovery.client == client {
		recovery.request(err, time.Now())
	}
}

func (cm *ClientManager) registerSocketRecovery(userID string, recovery *socketRecovery) bool {
	cm.Lock()
	defer cm.Unlock()
	if cm.whatsmeowClients[userID] != recovery.client || !recovery.active() {
		return false
	}
	cm.socketRecoveries[userID] = recovery
	return true
}

func (cm *ClientManager) removeSocketRecovery(userID string, recovery *socketRecovery) {
	recovery.stop()
	cm.Lock()
	defer cm.Unlock()
	if cm.socketRecoveries[userID] == recovery {
		delete(cm.socketRecoveries, userID)
	}
}
