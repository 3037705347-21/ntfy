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
	// gapWindowDuration is how long a reported delivery gap keeps widening that topic's replays
	gapWindowDuration = 5 * time.Minute

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
// The subscribers of those topics are closed, so their clients reconnect and replay with since=,
// which is what every ntfy client already does.
//
// Their own markers are not enough, though: an independent newer message on the same topic may
// have arrived in the meantime (from this node, or from another peer), leaving the marker past
// the hole, where neither its time nor its row id can reach back. So a dated report also opens a
// gap window for those topics, and while it is open this node serves since= from the gap's own
// position (see widenSince). The close waits for replayDelay first, because the lost messages
// were accepted on the peer and are still in its cache write batch.
func (s *Server) handleDeliveryGap(topics []string, since int64) {
	if since > 0 && !slices.Contains(topics, cluster.GapAllTopics) {
		s.openGapWindow(topics, since)
	}
	log.Tag(tagCluster).Info("Closing the subscribers of %d gapped topic(s) in %v, so their clients replay", len(topics), s.replayDelay)
	time.AfterFunc(s.replayDelay, func() {
		if !s.stopped.Load() {
			s.closeGappedSubscribers(topics)
		}
	})
}

// openGapWindow remembers, per topic, the position a peer's gap started at, so replays for that
// topic reach back to it until the window expires
func (s *Server) openGapWindow(topics []string, since int64) {
	expires := time.Now().Add(gapWindowDuration).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for topic, gap := range s.gapWindows { // Cheap to prune here: the map only holds gapped topics
		if gap.expires < time.Now().Unix() {
			delete(s.gapWindows, topic)
		}
	}
	for _, topic := range topics {
		if gap, ok := s.gapWindows[topic]; !ok || since < gap.since {
			s.gapWindows[topic] = &gapWindow{since: since, expires: expires}
		}
	}
}

// widenSince returns the position to replay this topic from: the client's own marker, or the
// start of a recently reported delivery gap when that is older (see handleDeliveryGap). It never
// narrows a marker, so a client that has been away longer than the gap still gets its history.
func (s *Server) widenSince(topicID string, since model.SinceMarker) model.SinceMarker {
	if since.IsNone() || since.IsAll() {
		return since // Nothing to replay, or everything already
	}
	s.mu.RLock()
	gap, ok := s.gapWindows[topicID]
	s.mu.RUnlock()
	if !ok || gap.expires < time.Now().Unix() {
		return since
	}
	if since.IsID() {
		m, err := s.messageCache.Message(since.ID())
		if err != nil {
			return since // Unknown marker: the replay falls back to the topic's history anyway
		}
		if m.Time <= gap.since {
			return since
		}
	} else if !since.IsLatest() && since.Time().Unix() <= gap.since {
		return since
	}
	log.Tag(tagCluster).Debug("Replaying topic %s from the gap at %v instead of the client's marker", topicID, time.Unix(gap.since, 0))
	return model.NewSinceTime(gap.since)
}

// closeGappedSubscribers closes the subscribers of the given topics, so their clients reconnect
// with since= and replay
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
