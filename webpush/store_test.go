package webpush_test

import (
	"database/sql"
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	dbtest "heckel.io/ntfy/v2/db/test"
	"heckel.io/ntfy/v2/webpush"
)

const testWebPushEndpoint = "https://updates.push.services.mozilla.com/wpush/v1/AAABBCCCDDEEEFFF"

// Schema layout as written by ntfy releases before the db/schema framework; used to verify
// that existing databases open cleanly without an adoption step
const (
	testPreFrameworkSQLiteSchema = `
		CREATE TABLE subscription (
			id TEXT PRIMARY KEY,
			endpoint TEXT NOT NULL,
			key_auth TEXT NOT NULL,
			key_p256dh TEXT NOT NULL,
			user_id TEXT NOT NULL,
			subscriber_ip TEXT NOT NULL,
			updated_at INT NOT NULL,
			warned_at INT NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX idx_endpoint ON subscription (endpoint);
		CREATE INDEX idx_subscriber_ip ON subscription (subscriber_ip);
		CREATE TABLE subscription_topic (
			subscription_id TEXT NOT NULL,
			topic TEXT NOT NULL,
			PRIMARY KEY (subscription_id, topic),
			FOREIGN KEY (subscription_id) REFERENCES subscription (id) ON DELETE CASCADE
		);
		CREATE INDEX idx_topic ON subscription_topic (topic);
		CREATE TABLE schemaVersion (id INT PRIMARY KEY, version INT NOT NULL);
		INSERT INTO schemaVersion VALUES (1, 1);
	`
	testPreFrameworkPostgresSchema = `
		CREATE TABLE webpush_subscription (
			id TEXT PRIMARY KEY,
			endpoint TEXT NOT NULL UNIQUE,
			key_auth TEXT NOT NULL,
			key_p256dh TEXT NOT NULL,
			user_id TEXT NOT NULL,
			subscriber_ip TEXT NOT NULL,
			updated_at BIGINT NOT NULL,
			warned_at BIGINT NOT NULL DEFAULT 0
		);
		CREATE INDEX idx_webpush_subscriber_ip ON webpush_subscription (subscriber_ip);
		CREATE INDEX idx_webpush_updated_at ON webpush_subscription (updated_at);
		CREATE INDEX idx_webpush_user_id ON webpush_subscription (user_id);
		CREATE TABLE webpush_subscription_topic (
			subscription_id TEXT NOT NULL REFERENCES webpush_subscription (id) ON DELETE CASCADE,
			topic TEXT NOT NULL,
			PRIMARY KEY (subscription_id, topic)
		);
		CREATE INDEX idx_webpush_topic ON webpush_subscription_topic (topic);
		CREATE TABLE schema_version (store TEXT PRIMARY KEY, version INT NOT NULL);
		INSERT INTO schema_version (store, version) VALUES ('webpush', 1);
	`
)

// TestStoreSchemaEquivalence verifies that a database adopted from the pre-framework layout is
// structurally identical to a freshly created one: same tables, columns, indexes and keys.
func TestStoreSchemaEquivalence(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		freshFile := filepath.Join(t.TempDir(), "fresh.db")
		fresh, err := webpush.NewSQLiteStore(freshFile, "")
		require.Nil(t, err)
		defer fresh.Close()
		migratedFile := filepath.Join(t.TempDir(), "migrated.db")
		d, err := sql.Open("sqlite3", migratedFile)
		require.Nil(t, err)
		_, err = d.Exec(testPreFrameworkSQLiteSchema)
		require.Nil(t, err)
		require.Nil(t, d.Close())
		migrated, err := webpush.NewSQLiteStore(migratedFile, "")
		require.Nil(t, err)
		defer migrated.Close()
		freshDB, err := sql.Open("sqlite3", freshFile)
		require.Nil(t, err)
		defer freshDB.Close()
		migratedDB, err := sql.Open("sqlite3", migratedFile)
		require.Nil(t, err)
		defer migratedDB.Close()
		require.Equal(t, dbtest.SQLiteSchema(t, freshDB), dbtest.SQLiteSchema(t, migratedDB))
	})
	t.Run("postgres", func(t *testing.T) {
		freshDB := dbtest.CreateTestPostgres(t)
		_, err := webpush.NewPostgresStore(freshDB)
		require.Nil(t, err)
		migratedDB := dbtest.CreateTestPostgres(t)
		_, err = migratedDB.Exec(testPreFrameworkPostgresSchema)
		require.Nil(t, err)
		_, err = webpush.NewPostgresStore(migratedDB)
		require.Nil(t, err)
		require.Equal(t, dbtest.PostgresSchema(t, freshDB), dbtest.PostgresSchema(t, migratedDB))
	})
}

func TestStoreSQLiteOpensExistingDatabase(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "webpush.db")
	d, err := sql.Open("sqlite3", filename)
	require.Nil(t, err)
	_, err = d.Exec(testPreFrameworkSQLiteSchema)
	require.Nil(t, err)
	require.Nil(t, d.Close())
	store, err := webpush.NewSQLiteStore(filename, "")
	require.Nil(t, err)
	defer store.Close()
	requireStoreUsable(t, store)
}

func TestStorePostgresOpensExistingDatabase(t *testing.T) {
	testDB := dbtest.CreateTestPostgres(t)
	_, err := testDB.Exec(testPreFrameworkPostgresSchema)
	require.Nil(t, err)
	store, err := webpush.NewPostgresStore(testDB)
	require.Nil(t, err)
	requireStoreUsable(t, store)
}

func requireStoreUsable(t *testing.T, store *webpush.Store) {
	t.Helper()
	err := store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"mytopic"})
	require.Nil(t, err)
	subs, err := store.SubscriptionsForTopic("mytopic")
	require.Nil(t, err)
	require.Len(t, subs, 1)
	require.Equal(t, testWebPushEndpoint, subs[0].Endpoint)
}

func forEachBackend(t *testing.T, f func(t *testing.T, store *webpush.Store)) {
	t.Run("sqlite", func(t *testing.T) {
		store, err := webpush.NewSQLiteStore(filepath.Join(t.TempDir(), "webpush.db"), "")
		require.Nil(t, err)
		t.Cleanup(func() { store.Close() })
		f(t, store)
	})
	t.Run("postgres", func(t *testing.T) {
		testDB := dbtest.CreateTestPostgres(t)
		store, err := webpush.NewPostgresStore(testDB)
		require.Nil(t, err)
		f(t, store)
	})
}

func TestStoreUpsertSubscriptionSubscriptionsForTopic(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"test-topic", "mytopic"}))

		subs, err := store.SubscriptionsForTopic("test-topic")
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.Equal(t, subs[0].Endpoint, testWebPushEndpoint)
		require.Equal(t, subs[0].P256dh, "p256dh-key")
		require.Equal(t, subs[0].Auth, "auth-key")
		require.Equal(t, subs[0].UserID, "u_1234")

		subs2, err := store.SubscriptionsForTopic("mytopic")
		require.Nil(t, err)
		require.Len(t, subs2, 1)
		require.Equal(t, subs[0].Endpoint, subs2[0].Endpoint)
	})
}

func TestStoreUpsertSubscriptionSubscriberIPLimitReached(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert 10 subscriptions with the same IP address
		for i := 0; i < 10; i++ {
			endpoint := fmt.Sprintf(testWebPushEndpoint+"%d", i)
			require.Nil(t, store.UpsertSubscription(endpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"test-topic", "mytopic"}))
		}

		// Another one for the same endpoint should be fine
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"0", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"test-topic", "mytopic"}))

		// But with a different endpoint it should fail
		require.Equal(t, webpush.ErrWebPushTooManySubscriptions, store.UpsertSubscription(testWebPushEndpoint+"11", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"test-topic", "mytopic"}))

		// But with a different IP address it should be fine again
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"99", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("9.9.9.9"), []string{"test-topic", "mytopic"}))
	})
}

func TestStoreUpsertSubscriptionUpdateTopics(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert subscription with two topics, and another with one topic
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"0", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1", "topic2"}))
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"1", "auth-key", "p256dh-key", "", netip.MustParseAddr("9.9.9.9"), []string{"topic1"}))

		subs, err := store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 2)
		require.Equal(t, testWebPushEndpoint+"0", subs[0].Endpoint)
		require.Equal(t, testWebPushEndpoint+"1", subs[1].Endpoint)

		subs, err = store.SubscriptionsForTopic("topic2")
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.Equal(t, testWebPushEndpoint+"0", subs[0].Endpoint)

		// Update the first subscription to have only one topic
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"0", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))

		subs, err = store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 2)
		require.Equal(t, testWebPushEndpoint+"0", subs[0].Endpoint)

		subs, err = store.SubscriptionsForTopic("topic2")
		require.Nil(t, err)
		require.Len(t, subs, 0)
	})
}

func TestStoreUpsertSubscriptionUpdateFields(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert a subscription
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))

		subs, err := store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.Equal(t, "auth-key", subs[0].Auth)
		require.Equal(t, "p256dh-key", subs[0].P256dh)
		require.Equal(t, "u_1234", subs[0].UserID)

		// Re-upsert the same endpoint with different auth, p256dh, and userID
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "new-auth", "new-p256dh", "u_5678", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))

		subs, err = store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.Equal(t, testWebPushEndpoint, subs[0].Endpoint)
		require.Equal(t, "new-auth", subs[0].Auth)
		require.Equal(t, "new-p256dh", subs[0].P256dh)
		require.Equal(t, "u_5678", subs[0].UserID)
	})
}

func TestStoreRemoveByUserIDMultiple(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert two subscriptions for u_1234 and one for u_5678
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"0", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"1", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"2", "auth-key", "p256dh-key", "u_5678", netip.MustParseAddr("9.9.9.9"), []string{"topic1"}))

		subs, err := store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 3)

		// Remove all subscriptions for u_1234
		require.Nil(t, store.RemoveSubscriptionsByUserID("u_1234"))

		// Only u_5678's subscription should remain
		subs, err = store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.Equal(t, testWebPushEndpoint+"2", subs[0].Endpoint)
		require.Equal(t, "u_5678", subs[0].UserID)
	})
}

func TestStoreRemoveByEndpoint(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert subscription with two topics
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1", "topic2"}))
		subs, err := store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 1)

		// And remove it again
		require.Nil(t, store.RemoveSubscriptionsByEndpoint(testWebPushEndpoint))
		subs, err = store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 0)
	})
}

func TestStoreRemoveByUserID(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert subscription with two topics
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1", "topic2"}))
		subs, err := store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 1)

		// And remove it again
		require.Nil(t, store.RemoveSubscriptionsByUserID("u_1234"))
		subs, err = store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 0)
	})
}

func TestStoreRemoveByUserIDEmpty(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		require.Equal(t, webpush.ErrWebPushUserIDCannotBeEmpty, store.RemoveSubscriptionsByUserID(""))
	})
}

func TestStoreExpiryWarningSent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert subscription with two topics
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1", "topic2"}))

		// Set updated_at to the past so it shows up as expiring
		require.Nil(t, store.SetSubscriptionUpdatedAt(testWebPushEndpoint, time.Now().Add(-8*24*time.Hour).Unix()))

		// Verify subscription appears in expiring list (warned_at == 0)
		subs, err := store.SubscriptionsExpiring(7 * 24 * time.Hour)
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.Equal(t, testWebPushEndpoint, subs[0].Endpoint)

		// Mark them as warning sent
		require.Nil(t, store.MarkExpiryWarningSent(subs))

		// Verify subscription no longer appears in expiring list (warned_at > 0)
		subs, err = store.SubscriptionsExpiring(7 * 24 * time.Hour)
		require.Nil(t, err)
		require.Len(t, subs, 0)
	})
}

func TestStoreExpiring(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert subscription with two topics
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1", "topic2"}))
		subs, err := store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 1)

		// Fake-mark them as soon-to-expire
		require.Nil(t, store.SetSubscriptionUpdatedAt(testWebPushEndpoint, time.Now().Add(-8*24*time.Hour).Unix()))

		// Should not be cleaned up yet
		require.Nil(t, store.RemoveExpiredSubscriptions(9*24*time.Hour))

		// Run expiration
		subs, err = store.SubscriptionsExpiring(7 * 24 * time.Hour)
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.Equal(t, testWebPushEndpoint, subs[0].Endpoint)
	})
}

func TestStoreRemoveExpired(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		// Insert subscription with two topics
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1", "topic2"}))
		subs, err := store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 1)

		// Fake-mark them as expired
		require.Nil(t, store.SetSubscriptionUpdatedAt(testWebPushEndpoint, time.Now().Add(-10*24*time.Hour).Unix()))

		// Run expiration
		require.Nil(t, store.RemoveExpiredSubscriptions(9*24*time.Hour))

		// List again, should be 0
		subs, err = store.SubscriptionsForTopic("topic1")
		require.Nil(t, err)
		require.Len(t, subs, 0)
	})
}

func subscriptionsForTestTopic(t *testing.T, store *webpush.Store, topic string) []*webpush.Subscription {
	t.Helper()
	subs, err := store.SubscriptionsForTopic(topic)
	require.Nil(t, err)
	return subs
}

func enqueueTestDelivery(t *testing.T, store *webpush.Store, messageID, topic string, expiresAt int64, subs []*webpush.Subscription) {
	t.Helper()
	require.Nil(t, store.EnqueueDeliveries(messageID, topic, []byte("payload-"+messageID), expiresAt, subs))
}

// TestStoreDeliveryEnqueueAndPending covers the basic ledger lifecycle: enqueue rows for each
// subscription, claim them, and confirm they drop out of the pending queue once claimed.
func TestStoreDeliveryEnqueueAndPending(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"0", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"1", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		subs := subscriptionsForTestTopic(t, store, "topic1")
		require.Len(t, subs, 2)

		now := time.Now().Unix()
		enqueueTestDelivery(t, store, "m_aaaaaaaaaaaa", "topic1", now+3600, subs)

		pending, err := store.PendingDeliveries(now, 100)
		require.Nil(t, err)
		require.Len(t, pending, 2)
		for _, d := range pending {
			require.Equal(t, "m_aaaaaaaaaaaa", d.MessageID)
			require.Equal(t, "topic1", d.Topic)
			require.Equal(t, []byte("payload-m_aaaaaaaaaaaa"), d.Payload)
			require.Equal(t, 0, d.Attempts)
			require.NotEmpty(t, d.Subscription.Endpoint)
		}

		// Re-enqueueing the same message must be a no-op (dedup anchor)
		enqueueTestDelivery(t, store, "m_aaaaaaaaaaaa", "topic1", now+3600, subs)
		pending, err = store.PendingDeliveries(now, 100)
		require.Nil(t, err)
		require.Len(t, pending, 2)

		// Claim one row: attempts increments and the row is invisible while in flight
		claimed, err := store.ClaimDelivery(pending[0].ID, now, now+120)
		require.Nil(t, err)
		require.True(t, claimed)
		claimedAgain, err := store.ClaimDelivery(pending[0].ID, now, now+120)
		require.Nil(t, err)
		require.False(t, claimedAgain)
		pending, err = store.PendingDeliveries(now, 100)
		require.Nil(t, err)
		require.Len(t, pending, 1)

		// After the visibility timeout passes (e.g. the claiming process died), the row is due again
		recovered, err := store.PendingDeliveries(now+121, 100)
		require.Nil(t, err)
		require.Len(t, recovered, 2)
	})
}

// TestStoreDeliveryDeliveredIsFinal verifies that a 2xx confirmation moves the row to a terminal
// delivered state from which it is never returned for sending again.
func TestStoreDeliveryDeliveredIsFinal(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		subs := subscriptionsForTestTopic(t, store, "topic1")
		now := time.Now().Unix()
		enqueueTestDelivery(t, store, "m_bbbbbbbbbbbb", "topic1", now+3600, subs)

		pending, err := store.PendingDeliveries(now, 100)
		require.Nil(t, err)
		require.Len(t, pending, 1)
		require.True(t, pending[0].Subscription.Endpoint == testWebPushEndpoint)

		claimed, err := store.ClaimDelivery(pending[0].ID, now, now+120)
		require.Nil(t, err)
		require.True(t, claimed)
		require.Nil(t, store.MarkDeliveryDelivered(pending[0].ID, now+1))

		delivery, err := store.Delivery(testWebPushEndpoint, "m_bbbbbbbbbbbb")
		require.Nil(t, err)
		require.Equal(t, webpush.DeliveryStatusDelivered, delivery.Status)
		require.Equal(t, 1, delivery.Attempts)
		require.Equal(t, now+1, delivery.DeliveredAt)

		// Delivered rows are never pending again, even far in the future
		recovered, err := store.PendingDeliveries(now+100000, 100)
		require.Nil(t, err)
		require.Empty(t, recovered)

		// A late duplicate claim does nothing
		claimed, err = store.ClaimDelivery(pending[0].ID, now+100000, now+100120)
		require.Nil(t, err)
		require.False(t, claimed)
	})
}

// TestStoreDeliveryRetryAndDead verifies retry backoff scheduling and the terminal dead state.
func TestStoreDeliveryRetryAndDead(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		subs := subscriptionsForTestTopic(t, store, "topic1")
		now := time.Now().Unix()
		enqueueTestDelivery(t, store, "m_cccccccccccc", "topic1", now+3600, subs)

		pending, err := store.PendingDeliveries(now, 100)
		require.Nil(t, err)
		require.Len(t, pending, 1)
		id := pending[0].ID

		// First attempt fails retryably: scheduled into the future
		claimed, err := store.ClaimDelivery(id, now, now+120)
		require.Nil(t, err)
		require.True(t, claimed)
		require.Nil(t, store.MarkDeliveryRetry(id, now+1, now+60, "connection refused"))
		pending, err = store.PendingDeliveries(now+30, 100)
		require.Nil(t, err)
		require.Empty(t, pending)
		pending, err = store.PendingDeliveries(now+61, 100)
		require.Nil(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, 1, pending[0].Attempts)
		require.Equal(t, "connection refused", storeDeliveryLastError(t, store, testWebPushEndpoint, "m_cccccccccccc"))

		// Second attempt gives up: dead rows leave the queue
		claimed, err = store.ClaimDelivery(id, now+61, now+181)
		require.Nil(t, err)
		require.True(t, claimed)
		require.Nil(t, store.MarkDeliveryDead(id, now+62, "max attempts reached"))
		pending, err = store.PendingDeliveries(now+100000, 100)
		require.Nil(t, err)
		require.Empty(t, pending)
		delivery, err := store.Delivery(testWebPushEndpoint, "m_cccccccccccc")
		require.Nil(t, err)
		require.Equal(t, webpush.DeliveryStatusDead, delivery.Status)
		require.Equal(t, 2, delivery.Attempts)
	})
}

func storeDeliveryLastError(t *testing.T, store *webpush.Store, endpoint, messageID string) string {
	t.Helper()
	delivery, err := store.Delivery(endpoint, messageID)
	require.Nil(t, err)
	return delivery.LastError
}

// TestStoreDeliveryExpire marks pending rows dead once their message passed its expiry horizon.
func TestStoreDeliveryExpire(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		subs := subscriptionsForTestTopic(t, store, "topic1")
		now := time.Now().Unix()
		enqueueTestDelivery(t, store, "m_dddddddddddd", "topic1", now-1, subs) // Already expired

		affected, err := store.ExpirePendingDeliveries(now)
		require.Nil(t, err)
		require.Equal(t, int64(1), affected)
		delivery, err := store.Delivery(testWebPushEndpoint, "m_dddddddddddd")
		require.Nil(t, err)
		require.Equal(t, webpush.DeliveryStatusDead, delivery.Status)

		// Idempotent: nothing to expire on the next manager run
		affected, err = store.ExpirePendingDeliveries(now)
		require.Nil(t, err)
		require.Equal(t, int64(0), affected)
	})
}

// TestStoreDeliverySubscriptionCascade verifies that deleting or expiring a subscription also
// removes its delivery rows and, once no rows remain, the stored payload.
func TestStoreDeliverySubscriptionCascade(t *testing.T) {
	t.Run("by_endpoint", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, store *webpush.Store) {
			require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"0", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
			require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"1", "auth-key", "p256dh-key", "u_5678", netip.MustParseAddr("9.9.9.9"), []string{"topic1"}))
			subs := subscriptionsForTestTopic(t, store, "topic1")
			now := time.Now().Unix()
			enqueueTestDelivery(t, store, "m_eeeeeeeeeeee", "topic1", now+3600, subs)

			require.Nil(t, store.RemoveSubscriptionsByEndpoint(testWebPushEndpoint+"0"))
			_, err := store.Delivery(testWebPushEndpoint+"0", "m_eeeeeeeeeeee")
			require.ErrorIs(t, err, webpush.ErrWebPushDeliveryNotFound)
			// The other subscription's row survives
			_, err = store.Delivery(testWebPushEndpoint+"1", "m_eeeeeeeeeeee")
			require.Nil(t, err)

			// Removing the remaining row orphans the payload row, which is then swept
			require.Nil(t, store.RemoveSubscriptionsByEndpoint(testWebPushEndpoint+"1"))
			orphaned, err := store.RemoveOrphanedDeliveryMessages()
			require.Nil(t, err)
			require.Equal(t, int64(1), orphaned)
		})
	})
	t.Run("by_user_id", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, store *webpush.Store) {
			require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
			subs := subscriptionsForTestTopic(t, store, "topic1")
			enqueueTestDelivery(t, store, "m_ffffffffffff", "topic1", time.Now().Unix()+3600, subs)
			require.Nil(t, store.RemoveSubscriptionsByUserID("u_1234"))
			_, err := store.Delivery(testWebPushEndpoint, "m_ffffffffffff")
			require.ErrorIs(t, err, webpush.ErrWebPushDeliveryNotFound)
		})
	})
	t.Run("by_age", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, store *webpush.Store) {
			require.Nil(t, store.UpsertSubscription(testWebPushEndpoint, "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
			subs := subscriptionsForTestTopic(t, store, "topic1")
			enqueueTestDelivery(t, store, "m_111111111111", "topic1", time.Now().Unix()+3600, subs)
			require.Nil(t, store.SetSubscriptionUpdatedAt(testWebPushEndpoint, time.Now().Add(-10*24*time.Hour).Unix()))
			require.Nil(t, store.RemoveExpiredSubscriptions(9*24*time.Hour))
			_, err := store.Delivery(testWebPushEndpoint, "m_111111111111")
			require.ErrorIs(t, err, webpush.ErrWebPushDeliveryNotFound)
		})
	})
}

// TestStoreDeliveryRetentionSweep deletes old terminal rows but keeps fresh and pending ones.
func TestStoreDeliveryRetentionSweep(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store *webpush.Store) {
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"0", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		require.Nil(t, store.UpsertSubscription(testWebPushEndpoint+"1", "auth-key", "p256dh-key", "u_1234", netip.MustParseAddr("1.2.3.4"), []string{"topic1"}))
		subs0 := subscriptionsForTestTopic(t, store, "topic1")
		now := time.Now().Unix()

		// One delivered row, one still-pending row
		enqueueTestDelivery(t, store, "m_222222222222", "topic1", now+3600, subs0[:1])
		pending, err := store.PendingDeliveries(now, 100)
		require.Nil(t, err)
		require.Len(t, pending, 1)
		require.True(t, mustClaim(t, store, pending[0].ID, now))
		require.Nil(t, store.MarkDeliveryDelivered(pending[0].ID, now))

		enqueueTestDelivery(t, store, "m_333333333333", "topic1", now+3600, subs0[1:])

		// Nothing older than a future cutoff... everything is younger than now+10s except nothing
		removed, err := store.RemoveOldDeliveries(now-10, 100)
		require.Nil(t, err)
		require.Equal(t, int64(0), removed)

		// A cutoff past the delivered row's update removes it but leaves the pending row
		removed, err = store.RemoveOldDeliveries(now+10, 100)
		require.Nil(t, err)
		require.Equal(t, int64(1), removed)
		_, err = store.Delivery(testWebPushEndpoint+"0", "m_222222222222")
		require.ErrorIs(t, err, webpush.ErrWebPushDeliveryNotFound)
		pending, err = store.PendingDeliveries(now, 100)
		require.Nil(t, err)
		require.Len(t, pending, 1)

		// The delivered message payload is now orphaned; the pending one is not
		orphaned, err := store.RemoveOrphanedDeliveryMessages()
		require.Nil(t, err)
		require.Equal(t, int64(1), orphaned)
	})
}

func mustClaim(t *testing.T, store *webpush.Store, id string, now int64) bool {
	t.Helper()
	claimed, err := store.ClaimDelivery(id, now, now+120)
	require.Nil(t, err)
	return claimed
}
