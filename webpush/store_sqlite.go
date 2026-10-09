package webpush

import (
	"database/sql"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
	"heckel.io/ntfy/v2/db"
	"heckel.io/ntfy/v2/db/schema"
)

const (
	sqliteBuiltinStartupQueries = `
		PRAGMA foreign_keys = ON;
	`

	sqliteSelectSubscriptionIDByEndpointQuery        = `SELECT id FROM subscription WHERE endpoint = ?`
	sqliteSelectSubscriptionCountBySubscriberIPQuery = `SELECT COUNT(*) FROM subscription WHERE subscriber_ip = ?`
	sqliteSelectSubscriptionsForTopicQuery           = `
		SELECT id, endpoint, key_auth, key_p256dh, user_id
		FROM subscription_topic st
		JOIN subscription s ON s.id = st.subscription_id
		WHERE st.topic = ?
		ORDER BY endpoint
	`
	sqliteSelectSubscriptionsExpiringSoonQuery = `
		SELECT id, endpoint, key_auth, key_p256dh, user_id 
		FROM subscription 
		WHERE warned_at = 0 AND updated_at <= ?
	`
	sqliteUpsertSubscriptionQuery = `
		INSERT INTO subscription (id, endpoint, key_auth, key_p256dh, user_id, subscriber_ip, updated_at, warned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (endpoint)
		DO UPDATE SET key_auth = excluded.key_auth, key_p256dh = excluded.key_p256dh, user_id = excluded.user_id, subscriber_ip = excluded.subscriber_ip, updated_at = excluded.updated_at, warned_at = excluded.warned_at
		RETURNING id
	`
	sqliteUpdateSubscriptionWarningSentQuery = `UPDATE subscription SET warned_at = ? WHERE id = ?`
	sqliteUpdateSubscriptionUpdatedAtQuery   = `UPDATE subscription SET updated_at = ? WHERE endpoint = ?`
	sqliteDeleteSubscriptionByEndpointQuery  = `DELETE FROM subscription WHERE endpoint = ?`
	sqliteDeleteSubscriptionByUserIDQuery    = `DELETE FROM subscription WHERE user_id = ?`
	sqliteDeleteSubscriptionByAgeQuery       = `DELETE FROM subscription WHERE updated_at <= ?` // Full table scan!

	sqliteInsertSubscriptionTopicQuery                    = `INSERT INTO subscription_topic (subscription_id, topic) VALUES (?, ?)`
	sqliteDeleteSubscriptionTopicAllQuery                 = `DELETE FROM subscription_topic WHERE subscription_id = ?`
	sqliteDeleteSubscriptionTopicWithoutSubscriptionQuery = `DELETE FROM subscription_topic WHERE subscription_id NOT IN (SELECT id FROM subscription)`

	sqliteInsertDeliveryMessageQuery = `
		INSERT OR IGNORE INTO delivery_message (message_id, topic, payload, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
	`
	sqliteInsertDeliveryQuery = `
		INSERT OR IGNORE INTO delivery (id, subscription_id, message_id, status, attempts, created_at, updated_at, next_attempt_at, claimed_at, delivered_at, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	sqliteSelectPendingDeliveriesQuery = `
		SELECT d.id, d.message_id, d.attempts, dm.topic, dm.payload, dm.expires_at,
		       s.id, s.endpoint, s.key_auth, s.key_p256dh, s.user_id
		FROM delivery d
		JOIN subscription s ON s.id = d.subscription_id
		JOIN delivery_message dm ON dm.message_id = d.message_id
		WHERE d.status = ? AND d.next_attempt_at <= ?
		ORDER BY d.next_attempt_at
		LIMIT ?
	`
	sqliteUpdateDeliveryNextAttemptQuery = `
		UPDATE delivery
		SET next_attempt_at = ?, claimed_at = 0
		WHERE subscription_id = (SELECT id FROM subscription WHERE endpoint = ?) AND message_id = ?
	`
	sqliteClaimDeliveryQuery = `
		UPDATE delivery
		SET attempts = attempts + 1, claimed_at = ?, next_attempt_at = ?, updated_at = ?
		WHERE id = ? AND status = ? AND next_attempt_at <= ?
	`
	sqliteMarkDeliveryDeliveredQuery = `
		UPDATE delivery SET status = ?, claimed_at = 0, delivered_at = ?, updated_at = ?, last_error = '' WHERE id = ?
	`
	sqliteMarkDeliveryRetryQuery = `
		UPDATE delivery SET status = ?, claimed_at = 0, next_attempt_at = ?, updated_at = ?, last_error = ? WHERE id = ?
	`
	sqliteMarkDeliveryDeadQuery = `
		UPDATE delivery SET status = ?, claimed_at = 0, updated_at = ?, last_error = ? WHERE id = ?
	`
	sqliteExpirePendingDeliveriesQuery = `
		UPDATE delivery
		SET status = ?, claimed_at = 0, updated_at = ?, last_error = 'message expired before delivery'
		WHERE status = ? AND message_id IN (
			SELECT message_id FROM delivery_message WHERE expires_at > 0 AND expires_at <= ?
		)
	`
	sqliteSelectDeliveryByEndpointQuery = `
		SELECT d.id, d.subscription_id, d.message_id, d.status, d.attempts,
		       d.created_at, d.updated_at, d.next_attempt_at, d.claimed_at, d.delivered_at, d.last_error
		FROM delivery d
		JOIN subscription s ON s.id = d.subscription_id
		WHERE s.endpoint = ? AND d.message_id = ?
	`
	sqliteDeleteOldDeliveriesQuery = `
		DELETE FROM delivery WHERE id IN (
			SELECT id FROM delivery WHERE status IN (?, ?) AND updated_at <= ?
			ORDER BY updated_at LIMIT ?
		)
	`
	sqliteDeleteOrphanedDeliveryMessagesQuery = `
		DELETE FROM delivery_message WHERE message_id NOT IN (SELECT message_id FROM delivery)
	`
)

// Schema version and queries
const (
	sqliteCurrentSchemaVersion = 2
)

// sqliteDeliveryTables is the delivery ledger schema, shared by the fresh-create step and
// the version 1 to 2 migration. The delivery_message table is created first because delivery
// references it.
const sqliteDeliveryTables = `
	CREATE TABLE IF NOT EXISTS delivery_message (
		message_id TEXT PRIMARY KEY,
		topic TEXT NOT NULL,
		payload BLOB NOT NULL,
		created_at INT NOT NULL,
		expires_at INT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS delivery (
		id TEXT PRIMARY KEY,
		subscription_id TEXT NOT NULL,
		message_id TEXT NOT NULL,
		status INT NOT NULL DEFAULT 0,
		attempts INT NOT NULL DEFAULT 0,
		created_at INT NOT NULL,
		updated_at INT NOT NULL,
		next_attempt_at INT NOT NULL,
		claimed_at INT NOT NULL DEFAULT 0,
		delivered_at INT NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		UNIQUE (subscription_id, message_id),
		FOREIGN KEY (subscription_id) REFERENCES subscription (id) ON DELETE CASCADE,
		FOREIGN KEY (message_id) REFERENCES delivery_message (message_id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_delivery_next ON delivery (status, next_attempt_at);
`

var (
	sqliteCreateTables = schema.AsMigrateFunc(`
		CREATE TABLE IF NOT EXISTS subscription (
			id TEXT PRIMARY KEY,
			endpoint TEXT NOT NULL,
			key_auth TEXT NOT NULL,
			key_p256dh TEXT NOT NULL,
			user_id TEXT NOT NULL,
			subscriber_ip TEXT NOT NULL,
			updated_at INT NOT NULL,
			warned_at INT NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_endpoint ON subscription (endpoint);
		CREATE INDEX IF NOT EXISTS idx_subscriber_ip ON subscription (subscriber_ip);
		CREATE TABLE IF NOT EXISTS subscription_topic (
			subscription_id TEXT NOT NULL,
			topic TEXT NOT NULL,
			PRIMARY KEY (subscription_id, topic),
			FOREIGN KEY (subscription_id) REFERENCES subscription (id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS idx_topic ON subscription_topic (topic);
	` + sqliteDeliveryTables)

	sqliteMigrations = map[int]schema.MigrateFunc{
		1: schema.AsMigrateFunc(sqliteDeliveryTables), // Version 1 -> 2: delivery ledger
	}
)

// NewSQLiteStore creates a new SQLite-backed web push store.
func NewSQLiteStore(filename, startupQueries string) (*Store, error) {
	d, err := sql.Open("sqlite3", filename)
	if err != nil {
		return nil, err
	}
	if err := schema.Migrate(d, schema.SQLite, schemaStore, sqliteCurrentSchemaVersion, sqliteCreateTables, sqliteMigrations); err != nil {
		return nil, err
	}
	if err := runSQLiteStartupQueries(d, startupQueries); err != nil {
		return nil, err
	}
	return &Store{
		db: db.New(&db.Host{DB: d}, nil),
		queries: queries{
			selectSubscriptionIDByEndpoint:             sqliteSelectSubscriptionIDByEndpointQuery,
			selectSubscriptionCountBySubscriberIP:      sqliteSelectSubscriptionCountBySubscriberIPQuery,
			selectSubscriptionsForTopic:                sqliteSelectSubscriptionsForTopicQuery,
			selectSubscriptionsExpiringSoon:            sqliteSelectSubscriptionsExpiringSoonQuery,
			upsertSubscription:                         sqliteUpsertSubscriptionQuery,
			updateSubscriptionWarningSent:              sqliteUpdateSubscriptionWarningSentQuery,
			updateSubscriptionUpdatedAt:                sqliteUpdateSubscriptionUpdatedAtQuery,
			deleteSubscriptionByEndpoint:               sqliteDeleteSubscriptionByEndpointQuery,
			deleteSubscriptionByUserID:                 sqliteDeleteSubscriptionByUserIDQuery,
			deleteSubscriptionByAge:                    sqliteDeleteSubscriptionByAgeQuery,
			insertSubscriptionTopic:                    sqliteInsertSubscriptionTopicQuery,
			deleteSubscriptionTopicAll:                 sqliteDeleteSubscriptionTopicAllQuery,
			deleteSubscriptionTopicWithoutSubscription: sqliteDeleteSubscriptionTopicWithoutSubscriptionQuery,
			insertDeliveryMessage:                      sqliteInsertDeliveryMessageQuery,
			insertDelivery:                             sqliteInsertDeliveryQuery,
			selectPendingDeliveries:                    sqliteSelectPendingDeliveriesQuery,
			updateDeliveryNextAttempt:                  sqliteUpdateDeliveryNextAttemptQuery,
			claimDelivery:                              sqliteClaimDeliveryQuery,
			markDeliveryDelivered:                      sqliteMarkDeliveryDeliveredQuery,
			markDeliveryRetry:                          sqliteMarkDeliveryRetryQuery,
			markDeliveryDead:                           sqliteMarkDeliveryDeadQuery,
			expirePendingDeliveries:                    sqliteExpirePendingDeliveriesQuery,
			selectDeliveryByEndpoint:                   sqliteSelectDeliveryByEndpointQuery,
			deleteOldDeliveries:                        sqliteDeleteOldDeliveriesQuery,
			deleteOrphanedDeliveryMessages:             sqliteDeleteOrphanedDeliveryMessagesQuery,
		},
	}, nil
}

func runSQLiteStartupQueries(db *sql.DB, startupQueries string) error {
	if _, err := db.Exec(startupQueries); err != nil {
		return err
	}
	if _, err := db.Exec(sqliteBuiltinStartupQueries); err != nil {
		return err
	}
	return nil
}
