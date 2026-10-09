//go:build !nowebpush

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/stretchr/testify/require"
	"heckel.io/ntfy/v2/user"
	"heckel.io/ntfy/v2/util"
	wpush "heckel.io/ntfy/v2/webpush"
)

const (
	testWebPushEndpoint = "https://updates.push.services.mozilla.com/wpush/v1/AAABBCCCDDEEEFFF"
)

func TestServer_WebPush_Enabled(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfig(t, databaseURL)
		conf.WebRoot = "" // Disable web app
		s := newTestServer(t, conf)

		rr := request(t, s, "GET", "/manifest.webmanifest", "", nil)
		require.Equal(t, 404, rr.Code)

		conf2 := newTestConfig(t, databaseURL)
		s2 := newTestServer(t, conf2)

		rr = request(t, s2, "GET", "/manifest.webmanifest", "", nil)
		require.Equal(t, 404, rr.Code)

		conf3 := newTestConfigWithWebPush(t, databaseURL)
		s3 := newTestServer(t, conf3)

		rr = request(t, s3, "GET", "/manifest.webmanifest", "", nil)
		require.Equal(t, 200, rr.Code)
		require.Equal(t, "application/manifest+json", rr.Header().Get("Content-Type"))

	})
}
func TestServer_WebPush_Disabled(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfig(t, databaseURL))

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{"test-topic"}, testWebPushEndpoint), nil)
		require.Equal(t, 404, response.Code)
	})
}

func TestServer_WebPush_TopicAdd(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{"test-topic"}, testWebPushEndpoint), nil)
		require.Equal(t, 200, response.Code)
		require.Equal(t, `{"success":true}`+"\n", response.Body.String())

		subs, err := s.webPush.SubscriptionsForTopic("test-topic")
		require.Nil(t, err)

		require.Len(t, subs, 1)
		require.Equal(t, subs[0].Endpoint, testWebPushEndpoint)
		require.Equal(t, subs[0].P256dh, "p256dh-key")
		require.Equal(t, subs[0].Auth, "auth-key")
		require.Equal(t, subs[0].UserID, "")
	})
}

func TestServer_WebPush_TopicAdd_InvalidEndpoint(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{"test-topic"}, "https://ddos-target.example.com/webpush"), nil)
		require.Equal(t, 400, response.Code)
		require.Equal(t, `{"code":40039,"http":400,"error":"invalid request: web push endpoint unknown"}`+"\n", response.Body.String())
	})
}

func TestServer_WebPush_EndpointRegex(t *testing.T) {
	// Synthetic endpoint samples representing each supported push service host shape.
	allowed := []string{
		// Google FCM (legacy send, webpush, preprod webpush)
		"https://fcm.googleapis.com/fcm/send/FAKETOKEN:APA91b-placeholder-not-a-real-token",
		"https://fcm.googleapis.com/wp/FAKETOKEN:APA91b-placeholder-not-a-real-token",
		"https://fcm.googleapis.com/preprod/wp/FAKETOKEN:APA91b-placeholder-not-a-real-token",
		"https://jmt17.google.com/fcm/send/FAKETOKEN:APA91b-placeholder-not-a-real-token",
		// Mozilla autopush (v1 legacy, v2 current, plus AWS-hosted infra)
		"https://updates.push.services.mozilla.com/wpush/v1/placeholder-not-a-real-token",
		"https://updates.push.services.mozilla.com/wpush/v2/placeholder-not-a-real-token",
		"https://autopush.mozaws.net/wpush/v1/placeholder-not-a-real-token",
		// Apple Web Push
		"https://web.push.apple.com/placeholder-not-a-real-token",
		// Microsoft WNS: instance-specific "wns2-<region>" prefix is wildcarded
		"https://wns2-bn3p.notify.windows.com/w/?token=placeholder",
		"https://wns2-ch1p.notify.windows.com/w/?token=placeholder",
		"https://wns2-par02p.notify.windows.com/w/?token=placeholder",
		"https://wns2-pn1p.notify.windows.com/w/?token=placeholder",
		"https://wns2-am3p.notify.windows.com/w/?token=placeholder",
	}
	denied := []string{
		// HTTP (not HTTPS)
		"http://fcm.googleapis.com/fcm/send/abc",
		// Unrelated host
		"https://attacker.example.com/webpush",
		// GHSA-w9hq-5jg7-q4j7 bypass: allowed host embedded in path
		"https://attacker.com/x.google.com/push",
		"https://attacker.example.com/fcm.googleapis.com/fcm/send/abc",
		"https://evil.test/web.push.apple.com/3/device/abc",
		"https://ntfytest.requestcatcher.com/path.google.com/push",
		"https://ntfytest.requestcatcher.com/a.google.com/toto",
		"https://ntfytest.requestcatcher.com/bypass.google.com/test",
		"https://webhook.site/86e94e2e-2af4-4a31-a80b-e2f335cc6495/path.google.com/push",
		"https://webhook.site/86e94e2e-2af4-4a31-a80b-e2f335cc6495/bypass.google.com/",
		// Allowed host as a prefix of a different host (no separating slash)
		"https://fcm.googleapis.com.attacker.com/fcm/send/abc",
		"https://web.push.apple.com.evil.test/tok",
		// Allowed host as a suffix of a different host (no separating dot)
		"https://evilgoogle.com/",
		"https://notapple.com/",
		// Credentials/userinfo in the URL pointing at a different host
		"https://fcm.googleapis.com@attacker.com/fcm/send/abc",
		// Previously allowed by the wildcard allowlist but not actually used by Web Push
		"https://api.push.apple.com/3/device/abc",
		"https://android.googleapis.com/send/xyz",
		"https://login.microsoft.com/anything",
		// Bare notify.windows.com with no subdomain label
		"https://notify.windows.com/w/?token=abc",
	}
	for _, endpoint := range allowed {
		require.Truef(t, webPushEndpointAllowed(endpoint), "expected endpoint to be allowed: %s", endpoint)
	}
	for _, endpoint := range denied {
		require.Falsef(t, webPushEndpointAllowed(endpoint), "expected endpoint to be denied: %s", endpoint)
	}
}

func TestServer_WebPush_TopicAdd_BypassAttempt(t *testing.T) {
	// Regression test for GHSA-w9hq-5jg7-q4j7: the allow-list regex previously had no
	// end anchor, so a URL like https://attacker.example.com/x.google.com/... passed
	// validation and caused the server to deliver push payloads to attacker-controlled
	// endpoints (SSRF + message exfiltration via attacker-supplied p256dh key).
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{"test-topic"}, "https://attacker.example.com/x.google.com/push"), nil)
		require.Equal(t, 400, response.Code)
		require.Equal(t, `{"code":40039,"http":400,"error":"invalid request: web push endpoint unknown"}`+"\n", response.Body.String())
	})
}

func TestServer_WebPush_TopicAdd_TooManyTopics(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		topicList := make([]string, 51)
		for i := range topicList {
			topicList[i] = util.RandomString(5)
		}

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, topicList, testWebPushEndpoint), nil)
		require.Equal(t, 400, response.Code)
		require.Equal(t, `{"code":40040,"http":400,"error":"invalid request: too many web push topic subscriptions"}`+"\n", response.Body.String())
	})
}

func TestServer_WebPush_TopicUnsubscribe(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		addSubscription(t, s, testWebPushEndpoint, "test-topic")
		requireSubscriptionCount(t, s, "test-topic", 1)

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{}, testWebPushEndpoint), nil)
		require.Equal(t, 200, response.Code)
		require.Equal(t, `{"success":true}`+"\n", response.Body.String())

		requireSubscriptionCount(t, s, "test-topic", 0)
	})
}

func TestServer_WebPush_Delete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		addSubscription(t, s, testWebPushEndpoint, "test-topic")
		requireSubscriptionCount(t, s, "test-topic", 1)

		response := request(t, s, "DELETE", "/v1/webpush", fmt.Sprintf(`{"endpoint":"%s"}`, testWebPushEndpoint), nil)
		require.Equal(t, 200, response.Code)
		require.Equal(t, `{"success":true}`+"\n", response.Body.String())

		requireSubscriptionCount(t, s, "test-topic", 0)
	})
}

func TestServer_WebPush_TopicSubscribeProtected_Allowed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		config := configureAuth(t, newTestConfigWithWebPush(t, databaseURL))
		config.AuthDefault = user.PermissionDenyAll
		s := newTestServer(t, config)

		require.Nil(t, s.userManager.AddUser("ben", "ben", user.RoleUser, false))
		require.Nil(t, s.userManager.AllowAccess("ben", "test-topic", user.PermissionReadWrite))

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{"test-topic"}, testWebPushEndpoint), map[string]string{
			"Authorization": util.BasicAuth("ben", "ben"),
		})
		require.Equal(t, 200, response.Code)
		require.Equal(t, `{"success":true}`+"\n", response.Body.String())

		subs, err := s.webPush.SubscriptionsForTopic("test-topic")
		require.Nil(t, err)
		require.Len(t, subs, 1)
		require.True(t, strings.HasPrefix(subs[0].UserID, "u_"))
	})
}

func TestServer_WebPush_TopicSubscribeProtected_Denied(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		config := configureAuth(t, newTestConfigWithWebPush(t, databaseURL))
		config.AuthDefault = user.PermissionDenyAll
		s := newTestServer(t, config)

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{"test-topic"}, testWebPushEndpoint), nil)
		require.Equal(t, 403, response.Code)

		requireSubscriptionCount(t, s, "test-topic", 0)
	})
}

func TestServer_WebPush_DeleteAccountUnsubscribe(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		config := configureAuth(t, newTestConfigWithWebPush(t, databaseURL))
		s := newTestServer(t, config)

		require.Nil(t, s.userManager.AddUser("ben", "ben", user.RoleUser, false))
		require.Nil(t, s.userManager.AllowAccess("ben", "test-topic", user.PermissionReadWrite))

		response := request(t, s, "POST", "/v1/webpush", payloadForTopics(t, []string{"test-topic"}, testWebPushEndpoint), map[string]string{
			"Authorization": util.BasicAuth("ben", "ben"),
		})

		require.Equal(t, 200, response.Code)
		require.Equal(t, `{"success":true}`+"\n", response.Body.String())

		requireSubscriptionCount(t, s, "test-topic", 1)

		request(t, s, "DELETE", "/v1/account", `{"password":"ben"}`, map[string]string{
			"Authorization": util.BasicAuth("ben", "ben"),
		})
		// should've been deleted with the account
		requireSubscriptionCount(t, s, "test-topic", 0)
	})
}

func TestServer_WebPush_Publish(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		var received atomic.Bool
		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			require.Nil(t, err)
			require.Equal(t, "/push-receive", r.URL.Path)
			require.Equal(t, "high", r.Header.Get("Urgency"))
			require.Equal(t, "", r.Header.Get("Topic"))
			received.Store(true)
		}))
		defer pushService.Close()

		addSubscription(t, s, pushService.URL+"/push-receive", "test-topic")
		request(t, s, "POST", "/test-topic", "web push test", nil)

		waitFor(t, func() bool {
			return received.Load()
		})
	})
}

func TestServer_WebPush_Publish_RemoveOnError(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		var received atomic.Bool
		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			require.Nil(t, err)
			w.WriteHeader(http.StatusGone)
			received.Store(true)
		}))
		defer pushService.Close()

		addSubscription(t, s, pushService.URL+"/push-receive", "test-topic", "test-topic-abc")
		requireSubscriptionCount(t, s, "test-topic", 1)
		requireSubscriptionCount(t, s, "test-topic-abc", 1)

		request(t, s, "POST", "/test-topic", "web push test", nil)

		// Receiving the 410 should've caused the publisher to expire all subscriptions on the endpoint
		waitFor(t, func() bool {
			subs, err := s.webPush.SubscriptionsForTopic("test-topic")
			require.Nil(t, err)
			return len(subs) == 0
		})
		requireSubscriptionCount(t, s, "test-topic-abc", 0)
	})
}

func TestServer_WebPush_Expiry(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		var received atomic.Bool

		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			require.Nil(t, err)
			w.WriteHeader(200)
			w.Write([]byte(``))
			received.Store(true)
		}))
		defer pushService.Close()

		endpoint := pushService.URL + "/push-receive"
		addSubscription(t, s, endpoint, "test-topic")
		requireSubscriptionCount(t, s, "test-topic", 1)

		require.Nil(t, s.webPush.SetSubscriptionUpdatedAt(endpoint, time.Now().Add(-55*24*time.Hour).Unix()))

		s.pruneAndNotifyWebPushSubscriptions()
		requireSubscriptionCount(t, s, "test-topic", 1)

		waitFor(t, func() bool {
			return received.Load()
		})

		require.Nil(t, s.webPush.SetSubscriptionUpdatedAt(endpoint, time.Now().Add(-60*24*time.Hour).Unix()))

		s.pruneAndNotifyWebPushSubscriptions()
		waitFor(t, func() bool {
			subs, err := s.webPush.SubscriptionsForTopic("test-topic")
			require.Nil(t, err)
			return len(subs) == 0
		})
	})
}

// TestServer_WebPush_Publish_RetryThenDeliver verifies that a retryable push-service failure
// (500) leaves the subscription and a pending ledger row in place, that a later worker pass
// delivers the same message (exactly one retry), and that confirmed messages are never sent again.
func TestServer_WebPush_Publish_RetryThenDeliver(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		var status atomic.Int32
		status.Store(http.StatusInternalServerError)
		var requests atomic.Int32
		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			require.Nil(t, err)
			requests.Add(1)
			w.WriteHeader(int(status.Load()))
		}))
		defer pushService.Close()

		endpoint := pushService.URL + "/push-receive"
		addSubscription(t, s, endpoint, "test-topic")
		rr := request(t, s, "POST", "/test-topic", "web push test", nil)
		messageID := publishedMessageID(t, rr)

		// First attempt fails with 500: row is pending, subscription survives
		waitFor(t, func() bool { return requests.Load() == 1 })
		waitForDeliveryRetryScheduled(t, s, endpoint, messageID)
		requireSubscriptionCount(t, s, "test-topic", 1)
		delivery := requireDelivery(t, s, endpoint, messageID)
		require.Equal(t, wpush.DeliveryStatusPending, delivery.Status)
		require.Equal(t, 1, delivery.Attempts)
		require.NotEmpty(t, delivery.LastError)

		// Push service recovers; the next worker pass delivers the pending row
		status.Store(http.StatusOK)
		require.Nil(t, s.webPush.SetDeliveryNextAttemptAt(endpoint, messageID, time.Now().Add(-time.Minute).Unix()))
		require.Nil(t, s.processWebPushDeliveries())
		waitFor(t, func() bool { return requests.Load() == 2 })
		waitForDeliveryStatus(t, s, endpoint, messageID, wpush.DeliveryStatusDelivered)
		delivery = requireDelivery(t, s, endpoint, messageID)
		require.Equal(t, wpush.DeliveryStatusDelivered, delivery.Status)
		require.Equal(t, 2, delivery.Attempts)
		requireSubscriptionCount(t, s, "test-topic", 1)

		// Further worker ticks must never push the confirmed message again
		require.Nil(t, s.webPush.SetDeliveryNextAttemptAt(endpoint, messageID, time.Now().Add(-time.Minute).Unix()))
		require.Nil(t, s.processWebPushDeliveries())
		require.Nil(t, s.processWebPushDeliveries())
		require.Equal(t, int32(2), requests.Load())
	})
}

// TestServer_WebPush_Publish_TransientNetworkError verifies that a dead push service
// (connection reset) does not delete the subscription anymore: the row is retried and delivered
// once the service is reachable again.
func TestServer_WebPush_Publish_TransientNetworkError(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		var fail atomic.Bool
		fail.Store(true)
		var requests atomic.Int32
		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			requests.Add(1)
			if fail.Load() {
				if hijacker, ok := w.(http.Hijacker); ok {
					if conn, _, err := hijacker.Hijack(); err == nil {
						conn.Close() // Simulate the connection dying before any response
						return
					}
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer pushService.Close()

		endpoint := pushService.URL + "/push-receive"
		addSubscription(t, s, endpoint, "test-topic")
		rr := request(t, s, "POST", "/test-topic", "web push test", nil)
		messageID := publishedMessageID(t, rr)

		waitFor(t, func() bool { return requests.Load() == 1 })
		waitForDeliveryRetryScheduled(t, s, endpoint, messageID)
		requireSubscriptionCount(t, s, "test-topic", 1) // Not removed despite the network error
		delivery := requireDelivery(t, s, endpoint, messageID)
		require.Equal(t, wpush.DeliveryStatusPending, delivery.Status)

		// Service comes back: retry succeeds
		fail.Store(false)
		require.Nil(t, s.webPush.SetDeliveryNextAttemptAt(endpoint, messageID, time.Now().Add(-time.Minute).Unix()))
		require.Nil(t, s.processWebPushDeliveries())
		waitFor(t, func() bool { return requests.Load() == 2 })
		waitForDeliveryStatus(t, s, endpoint, messageID, wpush.DeliveryStatusDelivered)
		require.Equal(t, wpush.DeliveryStatusDelivered, requireDelivery(t, s, endpoint, messageID).Status)
	})
}

// TestServer_WebPush_Publish_RestartResumesPending verifies that pending rows survive a server
// restart: a fresh server process reusing the same database resumes and delivers an unconfirmed
// message without the publisher doing anything.
func TestServer_WebPush_Publish_RestartResumesPending(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfigWithWebPush(t, databaseURL)
		s := newTestServer(t, conf)

		var status atomic.Int32
		status.Store(http.StatusInternalServerError)
		var requests atomic.Int32
		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			requests.Add(1)
			w.WriteHeader(int(status.Load()))
		}))
		defer pushService.Close()

		endpoint := pushService.URL + "/push-receive"
		addSubscription(t, s, endpoint, "test-topic")
		rr := request(t, s, "POST", "/test-topic", "web push test", nil)
		messageID := publishedMessageID(t, rr)
		waitFor(t, func() bool { return requests.Load() == 1 })
		waitForDeliveryRetryScheduled(t, s, endpoint, messageID)

		// Backoff would schedule the retry later; simulate time passing, then "restart" the server
		require.Nil(t, s.webPush.SetDeliveryNextAttemptAt(endpoint, messageID, time.Now().Add(-time.Minute).Unix()))
		s.Stop()
		status.Store(http.StatusOK)

		restartedConf := newTestConfig(t, databaseURL)
		restartedConf.WebPushEmailAddress = conf.WebPushEmailAddress
		restartedConf.WebPushPrivateKey = conf.WebPushPrivateKey
		restartedConf.WebPushPublicKey = conf.WebPushPublicKey
		restartedConf.WebPushFile = conf.WebPushFile
		s2 := newTestServer(t, restartedConf)

		// This is the immediate post-startup pass runWebPushDeliveryWorker performs
		require.Nil(t, s2.processWebPushDeliveries())
		waitFor(t, func() bool { return requests.Load() == 2 })
		waitForDeliveryStatus(t, s2, endpoint, messageID, wpush.DeliveryStatusDelivered)
		require.Equal(t, wpush.DeliveryStatusDelivered, requireDelivery(t, s2, endpoint, messageID).Status)
		requireSubscriptionCount(t, s2, "test-topic", 1)
	})
}

// TestServer_WebPush_Publish_410CascadesLedger verifies that a dead subscription (410 Gone) is
// removed and its delivery ledger rows are cleaned up with it.
func TestServer_WebPush_Publish_410CascadesLedger(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s := newTestServer(t, newTestConfigWithWebPush(t, databaseURL))

		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusGone)
		}))
		defer pushService.Close()

		endpoint := pushService.URL + "/push-receive"
		addSubscription(t, s, endpoint, "test-topic", "test-topic-abc")
		rr := request(t, s, "POST", "/test-topic", "web push test", nil)
		messageID := publishedMessageID(t, rr)

		// Wait until the invalid subscription is actually removed; the ledger-row absence alone is
		// an ambiguous gate because the row may not have been enqueued yet on a slow database.
		waitFor(t, func() bool {
			subs, err := s.webPush.SubscriptionsForTopic("test-topic")
			require.Nil(t, err)
			return len(subs) == 0
		})
		requireSubscriptionCount(t, s, "test-topic", 0)
		requireSubscriptionCount(t, s, "test-topic-abc", 0)
		_, err := s.webPush.Delivery(endpoint, messageID)
		require.ErrorIs(t, err, wpush.ErrWebPushDeliveryNotFound)
	})
}

// TestServer_WebPush_DeliveryCleanup verifies the manager-side ledger maintenance prunes old
// delivered rows and sweeps their now-orphaned payloads.
func TestServer_WebPush_DeliveryCleanup(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfigWithWebPush(t, databaseURL)
		conf.WebPushDeliveryRetentionDuration = 0 // Every terminal row is immediately old enough to prune
		s := newTestServer(t, conf)

		pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer pushService.Close()

		endpoint := pushService.URL + "/push-receive"
		addSubscription(t, s, endpoint, "test-topic")
		rr := request(t, s, "POST", "/test-topic", "web push test", nil)
		messageID := publishedMessageID(t, rr)
		waitForDeliveryStatus(t, s, endpoint, messageID, wpush.DeliveryStatusDelivered)

		require.Nil(t, s.cleanupWebPushDeliveries())
		_, err := s.webPush.Delivery(endpoint, messageID)
		require.ErrorIs(t, err, wpush.ErrWebPushDeliveryNotFound)
	})
}

func waitForDeliveryStatus(t *testing.T, s *Server, endpoint, messageID string, status int) {
	t.Helper()
	waitFor(t, func() bool {
		d, err := s.webPush.Delivery(endpoint, messageID)
		return err == nil && d.Status == status
	})
}

// waitForDeliveryRetryScheduled waits until the first failed attempt has been written back as a
// scheduled retry (attempt counter incremented and error recorded).
func waitForDeliveryRetryScheduled(t *testing.T, s *Server, endpoint, messageID string) {
	t.Helper()
	waitFor(t, func() bool {
		d, err := s.webPush.Delivery(endpoint, messageID)
		return err == nil && d.Attempts == 1 && d.LastError != ""
	})
}

func publishedMessageID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var published struct {
		ID string `json:"id"`
	}
	require.Nil(t, json.Unmarshal(rr.Body.Bytes(), &published))
	require.NotEmpty(t, published.ID)
	return published.ID
}

func requireDelivery(t *testing.T, s *Server, endpoint, messageID string) *wpush.Delivery {
	t.Helper()
	delivery, err := s.webPush.Delivery(endpoint, messageID)
	require.Nil(t, err)
	return delivery
}

func payloadForTopics(t *testing.T, topics []string, endpoint string) string {
	topicsJSON, err := json.Marshal(topics)
	require.Nil(t, err)

	return fmt.Sprintf(`{
		"topics": %s,
		"endpoint": "%s",
		"p256dh": "p256dh-key",
		"auth": "auth-key"
	}`, topicsJSON, endpoint)
}

func addSubscription(t *testing.T, s *Server, endpoint string, topics ...string) {
	require.Nil(t, s.webPush.UpsertSubscription(endpoint, "kSC3T8aN1JCQxxPdrFLrZg", "BMKKbxdUU_xLS7G1Wh5AN8PvWOjCzkCuKZYb8apcqYrDxjOF_2piggBnoJLQYx9IeSD70fNuwawI3e9Y8m3S3PE", "u_123", netip.MustParseAddr("1.2.3.4"), topics)) // Test auth and p256dh
}

func requireSubscriptionCount(t *testing.T, s *Server, topic string, expectedLength int) {
	subs, err := s.webPush.SubscriptionsForTopic(topic)
	require.Nil(t, err)
	require.Len(t, subs, expectedLength)
}

func newTestConfigWithWebPush(t *testing.T, databaseURL string) *Config {
	conf := newTestConfig(t, databaseURL)
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	require.Nil(t, err)
	if conf.DatabaseURL == "" {
		conf.WebPushFile = filepath.Join(t.TempDir(), "webpush.db")
	}
	conf.WebPushEmailAddress = "testing@example.com"
	conf.WebPushPrivateKey = privateKey
	conf.WebPushPublicKey = publicKey
	return conf
}
