//go:build !nowebpush

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"heckel.io/ntfy/v2/log"
	"heckel.io/ntfy/v2/model"
	"heckel.io/ntfy/v2/user"
	"heckel.io/ntfy/v2/util"
	wpush "heckel.io/ntfy/v2/webpush"
)

const (
	// WebPushAvailable is a constant used to indicate that WebPush support is available.
	// It can be disabled with the 'nowebpush' build tag.
	WebPushAvailable = true

	webPushTopicSubscribeLimit = 50

	// webPushDeliveryMaxAttempts is the ceiling on retries for one message/subscription pair.
	webPushDeliveryMaxAttempts = 15
	// webPushDeliveryVisibilityTimeout is how far a claimed row is pushed into the future. If the
	// claiming process dies mid-send, the row becomes due again after this timeout.
	webPushDeliveryVisibilityTimeout = 2 * time.Minute
	// webPushDeliveryBackoffStart / Max bound the exponential retry backoff (30s, 60s, ..., 1h).
	webPushDeliveryBackoffStart = 30 * time.Second
	webPushDeliveryBackoffMax   = time.Hour
)

// webPushAllowedEndpointsRegexes is the host-level allow-list of web push services ntfy
// will deliver to. Each regex anchors the scheme and matches the stable service host,
// followed by the authority/path boundary "/". Instance-specific labels (e.g. the
// "wns2-<region>" prefix on Windows Notification Service hosts) are wildcarded with a
// single-label pattern ([^/]+) that cannot span into the path.
// See GHSA-w9hq-5jg7-q4j7 for why wildcarding the entire host is insufficient.
var webPushAllowedEndpointsRegexes = []*regexp.Regexp{
	regexp.MustCompile(`^https://fcm\.googleapis\.com/`),
	regexp.MustCompile(`^https://jmt17\.google\.com/`),
	regexp.MustCompile(`^https://updates\.push\.services\.mozilla\.com/`),
	regexp.MustCompile(`^https://[^/]+\.mozaws\.net/`),
	regexp.MustCompile(`^https://web\.push\.apple\.com/`),
	regexp.MustCompile(`^https://[^/]+\.notify\.windows\.com/`),
}

func webPushEndpointAllowed(endpoint string) bool {
	for _, re := range webPushAllowedEndpointsRegexes {
		if re.MatchString(endpoint) {
			return true
		}
	}
	return false
}

// webPushSendStatus classifies the outcome of one push-service HTTP request.
type webPushSendStatus int

const (
	webPushSendOK                  webPushSendStatus = iota // 2xx: the push service accepted the message
	webPushSendRetry                                        // Transient failure (transport, 429/413/5xx): keep the subscription and retry later
	webPushSendInvalidSubscription                          // Invalid subscription (404/410/other 4xx): the endpoint must be removed
)

// webPushSendResult is the classified outcome of sendWebPushNotification.
type webPushSendResult struct {
	status     webPushSendStatus
	statusCode int
	retryAfter time.Duration // Parsed from the Retry-After header on 429/503, zero otherwise
	err        error
}

func (s *Server) handleWebPushUpdate(w http.ResponseWriter, r *http.Request, v *visitor) error {
	req, err := readJSONWithLimit[apiWebPushUpdateSubscriptionRequest](r.Body, jsonBodyBytesLimit, false)
	if err != nil || req.Endpoint == "" || req.Auth == "" || req.P256dh == "" {
		return errHTTPBadRequestWebPushSubscriptionInvalid
	} else if !webPushEndpointAllowed(req.Endpoint) {
		return errHTTPBadRequestWebPushEndpointUnknown
	} else if len(req.Topics) > webPushTopicSubscribeLimit {
		return errHTTPBadRequestWebPushTopicCountTooHigh
	}
	topics, err := s.topicsFromIDs(v, req.Topics...)
	if err != nil {
		return err
	}
	if s.userManager != nil {
		u := v.User()
		for _, t := range topics {
			if err := s.userManager.Authorize(u, t.ID, user.PermissionRead); err != nil {
				logvr(v, r).With(t).Err(err).Debug("Access to topic %s not authorized", t.ID)
				return errHTTPForbidden.With(t)
			}
		}
	}
	if err := s.webPush.UpsertSubscription(req.Endpoint, req.Auth, req.P256dh, v.MaybeUserID(), v.IP(), req.Topics); err != nil {
		return err
	}
	return s.writeJSON(w, newSuccessResponse())
}

func (s *Server) handleWebPushDelete(w http.ResponseWriter, r *http.Request, _ *visitor) error {
	req, err := readJSONWithLimit[apiWebPushUpdateSubscriptionRequest](r.Body, jsonBodyBytesLimit, false)
	if err != nil || req.Endpoint == "" {
		return errHTTPBadRequestWebPushSubscriptionInvalid
	}
	if err := s.webPush.RemoveSubscriptionsByEndpoint(req.Endpoint); err != nil {
		return err
	}
	return s.writeJSON(w, newSuccessResponse())
}

// publishToWebPushEndpoints fans one message out to every browser subscription on the message's
// topic. Delivery is driven by the persistent ledger: rows are written first (one per
// subscription), then processed immediately so the normal online path keeps its latency. The
// ledger makes delivery idempotent and crash-safe; the background worker (runWebPushDeliveryWorker)
// retries anything this attempt does not confirm.
func (s *Server) publishToWebPushEndpoints(v *visitor, m *model.Message) {
	subscriptions, err := s.webPush.SubscriptionsForTopic(m.Topic)
	if err != nil {
		logvm(v, m).Err(err).With(v, m).Warn("Unable to publish web push messages")
		return
	} else if len(subscriptions) == 0 {
		return
	}
	log.Tag(tagWebPush).With(v, m).Debug("Publishing web push message to %d subscribers", len(subscriptions))
	payload, err := json.Marshal(newWebPushPayload(fmt.Sprintf("%s/%s", s.config.BaseURL, m.Topic), m.ForJSON()))
	if err != nil {
		log.Tag(tagWebPush).Err(err).With(v, m).Warn("Unable to marshal web push payload")
		return
	}
	// Messages that are not cached have no Expires; bound pending delivery by the same horizon the
	// push service is told to keep the message (the Web Push TTL, see sendWebPushNotification).
	expiresAt := m.Expires
	if expiresAt == 0 {
		expiresAt = time.Now().Add(s.config.CacheDuration).Unix()
	}
	if err := s.webPush.EnqueueDeliveries(m.ID, m.Topic, payload, expiresAt, subscriptions); err != nil {
		log.Tag(tagWebPush).Err(err).With(v, m).Warn("Unable to enqueue web push messages")
		return
	}
	if err := s.processWebPushDeliveries(); err != nil {
		log.Tag(tagWebPush).Err(err).With(v, m).Warn("Unable to process web push deliveries")
	}
}

// runWebPushDeliveryWorker retries unconfirmed ledger rows on a ticker. It runs one pass
// immediately at startup so pending messages are resumed right after a restart, and prefers the
// cluster leader (per-row claiming makes a non-leader harmless during failover).
func (s *Server) runWebPushDeliveryWorker() {
	if s.config.WebPushPublicKey == "" {
		return
	}
	for {
		if s.cluster.IsLeader() {
			if err := s.processWebPushDeliveries(); err != nil {
				log.Tag(tagWebPush).Err(err).Warn("Unable to process web push deliveries")
			}
		}
		select {
		case <-time.After(s.config.WebPushDeliveryInterval):
		case <-s.closeChan:
			return
		}
	}
}

// processWebPushDeliveries claims and sends all due pending rows. Selecting rows and claiming
// them are separate steps: the conditional claim (ClaimDelivery) is what guarantees at most one
// worker or cluster node sends a given row at a time.
func (s *Server) processWebPushDeliveries() error {
	now := time.Now()
	pending, err := s.webPush.PendingDeliveries(now.Unix(), s.config.ManagerBatchSize)
	if err != nil {
		return err
	}
	for _, d := range pending {
		s.deliverWebPushMessage(d, now)
	}
	return nil
}

func (s *Server) deliverWebPushMessage(d *wpush.PendingDelivery, now time.Time) {
	claimed, err := s.webPush.ClaimDelivery(d.ID, now.Unix(), now.Add(webPushDeliveryVisibilityTimeout).Unix())
	if err != nil {
		log.Tag(tagWebPush).Err(err).With(d).Warn("Unable to claim web push delivery")
		return
	} else if !claimed {
		return // Another worker (or cluster node) is already sending this row
	}
	result := s.sendWebPushNotification(d.Subscription, d.Payload, d)
	finishedAt := time.Now()
	switch result.status {
	case webPushSendOK:
		if err := s.webPush.MarkDeliveryDelivered(d.ID, finishedAt.Unix()); err != nil {
			log.Tag(tagWebPush).Err(err).With(d).Warn("Unable to mark web push delivery as delivered")
		}
	case webPushSendInvalidSubscription:
		log.Tag(tagWebPush).With(d, d.Subscription).Field("response_code", result.statusCode).Debug("Web push subscription is invalid, removing endpoint")
		if err := s.webPush.RemoveSubscriptionsByEndpoint(d.Subscription.Endpoint); err != nil {
			log.Tag(tagWebPush).Err(err).With(d, d.Subscription).Warn("Unable to remove invalid web push subscription")
		}
	default:
		s.rescheduleWebPushDelivery(d, result, finishedAt)
	}
}

// rescheduleWebPushDelivery either schedules the next retry with backoff or gives up (dead) when
// the attempt ceiling is reached or the retry would land after the message's expiry horizon.
func (s *Server) rescheduleWebPushDelivery(d *wpush.PendingDelivery, result *webPushSendResult, now time.Time) {
	attempts := d.Attempts + 1 // ClaimDelivery already incremented the stored attempt counter
	lastError := webPushDeliveryError(result)
	nextAttemptAt := now.Add(webPushDeliveryBackoff(attempts, result.retryAfter))
	if attempts >= webPushDeliveryMaxAttempts || now.Unix() >= d.ExpiresAt || nextAttemptAt.Unix() >= d.ExpiresAt {
		if err := s.webPush.MarkDeliveryDead(d.ID, now.Unix(), lastError); err != nil {
			log.Tag(tagWebPush).Err(err).With(d).Warn("Unable to mark web push delivery as dead")
			return
		}
		log.Tag(tagWebPush).With(d).Field("attempt", attempts).Warn("Giving up on web push message after %d failed attempt(s): %s", attempts, lastError)
		return
	}
	if err := s.webPush.MarkDeliveryRetry(d.ID, now.Unix(), nextAttemptAt.Unix(), lastError); err != nil {
		log.Tag(tagWebPush).Err(err).With(d).Warn("Unable to reschedule web push delivery")
		return
	}
	log.Tag(tagWebPush).With(d, d.Subscription).Field("attempt", attempts).Debug("Web push delivery failed, will retry at %s: %s", util.FormatTime(nextAttemptAt), lastError)
}

// webPushDeliveryBackoff returns the wait before the next attempt: the push service's
// Retry-After hint if present, otherwise capped exponential backoff.
func webPushDeliveryBackoff(attempts int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	backoff := webPushDeliveryBackoffStart << (attempts - 1)
	if attempts <= 0 || backoff <= 0 || backoff > webPushDeliveryBackoffMax {
		return webPushDeliveryBackoffMax
	}
	return backoff
}

func webPushDeliveryError(result *webPushSendResult) string {
	if result.statusCode == 0 && result.err != nil {
		return result.err.Error()
	} else if result.err != nil {
		return fmt.Sprintf("push service responded with status %d: %s", result.statusCode, result.err.Error())
	}
	return fmt.Sprintf("push service responded with status %d", result.statusCode)
}

func parseWebPushRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value) // Delta-seconds form; the HTTP-date form is ignored
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (s *Server) pruneAndNotifyWebPushSubscriptions() {
	if s.config.WebPushPublicKey == "" {
		return
	}
	go func() {
		if err := s.pruneAndNotifyWebPushSubscriptionsInternal(); err != nil {
			log.Tag(tagWebPush).Err(err).Warn("Unable to prune or notify web push subscriptions")
		}
	}()
}

func (s *Server) pruneAndNotifyWebPushSubscriptionsInternal() error {
	// Expire old subscriptions (foreign-key cascade removes their ledger rows too)
	if err := s.webPush.RemoveExpiredSubscriptions(s.config.WebPushExpiryDuration); err != nil {
		return err
	}
	// Maintain the delivery ledger: give up on messages past their expiry, prune old terminal
	// rows and payloads, so the long-term ledger stays bounded.
	if err := s.cleanupWebPushDeliveries(); err != nil {
		return err
	}
	// Notify subscriptions that will expire soon
	subscriptions, err := s.webPush.SubscriptionsExpiring(s.config.WebPushExpiryWarningDuration)
	if err != nil {
		return err
	} else if len(subscriptions) == 0 {
		return nil
	}
	payload, err := json.Marshal(newWebPushSubscriptionExpiringPayload())
	if err != nil {
		return err
	}
	warningSent := make([]*wpush.Subscription, 0)
	for _, subscription := range subscriptions {
		result := s.sendWebPushNotification(subscription, payload)
		if result.status == webPushSendOK {
			warningSent = append(warningSent, subscription)
			continue
		}
		log.Tag(tagWebPush).Err(result.err).With(subscription).Field("response_code", result.statusCode).Warn("Unable to publish expiry imminent warning")
		if result.status == webPushSendInvalidSubscription {
			if err := s.webPush.RemoveSubscriptionsByEndpoint(subscription.Endpoint); err != nil {
				return err
			}
		}
	}
	if err := s.webPush.MarkExpiryWarningSent(warningSent); err != nil {
		return err
	}
	log.Tag(tagWebPush).Debug("Expired old subscriptions and published %d expiry imminent warnings", len(warningSent))
	return nil
}

// cleanupWebPushDeliveries performs the periodic ledger maintenance: pending rows past their
// message's expiry are given up on, terminal rows older than the retention duration are pruned,
// and payloads no longer referenced by any row are removed.
func (s *Server) cleanupWebPushDeliveries() error {
	now := time.Now()
	expired, err := s.webPush.ExpirePendingDeliveries(now.Unix())
	if err != nil {
		return err
	}
	cutoff := now.Add(-s.config.WebPushDeliveryRetentionDuration).Unix()
	var pruned int64
	for {
		n, err := s.webPush.RemoveOldDeliveries(cutoff, s.config.ManagerBatchSize)
		if err != nil {
			return err
		}
		pruned += n
		if n == 0 {
			break
		}
	}
	orphaned, err := s.webPush.RemoveOrphanedDeliveryMessages()
	if err != nil {
		return err
	}
	if expired > 0 || pruned > 0 || orphaned > 0 {
		log.Tag(tagWebPush).Debug("Web push delivery ledger maintenance: %d expired, %d pruned, %d orphaned payloads removed", expired, pruned, orphaned)
	}
	return nil
}

// sendWebPushNotification sends one encrypted payload to one subscription and classifies the
// outcome. Unlike the previous fire-and-forget behavior, a transport error or a 429/413/5xx
// response no longer deletes the subscription: those are transient conditions and the caller
// retries. Only an invalid subscription (404/410/other 4xx) causes the endpoint to be removed.
func (s *Server) sendWebPushNotification(sub *wpush.Subscription, message []byte, contexters ...log.Contexter) *webPushSendResult {
	log.Tag(tagWebPush).With(sub).With(contexters...).Debug("Sending web push message")
	payload := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			Auth:   sub.Auth,
			P256dh: sub.P256dh,
		},
	}
	resp, err := webpush.SendNotification(message, payload, &webpush.Options{
		Subscriber:      s.config.WebPushEmailAddress,
		VAPIDPublicKey:  s.config.WebPushPublicKey,
		VAPIDPrivateKey: s.config.WebPushPrivateKey,
		Urgency:         webpush.UrgencyHigh, // iOS requires this to ensure delivery
		TTL:             int(s.config.CacheDuration.Seconds()),
	})
	if err != nil {
		// Network/push-service failure: transient, keep the subscription and retry
		return &webPushSendResult{status: webPushSendRetry, err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return &webPushSendResult{status: webPushSendOK, statusCode: resp.StatusCode}
	}
	sendErr := errHTTPInternalErrorWebPushUnableToPublish.With(sub).With(contexters...)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestEntityTooLarge || resp.StatusCode >= 500 {
		return &webPushSendResult{
			status:     webPushSendRetry,
			statusCode: resp.StatusCode,
			retryAfter: parseWebPushRetryAfter(resp.Header.Get("Retry-After")),
			err:        sendErr,
		}
	}
	// 404/410 and any other 4xx: the subscription is invalid and must be removed
	return &webPushSendResult{status: webPushSendInvalidSubscription, statusCode: resp.StatusCode, err: sendErr}
}
