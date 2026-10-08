package server

import (
	"slices"
	"time"

	"heckel.io/ntfy/v2/cluster"
	"heckel.io/ntfy/v2/log"
	"heckel.io/ntfy/v2/metrics"
	"heckel.io/ntfy/v2/model"
)

const (
	// gapReplayMaxAge is how old a peer's delivery gap may be for this node to replay it
	// itself; past that the subscribers are closed and their clients replay instead
	gapReplayMaxAge = 5 * time.Minute

	// replayMargin is added to the cache batch timeout and the fan-out linger to cover replica lag
	replayMargin = time.Second
)

// closeLocalSubscribers closes every subscriber connection on this node. The cluster calls it
// while this node is isolated (lost its registration, peers healthy): peers no longer forward
// to it, so its subscribers would silently receive nothing; they reconnect to a healthy node.
func (s *Server) closeLocalSubscribers() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.topics {
		t.CancelAllSubscribers()
	}
}

// deliverFromBus delivers a message received from a peer node (via the cluster) to this
// node's local subscribers. It is the receive-side counterpart to Cluster.ForwardMessage: local
// delivery and all global side effects (Firebase, email, web push, upstream) already ran on the
// origin node, so this only publishes to the local topic, and never re-relays.
func (s *Server) deliverFromBus(m *model.Message) {
	s.mu.RLock()
	t, ok := s.topics[m.Topic]
	s.mu.RUnlock()
	if !ok {
		metrics.ClusterMessagesWasted.Inc() // Relayed here needlessly: this node had no use for the message
		return
	}
	v := s.visitor(m.Sender, nil)
	if err := t.Publish(v, m); err != nil {
		logvm(v, m).Err(err).Warn("Cluster: unable to deliver fan-out message to local subscribers")
	}
}

// handleDeliveryGap repairs what a peer could not deliver to this node (see cluster.GapFunc).
// A dated report is replayed here, from the position the peer lost, because the clients' own
// since= markers cannot ask for it: an independent newer message on the same topic may already
// have moved a marker past the hole. Undated reports (a peer on an older build), "every topic",
// and gaps older than gapReplayMaxAge close the subscribers instead, because there the clients'
// markers are the better source of truth.
func (s *Server) handleDeliveryGap(topics []string, since int64) {
	if since <= 0 || slices.Contains(topics, cluster.GapAllTopics) || time.Since(time.Unix(since, 0)) > gapReplayMaxAge {
		log.Tag(tagCluster).Info("Closing the subscribers of %d gapped topic(s), so their clients replay", len(topics))
		s.closeGappedSubscribers(topics)
		return
	}
	log.Tag(tagCluster).Info("Replaying %d gapped topic(s) from %v", len(topics), time.Unix(since, 0).Format(time.RFC3339))
	go s.replayDeliveryGap(topics, since) // The peer's report must not wait for the replay
}

// replayDeliveryGap re-publishes a gapped range to the local subscribers of those topics, after
// waiting for the lost messages to be readable: they were accepted on the origin node, so they
// sit in its cache write batch for up to replayDelay. Subscribers that did receive part of the
// range get those messages twice and dedupe by id.
func (s *Server) replayDeliveryGap(topics []string, since int64) {
	time.Sleep(s.replayDelay)
	if s.stopped.Load() {
		return
	}
	marker := model.NewSinceTime(since)
	for _, id := range topics {
		s.mu.RLock()
		t, ok := s.topics[id]
		s.mu.RUnlock()
		if !ok {
			continue // Nobody here is subscribed any more, so there is nothing to repair
		}
		messages, err := s.messageCache.Messages(id, marker, false)
		if err != nil {
			log.Tag(tagCluster).Err(err).Field("topic", id).Warn("Cannot read back a peer's delivery gap")
			continue
		}
		for _, m := range messages {
			v := s.visitor(m.Sender, nil)
			if err := t.Publish(v, m); err != nil {
				logvm(v, m).Err(err).Warn("Cluster: unable to replay gapped message to local subscribers")
				continue
			}
			metrics.ClusterGapsReplayed.Inc()
		}
	}
}

// closeGappedSubscribers closes the subscribers of the given topics, so their clients reconnect
// with since= and replay from their own markers
func (s *Server) closeGappedSubscribers(topics []string) {
	if slices.Contains(topics, cluster.GapAllTopics) {
		s.closeLocalSubscribers()
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range topics {
		if t, ok := s.topics[id]; ok {
			t.CancelAllSubscribers()
		}
	}
}
