package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"heckel.io/ntfy/v2/model"
	"heckel.io/ntfy/v2/user"
	"heckel.io/ntfy/v2/util"
)

func TestParseTopicAliases(t *testing.T) {
	aliases, err := ParseTopicAliases([]string{"old=new", "other = current "})
	require.Nil(t, err)
	require.Equal(t, map[string]string{"old": "new", "other": "current"}, aliases)

	// Missing separator
	_, err = ParseTopicAliases([]string{"noseparator"})
	require.Error(t, err)

	// Duplicate alias
	_, err = ParseTopicAliases([]string{"old=new", "old=other"})
	require.Error(t, err)

	// Empty map is fine
	aliases, err = ParseTopicAliases(nil)
	require.Nil(t, err)
	require.Equal(t, 0, len(aliases))
}

func TestValidateTopicAliases(t *testing.T) {
	disallowed := DefaultDisallowedTopics

	// Valid: two aliases may share one canonical topic
	require.Nil(t, validateTopicAliases(map[string]string{"a": "x", "b": "x"}, disallowed))
	require.Nil(t, validateTopicAliases(map[string]string{}, disallowed))

	// Self mapping
	require.Error(t, validateTopicAliases(map[string]string{"a": "a"}, disallowed))

	// Chains and cycles are rejected
	require.Error(t, validateTopicAliases(map[string]string{"a": "b", "b": "c"}, disallowed))
	require.Error(t, validateTopicAliases(map[string]string{"a": "b", "b": "a"}, disallowed))

	// Invalid topic names
	require.Error(t, validateTopicAliases(map[string]string{"bad/name": "x"}, disallowed))
	require.Error(t, validateTopicAliases(map[string]string{"a": "bad/name"}, disallowed))

	// Disallowed/reserved names on either side
	require.Error(t, validateTopicAliases(map[string]string{"docs": "x"}, disallowed))
	require.Error(t, validateTopicAliases(map[string]string{"a": "metrics"}, disallowed))
}

func TestServer_TopicAlias_PublishSubscribe(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfig(t, databaseURL)
		conf.TopicAliases = map[string]string{"oldtopic": "newtopic"}
		s := newTestServer(t, conf)

		// A live subscriber on the alias receives messages published to the canonical topic
		subscribeRR := httptest.NewRecorder()
		subscribeCancel := subscribe(t, s, "/oldtopic/json", subscribeRR)

		// Publishing via the alias stores and returns the canonical topic name
		response := request(t, s, "PUT", "/oldtopic", "via alias", nil)
		require.Equal(t, 200, response.Code)
		require.Equal(t, "newtopic", toMessage(t, response.Body.String()).Topic)

		// Polling the canonical topic returns the message published via the alias
		response = request(t, s, "GET", "/newtopic/json?poll=1", "", nil)
		require.Equal(t, 200, response.Code)
		messages := toMessages(t, response.Body.String())
		require.Equal(t, 1, len(messages))
		require.Equal(t, "via alias", messages[0].Message)
		require.Equal(t, "newtopic", messages[0].Topic)

		// Publishing via the canonical topic ...
		response = request(t, s, "PUT", "/newtopic", "via canonical", nil)
		require.Equal(t, 200, response.Code)

		// ... is visible when polling the alias, and all rows carry the canonical name
		response = request(t, s, "GET", "/oldtopic/json?poll=1", "", nil)
		require.Equal(t, 200, response.Code)
		messages = toMessages(t, response.Body.String())
		require.Equal(t, 2, len(messages))
		require.Equal(t, "via alias", messages[0].Message)
		require.Equal(t, "via canonical", messages[1].Message)
		for _, m := range messages {
			require.Equal(t, "newtopic", m.Topic)
		}

		// The live alias subscriber got the canonical-topic publish in real time
		time.Sleep(300 * time.Millisecond)
		subscribeCancel()
		messages = toMessages(t, subscribeRR.Body.String())
		require.GreaterOrEqual(t, len(messages), 2) // Open event + message(s)
		found := false
		for _, m := range messages {
			if m.Message == "via canonical" {
				require.Equal(t, "newtopic", m.Topic)
				found = true
			}
		}
		require.True(t, found, "live subscriber on alias did not receive the canonical-topic message")

		// No separate in-memory topic object exists for the alias
		require.Nil(t, s.topics["oldtopic"])
		require.NotNil(t, s.topics["newtopic"])
	})
}

func TestServer_TopicAlias_MultiTopicSubscribeDedup(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfig(t, databaseURL)
		conf.TopicAliases = map[string]string{"oldtopic": "newtopic"}
		s := newTestServer(t, conf)

		require.Equal(t, 200, request(t, s, "PUT", "/newtopic", "only once", nil).Code)

		// One request covering both names must not deliver the message twice
		response := request(t, s, "GET", "/oldtopic,newtopic/json?poll=1&since=all", "", nil)
		require.Equal(t, 200, response.Code)
		messages := toMessages(t, response.Body.String())
		require.Equal(t, 1, len(messages))
		require.Equal(t, "only once", messages[0].Message)
	})
}

func TestServer_TopicAlias_ACLAppliesToCanonical(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfigWithAuthFile(t, databaseURL)
		conf.AuthDefault = user.PermissionReadWrite // Open by default
		conf.TopicAliases = map[string]string{"oldalerts": "announcements"}
		s := newTestServer(t, conf)

		require.Nil(t, s.userManager.AddUser("phil", "phil", user.RoleAdmin, false))
		require.Nil(t, s.userManager.AllowAccess(user.Everyone, "announcements", user.PermissionRead)) // Read-only

		// Anonymous write via the alias is blocked: authorization runs against the canonical topic
		require.Equal(t, 403, request(t, s, "PUT", "/oldalerts", "test", nil).Code)
		require.Equal(t, 403, request(t, s, "PUT", "/announcements", "test", nil).Code)

		// Admin can write via the alias ...
		require.Equal(t, 200, request(t, s, "PUT", "/oldalerts", "admin message", map[string]string{
			"Authorization": util.BasicAuth("phil", "phil"),
		}).Code)

		// ... and anonymous reads via either name succeed
		require.Equal(t, 200, request(t, s, "GET", "/oldalerts/json?poll=1", "", nil).Code)
		require.Equal(t, 200, request(t, s, "GET", "/announcements/json?poll=1", "", nil).Code)
	})
}

func TestServer_TopicAlias_InvalidConfig(t *testing.T) {
	for _, aliases := range []map[string]string{
		{"a": "a"},           // Self mapping
		{"a": "b", "b": "c"}, // Chain
		{"bad/name": "x"},    // Invalid alias name
		{"x": "bad/name"},    // Invalid canonical name
		{"docs": "x"},        // Disallowed alias
		{"x": "metrics"},     // Disallowed canonical
	} {
		conf := newTestConfig(t, "")
		conf.TopicAliases = aliases
		s, err := New(conf)
		require.Error(t, err, "expected error for aliases %v", aliases)
		require.Nil(t, s)
	}
}

// TestServer_TopicAlias_HistoryMigration walks the full lifecycle across server restarts on a
// persistent cache: messages cached under the old name are merged into the canonical topic once
// (no loss, no duplicates), re-merging is a no-op, and removing the mapping later leaves the
// merged history in place while turning the old name back into an independent topic.
func TestServer_TopicAlias_HistoryMigration(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		baseConf := newTestConfig(t, databaseURL)
		cacheFile := baseConf.CacheFile // Empty for Postgres (uses DatabaseURL)

		// Restart the server against the same persistent cache, optionally with alias config
		restart := func(aliases map[string]string) *Server {
			conf := newRestartTestConfig(t, databaseURL, cacheFile)
			conf.TopicAliases = aliases
			s, err := New(conf)
			require.Nil(t, err)
			t.Cleanup(s.Stop)
			return s
		}
		poll := func(s *Server, topic string) []*model.Message {
			response := request(t, s, "GET", "/"+topic+"/json?poll=1&since=all", "", nil)
			require.Equal(t, 200, response.Code)
			return toMessages(t, response.Body.String())
		}

		// Phase 1: no mapping yet; history accumulates under both names independently
		s1 := restart(nil)
		require.Equal(t, 200, request(t, s1, "PUT", "/oldtopic", "old message 1", nil).Code)
		require.Equal(t, 200, request(t, s1, "PUT", "/newtopic", "new message 1", nil).Code)
		require.Equal(t, 200, request(t, s1, "PUT", "/oldtopic", "old message 2", nil).Code)
		require.Equal(t, 2, len(poll(s1, "oldtopic")))
		require.Equal(t, 1, len(poll(s1, "newtopic")))
		s1.Stop()

		// Phase 2: mapping introduced; cached history is merged, no loss and no duplicates
		s2 := restart(map[string]string{"oldtopic": "newtopic"})
		require.Equal(t, []string{"old message 1", "new message 1", "old message 2"}, messageBodies(poll(s2, "newtopic")))
		require.Equal(t, 3, len(poll(s2, "oldtopic"))) // Reads through the alias see the same history

		// New traffic via the alias lands in the canonical topic
		require.Equal(t, 200, request(t, s2, "PUT", "/oldtopic", "after merge", nil).Code)
		require.Equal(t, 4, len(poll(s2, "newtopic")))

		// The alias no longer appears as a topic in the cache
		topics, err := s2.messageCache.Topics()
		require.Nil(t, err)
		require.NotContains(t, topics, "oldtopic")
		require.Contains(t, topics, "newtopic")
		s2.Stop()

		// Phase 3: restart with the mapping still active; migration is idempotent
		s3 := restart(map[string]string{"oldtopic": "newtopic"})
		require.Equal(t, 4, len(poll(s3, "newtopic")))
		require.Equal(t, 4, len(poll(s3, "oldtopic")))
		s3.Stop()

		// Phase 4: mapping removed; merged history stays in the canonical topic, nothing is lost,
		// and the old name is an independent (empty) topic again
		s4 := restart(nil)
		require.Equal(t, 0, len(poll(s4, "oldtopic")))
		require.Equal(t, []string{"old message 1", "new message 1", "old message 2", "after merge"}, messageBodies(poll(s4, "newtopic")))
	})
}

// newRestartTestConfig builds a config pointing at the same persistent cache as a previous
// server instance, so the test can simulate a restart.
func newRestartTestConfig(t *testing.T, databaseURL, cacheFile string) *Config {
	conf := NewConfig()
	conf.BaseURL = "http://127.0.0.1:12345"
	if databaseURL != "" {
		conf.DatabaseURL = databaseURL
	} else {
		conf.CacheFile = cacheFile
	}
	conf.AttachmentCacheDir = t.TempDir()
	conf.TemplateDir = t.TempDir()
	return conf
}

func messageBodies(msgs []*model.Message) []string {
	bodies := make([]string, 0, len(msgs))
	for _, m := range msgs {
		bodies = append(bodies, m.Message)
	}
	return bodies
}
