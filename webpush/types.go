package webpush

import "heckel.io/ntfy/v2/log"

// Delivery statuses, as stored in the delivery table's status column.
const (
	DeliveryStatusPending   = 0 // Not yet confirmed by the push service, eligible for (re)trying
	DeliveryStatusDelivered = 1 // Accepted (2xx) by the push service, never sent again
	DeliveryStatusDead      = 2 // Given up: max attempts reached or message expired before delivery
)

// Subscription represents a web push subscription.
type Subscription struct {
	ID       string
	Endpoint string
	Auth     string
	P256dh   string
	UserID   string
}

// Context returns the logging context for the subscription.
func (w *Subscription) Context() log.Context {
	return map[string]any{
		"web_push_subscription_id":       w.ID,
		"web_push_subscription_user_id":  w.UserID,
		"web_push_subscription_endpoint": w.Endpoint,
	}
}

// Delivery is one ledger row: the delivery state of a single message to a single
// subscription. The unique (subscription_id, message_id) key is the idempotency anchor
// that guarantees each message is pushed at most once per subscription, including across
// restarts.
type Delivery struct {
	ID             string
	SubscriptionID string
	MessageID      string
	Status         int
	Attempts       int
	CreatedAt      int64
	UpdatedAt      int64
	NextAttemptAt  int64
	ClaimedAt      int64
	DeliveredAt    int64
	LastError      string
}

// Context returns the logging context for the delivery.
func (d *Delivery) Context() log.Context {
	return map[string]any{
		"web_push_delivery_id":      d.ID,
		"web_push_message_id":       d.MessageID,
		"web_push_delivery_status":  d.Status,
		"web_push_delivery_attempt": d.Attempts,
	}
}

// PendingDelivery is a due (status=pending, next_attempt_at reached) delivery row joined
// with its subscription keys/endpoint and the ready-to-send message payload.
type PendingDelivery struct {
	ID           string
	MessageID    string
	Topic        string
	Attempts     int
	ExpiresAt    int64
	Payload      []byte
	Subscription *Subscription
}

// Context returns the logging context for the pending delivery.
func (d *PendingDelivery) Context() log.Context {
	return log.Context{
		"web_push_delivery_id":      d.ID,
		"web_push_message_id":       d.MessageID,
		"web_push_delivery_attempt": d.Attempts,
	}
}
