package webpush

import (
	"database/sql"
	"errors"
	"net/netip"
	"time"

	"heckel.io/ntfy/v2/db"
	"heckel.io/ntfy/v2/util"
)

const (
	subscriptionIDPrefix                     = "wps_"
	subscriptionIDLength                     = 10
	subscriptionEndpointLimitPerSubscriberIP = 10

	deliveryIDPrefix = "wpd_"
	deliveryIDLength = 10

	schemaStore = "webpush"
)

// Errors returned by the store
var (
	ErrWebPushTooManySubscriptions = errors.New("too many subscriptions")
	ErrWebPushUserIDCannotBeEmpty  = errors.New("user ID cannot be empty")
	ErrWebPushDeliveryNotFound     = errors.New("delivery not found")
)

// Store holds the database connection and queries for web push subscriptions.
type Store struct {
	db      *db.DB
	queries queries
}

// queries holds the database-specific SQL queries.
type queries struct {
	selectSubscriptionIDByEndpoint             string
	selectSubscriptionCountBySubscriberIP      string
	selectSubscriptionsForTopic                string
	selectSubscriptionsExpiringSoon            string
	upsertSubscription                         string
	updateSubscriptionWarningSent              string
	updateSubscriptionUpdatedAt                string
	deleteSubscriptionByEndpoint               string
	deleteSubscriptionByUserID                 string
	deleteSubscriptionByAge                    string
	insertSubscriptionTopic                    string
	deleteSubscriptionTopicAll                 string
	deleteSubscriptionTopicWithoutSubscription string

	insertDeliveryMessage          string
	insertDelivery                 string
	selectPendingDeliveries        string
	updateDeliveryNextAttempt      string
	claimDelivery                  string
	markDeliveryDelivered          string
	markDeliveryRetry              string
	markDeliveryDead               string
	expirePendingDeliveries        string
	selectDeliveryByEndpoint       string
	deleteOldDeliveries            string
	deleteOrphanedDeliveryMessages string
}

// UpsertSubscription adds or updates Web Push subscriptions for the given topics and user ID.
func (s *Store) UpsertSubscription(endpoint string, auth, p256dh, userID string, subscriberIP netip.Addr, topics []string) error {
	return db.ExecTx(s.db, func(tx *sql.Tx) error {
		// Read number of subscriptions for subscriber IP address
		var subscriptionCount int
		if err := tx.QueryRow(s.queries.selectSubscriptionCountBySubscriberIP, subscriberIP.String()).Scan(&subscriptionCount); err != nil {
			return err
		}
		// Read existing subscription ID for endpoint (or create new ID)
		var subscriptionID string
		if err := tx.QueryRow(s.queries.selectSubscriptionIDByEndpoint, endpoint).Scan(&subscriptionID); errors.Is(err, sql.ErrNoRows) {
			if subscriptionCount >= subscriptionEndpointLimitPerSubscriberIP {
				return ErrWebPushTooManySubscriptions
			}
			subscriptionID = util.RandomStringPrefix(subscriptionIDPrefix, subscriptionIDLength)
		} else if err != nil {
			return err
		}
		// Insert or update subscription, and read back the actual ID (which may differ from
		// the generated one if another request for the same endpoint raced us and inserted first)
		updatedAt, warnedAt := time.Now().Unix(), 0
		if err := tx.QueryRow(s.queries.upsertSubscription, subscriptionID, endpoint, auth, p256dh, userID, subscriberIP.String(), updatedAt, warnedAt).Scan(&subscriptionID); err != nil {
			return err
		}
		// Replace all subscription topics
		if _, err := tx.Exec(s.queries.deleteSubscriptionTopicAll, subscriptionID); err != nil {
			return err
		}
		for _, topic := range topics {
			if _, err := tx.Exec(s.queries.insertSubscriptionTopic, subscriptionID, topic); err != nil {
				return err
			}
		}
		return nil
	})
}

// SubscriptionsForTopic returns all subscriptions for the given topic.
func (s *Store) SubscriptionsForTopic(topic string) ([]*Subscription, error) {
	rows, err := s.db.ReadOnly().Query(s.queries.selectSubscriptionsForTopic, topic)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return subscriptionsFromRows(rows)
}

// SubscriptionsExpiring returns all subscriptions that have not been updated for a given time period.
func (s *Store) SubscriptionsExpiring(warnAfter time.Duration) ([]*Subscription, error) {
	rows, err := s.db.ReadOnly().Query(s.queries.selectSubscriptionsExpiringSoon, time.Now().Add(-warnAfter).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return subscriptionsFromRows(rows)
}

// MarkExpiryWarningSent marks the given subscriptions as having received a warning about expiring soon.
func (s *Store) MarkExpiryWarningSent(subscriptions []*Subscription) error {
	return db.ExecTx(s.db, func(tx *sql.Tx) error {
		for _, subscription := range subscriptions {
			if _, err := tx.Exec(s.queries.updateSubscriptionWarningSent, time.Now().Unix(), subscription.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveSubscriptionsByEndpoint removes the subscription for the given endpoint.
func (s *Store) RemoveSubscriptionsByEndpoint(endpoint string) error {
	_, err := s.db.Exec(s.queries.deleteSubscriptionByEndpoint, endpoint)
	return err
}

// RemoveSubscriptionsByUserID removes all subscriptions for the given user ID.
func (s *Store) RemoveSubscriptionsByUserID(userID string) error {
	if userID == "" {
		return ErrWebPushUserIDCannotBeEmpty
	}
	_, err := s.db.Exec(s.queries.deleteSubscriptionByUserID, userID)
	return err
}

// RemoveExpiredSubscriptions removes all subscriptions that have not been updated for a given time period.
func (s *Store) RemoveExpiredSubscriptions(expireAfter time.Duration) error {
	return db.ExecTx(s.db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(s.queries.deleteSubscriptionByAge, time.Now().Add(-expireAfter).Unix()); err != nil {
			return err
		}
		_, err := tx.Exec(s.queries.deleteSubscriptionTopicWithoutSubscription)
		return err
	})
}

// SetSubscriptionUpdatedAt updates the updated_at timestamp for a subscription by endpoint. This is
// exported for testing purposes.
func (s *Store) SetSubscriptionUpdatedAt(endpoint string, updatedAt int64) error {
	_, err := s.db.Exec(s.queries.updateSubscriptionUpdatedAt, updatedAt, endpoint)
	return err
}

// EnqueueDeliveries writes the persistent ledger rows for one message: one delivery_message row
// (the payload, stored once and ignored if it exists) plus one pending delivery per subscription.
// The unique (subscription_id, message_id) constraint makes this idempotent: enqueuing the same
// message again never creates a duplicate row and therefore never causes a duplicate push.
func (s *Store) EnqueueDeliveries(messageID, topic string, payload []byte, expiresAt int64, subscriptions []*Subscription) error {
	return db.ExecTx(s.db, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		if _, err := tx.Exec(s.queries.insertDeliveryMessage, messageID, topic, payload, now, expiresAt); err != nil {
			return err
		}
		for _, sub := range subscriptions {
			id := util.RandomStringPrefix(deliveryIDPrefix, deliveryIDLength)
			if _, err := tx.Exec(s.queries.insertDelivery, id, sub.ID, messageID, DeliveryStatusPending, 0, now, now, now, 0, 0, ""); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingDeliveries returns due pending rows (status pending, next_attempt_at reached), joined
// with their subscription and message payload. Rows claimed by an in-flight worker have their
// next_attempt_at pushed into the future (the visibility timeout), so they are not returned
// until that timeout passes, which also recovers rows claimed by a process that died.
func (s *Store) PendingDeliveries(now int64, limit int) ([]*PendingDelivery, error) {
	rows, err := s.db.ReadOnly().Query(s.queries.selectPendingDeliveries, DeliveryStatusPending, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pending := make([]*PendingDelivery, 0)
	for rows.Next() {
		d := &PendingDelivery{Subscription: &Subscription{}}
		var payload []byte
		if err := rows.Scan(
			&d.ID, &d.MessageID, &d.Attempts, &d.Topic, &payload, &d.ExpiresAt,
			&d.Subscription.ID, &d.Subscription.Endpoint, &d.Subscription.Auth, &d.Subscription.P256dh, &d.Subscription.UserID,
		); err != nil {
			return nil, err
		}
		d.Payload = payload
		pending = append(pending, d)
	}
	return pending, nil
}

// SetDeliveryNextAttemptAt overrides when a row is next due and releases any in-flight claim.
// Exported for testing purposes (simulating backoff timers and restarts).
func (s *Store) SetDeliveryNextAttemptAt(endpoint, messageID string, nextAttemptAt int64) error {
	_, err := s.db.Exec(s.queries.updateDeliveryNextAttempt, nextAttemptAt, endpoint, messageID)
	return err
}

// ClaimDelivery atomically claims a due pending row for one worker: attempts is incremented and
// next_attempt_at pushed to claimedUntil (the visibility timeout). It returns false if another
// worker (or cluster node) claimed the row first. This is the mutual-exclusion guard that makes
// delivery at-most-once per attempt window across processes.
func (s *Store) ClaimDelivery(id string, now, claimedUntil int64) (bool, error) {
	result, err := s.db.Exec(s.queries.claimDelivery, now, claimedUntil, now, id, DeliveryStatusPending, now)
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return claimed == 1, nil
}

// MarkDeliveryDelivered records a confirmed (2xx) delivery. Delivered rows are never selected or
// sent again, which is the "never push twice after success" guarantee.
func (s *Store) MarkDeliveryDelivered(id string, now int64) error {
	_, err := s.db.Exec(s.queries.markDeliveryDelivered, DeliveryStatusDelivered, now, now, id)
	return err
}

// MarkDeliveryRetry releases a claimed row back to pending and schedules its next attempt.
func (s *Store) MarkDeliveryRetry(id string, now, nextAttemptAt int64, lastError string) error {
	_, err := s.db.Exec(s.queries.markDeliveryRetry, DeliveryStatusPending, nextAttemptAt, now, lastError, id)
	return err
}

// MarkDeliveryDead gives up on a claimed row (max attempts reached or message expired).
func (s *Store) MarkDeliveryDead(id string, now int64, lastError string) error {
	_, err := s.db.Exec(s.queries.markDeliveryDead, DeliveryStatusDead, now, lastError, id)
	return err
}

// ExpirePendingDeliveries marks pending rows dead whose enclosing message has passed its expiry
// time, and returns the number of rows affected.
func (s *Store) ExpirePendingDeliveries(now int64) (int64, error) {
	result, err := s.db.Exec(s.queries.expirePendingDeliveries, DeliveryStatusDead, now, DeliveryStatusPending, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Delivery returns the ledger row for one message and endpoint, or ErrWebPushDeliveryNotFound.
// Exported for testing and diagnostics.
func (s *Store) Delivery(endpoint, messageID string) (*Delivery, error) {
	d := &Delivery{}
	err := s.db.ReadOnly().QueryRow(s.queries.selectDeliveryByEndpoint, endpoint, messageID).Scan(
		&d.ID, &d.SubscriptionID, &d.MessageID, &d.Status, &d.Attempts,
		&d.CreatedAt, &d.UpdatedAt, &d.NextAttemptAt, &d.ClaimedAt, &d.DeliveredAt, &d.LastError,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWebPushDeliveryNotFound
	} else if err != nil {
		return nil, err
	}
	return d, nil
}

// RemoveOldDeliveries deletes up to limit delivered or dead rows whose last update is older than
// updatedBefore, and returns the number of rows deleted. Rows for deleted or expired
// subscriptions are removed separately via foreign key cascade.
func (s *Store) RemoveOldDeliveries(updatedBefore int64, limit int) (int64, error) {
	result, err := s.db.Exec(s.queries.deleteOldDeliveries, DeliveryStatusDelivered, DeliveryStatusDead, updatedBefore, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RemoveOrphanedDeliveryMessages deletes message payload rows no longer referenced by any delivery
// row (all per-subscription rows were delivered, aged out, or cascaded away with their
// subscription), and returns the number of rows deleted.
func (s *Store) RemoveOrphanedDeliveryMessages() (int64, error) {
	result, err := s.db.Exec(s.queries.deleteOrphanedDeliveryMessages)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func subscriptionsFromRows(rows *sql.Rows) ([]*Subscription, error) {
	subscriptions := make([]*Subscription, 0)
	for rows.Next() {
		var id, endpoint, auth, p256dh, userID string
		if err := rows.Scan(&id, &endpoint, &auth, &p256dh, &userID); err != nil {
			return nil, err
		}
		subscriptions = append(subscriptions, &Subscription{
			ID:       id,
			Endpoint: endpoint,
			Auth:     auth,
			P256dh:   p256dh,
			UserID:   userID,
		})
	}
	return subscriptions, nil
}
