package bot

import (
	"sync"
	"time"
)

// StateEvent represents a typed state change notification.
type StateEvent struct {
	Type      string `json:"type"`
	Timestamp int64  `json:"ts"`
}

// State event types.
const (
	EventShipBought   = "ship_bought"
	EventShipDocked   = "ship_docked"
	EventShipSailed   = "ship_sailed"
	EventShipSold     = "ship_sold"
	EventEconomyTick  = "economy"
	EventTrade        = "trade"
	EventPassenger    = "passenger"
	EventWarehouse    = "warehouse"
	EventOrderUpdate  = "order"
)

// EventBroadcaster manages subscribers that receive state change notifications.
// Modelled after CompanyLogger's subscriber pattern.
type EventBroadcaster struct {
	subscribers map[int]chan StateEvent
	nextSubID   int
	mu          sync.RWMutex
}

// NewEventBroadcaster creates a new broadcaster.
func NewEventBroadcaster() *EventBroadcaster {
	return &EventBroadcaster{
		subscribers: make(map[int]chan StateEvent),
	}
}

// Subscribe returns a channel that receives state events.
// Call Unsubscribe with the returned ID when done.
func (eb *EventBroadcaster) Subscribe() (int, <-chan StateEvent) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	ch := make(chan StateEvent, 32)
	id := eb.nextSubID
	eb.nextSubID++
	eb.subscribers[id] = ch
	return id, ch
}

// Unsubscribe removes a subscriber.
func (eb *EventBroadcaster) Unsubscribe(id int) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	if ch, ok := eb.subscribers[id]; ok {
		close(ch)
		delete(eb.subscribers, id)
	}
}

// Emit broadcasts an event to all subscribers (non-blocking).
func (eb *EventBroadcaster) Emit(eventType string) {
	event := StateEvent{
		Type:      eventType,
		Timestamp: time.Now().UnixMilli(),
	}

	eb.mu.RLock()
	defer eb.mu.RUnlock()

	for _, ch := range eb.subscribers {
		select {
		case ch <- event:
		default:
			// Drop if subscriber is slow.
		}
	}
}
