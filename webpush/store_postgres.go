package webpush

import (
	"heckel.io/ntfy/v2/db"
	"heckel.io/ntfy/v2/db/schema"
)

const (
	postgresSelectSubscriptionIDByEndpointQuery        = `SELECT id FROM webpush_subscription WHERE endpoint = $1`
	postgresSelectSubscriptionCountBySubscriberIPQuery = `SELECT COUNT(*) FROM webpush_subscription WHERE subscriber_ip = $1`
	postgresSelectSubscriptionsForTopicQuery           = `
		SELECT s.id, s.endpoint, s.key_auth, s.key_p256dh, s.user_id
		FROM webpush_subscription_topic st
		JOIN webpush_subscription s ON s.id = st.subscription_id
		WHERE st.topic = $1
		ORDER BY s.endpoint
	`
	postgresSelectSubscriptionsExpiringSoonQuery = `
		SELECT id, endpoint, key_auth, key_p256dh, user_id
		FROM webpush_subscription
		WHERE warned_at = 0 AND updated_at <= $1
	`
	postgresUpsertSubscriptionQuery = `
		INSERT INTO webpush_subscription (id, endpoint, key_auth, key_p256dh, user_id, subscriber_ip, updated_at, warned_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (endpoint)
		DO UPDATE SET key_auth = excluded.key_auth, key_p256dh = excluded.key_p256dh, user_id = excluded.user_id, subscriber_ip = excluded.subscriber_ip, updated_at = excluded.updated_at, warned_at = excluded.warned_at
		RETURNING id
	`
	postgresUpdateSubscriptionWarningSentQuery = `UPDATE webpush_subscription SET warned_at = $1 WHERE id = $2`
	postgresUpdateSubscriptionUpdatedAtQuery   = `UPDATE webpush_subscription SET updated_at = $1 WHERE endpoint = $2`
	postgresDeleteSubscriptionByEndpointQuery  = `DELETE FROM webpush_subscription WHERE endpoint = $1`
	postgresDeleteSubscriptionByUserIDQuery    = `DELETE FROM webpush_subscription WHERE user_id = $1`
	postgresDeleteSubscriptionByAgeQuery       = `DELETE FROM webpush_subscription WHERE updated_at <= $1`

	postgresInsertSubscriptionTopicQuery                    = `INSERT INTO webpush_subscription_topic (subscription_id, topic) VALUES ($1, $2)`
	postgresDeleteSubscriptionTopicAllQuery                 = `DELETE FROM webpush_subscription_topic WHERE subscription_id = $1`
	postgresDeleteSubscriptionTopicWithoutSubscriptionQuery = `DELETE FROM webpush_subscription_topic WHERE subscription_id NOT IN (SELECT id FROM webpush_subscription)`

	postgresInsertDeliveryMessageQuery = `
		INSERT INTO webpush_delivery_message (message_id, topic, payload, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (message_id) DO NOTHING
	`
	postgresInsertDeliveryQuery = `
		INSERT INTO webpush_delivery (id, subscription_id, message_id, status, attempts, created_at, updated_at, next_attempt_at, claimed_at, delivered_at, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (subscription_id, message_id) DO NOTHING
	`
	postgresSelectPendingDeliveriesQuery = `
		SELECT d.id, d.message_id, d.attempts, dm.topic, dm.payload, dm.expires_at,
		       s.id, s.endpoint, s.key_auth, s.key_p256dh, s.user_id
		FROM webpush_delivery d
		JOIN webpush_subscription s ON s.id = d.subscription_id
		JOIN webpush_delivery_message dm ON dm.message_id = d.message_id
		WHERE d.status = $1 AND d.next_attempt_at <= $2
		ORDER BY d.next_attempt_at
		LIMIT $3
	`
	postgresUpdateDeliveryNextAttemptQuery = `
		UPDATE webpush_delivery
		SET next_attempt_at = $1, claimed_at = 0
		WHERE subscription_id = (SELECT id FROM webpush_subscription WHERE endpoint = $2) AND message_id = $3
	`
	postgresClaimDeliveryQuery = `
		UPDATE webpush_delivery
		SET attempts = attempts + 1, claimed_at = $1, next_attempt_at = $2, updated_at = $3
		WHERE id = $4 AND status = $5 AND next_attempt_at <= $6
	`
	postgresMarkDeliveryDeliveredQuery = `
		UPDATE webpush_delivery SET status = $1, claimed_at = 0, delivered_at = $2, updated_at = $3, last_error = '' WHERE id = $4
	`
	postgresMarkDeliveryRetryQuery = `
		UPDATE webpush_delivery SET status = $1, claimed_at = 0, next_attempt_at = $2, updated_at = $3, last_error = $4 WHERE id = $5
	`
	postgresMarkDeliveryDeadQuery = `
		UPDATE webpush_delivery SET status = $1, claimed_at = 0, updated_at = $2, last_error = $3 WHERE id = $4
	`
	postgresExpirePendingDeliveriesQuery = `
		UPDATE webpush_delivery
		SET status = $1, claimed_at = 0, updated_at = $2, last_error = 'message expired before delivery'
		WHERE status = $3 AND message_id IN (
			SELECT message_id FROM webpush_delivery_message WHERE expires_at > 0 AND expires_at <= $4
		)
	`
	postgresSelectDeliveryByEndpointQuery = `
		SELECT d.id, d.subscription_id, d.message_id, d.status, d.attempts,
		       d.created_at, d.updated_at, d.next_attempt_at, d.claimed_at, d.delivered_at, d.last_error
		FROM webpush_delivery d
		JOIN webpush_subscription s ON s.id = d.subscription_id
		WHERE s.endpoint = $1 AND d.message_id = $2
	`
	postgresDeleteOldDeliveriesQuery = `
		DELETE FROM webpush_delivery WHERE id IN (
			SELECT id FROM webpush_delivery WHERE status IN ($1, $2) AND updated_at <= $3
			ORDER BY updated_at LIMIT $4
		)
	`
	postgresDeleteOrphanedDeliveryMessagesQuery = `
		DELETE FROM webpush_delivery_message WHERE message_id NOT IN (SELECT message_id FROM webpush_delivery)
	`
)

// Schema version and queries
const (
	postgresCurrentSchemaVersion = 2
)

// postgresDeliveryTables is the delivery ledger schema, shared by the fresh-create step and
// the version 1 to 2 migration. The delivery message table is created first because delivery
// references it.
const postgresDeliveryTables = `
	CREATE TABLE IF NOT EXISTS webpush_delivery_message (
		message_id TEXT PRIMARY KEY,
		topic TEXT NOT NULL,
		payload BYTEA NOT NULL,
		created_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS webpush_delivery (
		id TEXT PRIMARY KEY,
		subscription_id TEXT NOT NULL REFERENCES webpush_subscription (id) ON DELETE CASCADE,
		message_id TEXT NOT NULL REFERENCES webpush_delivery_message (message_id) ON DELETE CASCADE,
		status INT NOT NULL DEFAULT 0,
		attempts INT NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		next_attempt_at BIGINT NOT NULL,
		claimed_at BIGINT NOT NULL DEFAULT 0,
		delivered_at BIGINT NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		UNIQUE (subscription_id, message_id)
	);
	CREATE INDEX IF NOT EXISTS idx_webpush_delivery_next ON webpush_delivery (status, next_attempt_at);
`

var (
	postgresCreateTables = schema.AsMigrateFunc(`
		CREATE TABLE IF NOT EXISTS webpush_subscription (
			id TEXT PRIMARY KEY,
			endpoint TEXT NOT NULL UNIQUE,
			key_auth TEXT NOT NULL,
			key_p256dh TEXT NOT NULL,
			user_id TEXT NOT NULL,
			subscriber_ip TEXT NOT NULL,
			updated_at BIGINT NOT NULL,
			warned_at BIGINT NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_webpush_subscriber_ip ON webpush_subscription (subscriber_ip);
		CREATE INDEX IF NOT EXISTS idx_webpush_updated_at ON webpush_subscription (updated_at);
		CREATE INDEX IF NOT EXISTS idx_webpush_user_id ON webpush_subscription (user_id);
		CREATE TABLE IF NOT EXISTS webpush_subscription_topic (
			subscription_id TEXT NOT NULL REFERENCES webpush_subscription (id) ON DELETE CASCADE,
			topic TEXT NOT NULL,
			PRIMARY KEY (subscription_id, topic)
		);
		CREATE INDEX IF NOT EXISTS idx_webpush_topic ON webpush_subscription_topic (topic);
	` + postgresDeliveryTables)

	postgresMigrations = map[int]schema.MigrateFunc{
		1: schema.AsMigrateFunc(postgresDeliveryTables), // Version 1 -> 2: delivery ledger
	}
)

// NewPostgresStore creates a new PostgreSQL-backed web push store using an existing database connection pool.
func NewPostgresStore(d *db.DB) (*Store, error) {
	if err := schema.Migrate(d.Primary(), schema.Postgres, schemaStore, postgresCurrentSchemaVersion, postgresCreateTables, postgresMigrations); err != nil {
		return nil, err
	}
	return &Store{
		db: d,
		queries: queries{
			selectSubscriptionIDByEndpoint:             postgresSelectSubscriptionIDByEndpointQuery,
			selectSubscriptionCountBySubscriberIP:      postgresSelectSubscriptionCountBySubscriberIPQuery,
			selectSubscriptionsForTopic:                postgresSelectSubscriptionsForTopicQuery,
			selectSubscriptionsExpiringSoon:            postgresSelectSubscriptionsExpiringSoonQuery,
			upsertSubscription:                         postgresUpsertSubscriptionQuery,
			updateSubscriptionWarningSent:              postgresUpdateSubscriptionWarningSentQuery,
			updateSubscriptionUpdatedAt:                postgresUpdateSubscriptionUpdatedAtQuery,
			deleteSubscriptionByEndpoint:               postgresDeleteSubscriptionByEndpointQuery,
			deleteSubscriptionByUserID:                 postgresDeleteSubscriptionByUserIDQuery,
			deleteSubscriptionByAge:                    postgresDeleteSubscriptionByAgeQuery,
			insertSubscriptionTopic:                    postgresInsertSubscriptionTopicQuery,
			deleteSubscriptionTopicAll:                 postgresDeleteSubscriptionTopicAllQuery,
			deleteSubscriptionTopicWithoutSubscription: postgresDeleteSubscriptionTopicWithoutSubscriptionQuery,
			insertDeliveryMessage:                      postgresInsertDeliveryMessageQuery,
			insertDelivery:                             postgresInsertDeliveryQuery,
			selectPendingDeliveries:                    postgresSelectPendingDeliveriesQuery,
			updateDeliveryNextAttempt:                  postgresUpdateDeliveryNextAttemptQuery,
			claimDelivery:                              postgresClaimDeliveryQuery,
			markDeliveryDelivered:                      postgresMarkDeliveryDeliveredQuery,
			markDeliveryRetry:                          postgresMarkDeliveryRetryQuery,
			markDeliveryDead:                           postgresMarkDeliveryDeadQuery,
			expirePendingDeliveries:                    postgresExpirePendingDeliveriesQuery,
			selectDeliveryByEndpoint:                   postgresSelectDeliveryByEndpointQuery,
			deleteOldDeliveries:                        postgresDeleteOldDeliveriesQuery,
			deleteOrphanedDeliveryMessages:             postgresDeleteOrphanedDeliveryMessagesQuery,
		},
	}, nil
}
