package server

import (
	"fmt"
	"sort"
	"strings"

	"heckel.io/ntfy/v2/log"
	"heckel.io/ntfy/v2/message"
	"heckel.io/ntfy/v2/util"
)

const (
	tagAlias = "alias"

	// topicAliasSeparator separates an alias from its canonical topic in a "topic-aliases"
	// config entry, e.g. "old-alerts=alerts"
	topicAliasSeparator = "="
)

// ParseTopicAliases parses "alias=canonical" config entries into an alias map. Several aliases
// may point at the same canonical topic, but an alias can only be listed once. The result is
// validated by validateTopicAliases before use.
func ParseTopicAliases(entries []string) (map[string]string, error) {
	aliases := make(map[string]string)
	for _, entry := range entries {
		alias, canonical, found := strings.Cut(entry, topicAliasSeparator)
		if !found {
			return nil, fmt.Errorf("invalid topic alias %q: expected alias%scanonical, e.g. old-topic%scurrent-topic", entry, topicAliasSeparator, topicAliasSeparator)
		}
		alias = strings.TrimSpace(alias)
		canonical = strings.TrimSpace(canonical)
		if existing, dup := aliases[alias]; dup {
			return nil, fmt.Errorf("invalid topic alias %q: alias %q is mapped more than once (to %q and %q)", entry, alias, existing, canonical)
		}
		aliases[alias] = canonical
	}
	return aliases, nil
}

// validateTopicAliases ensures that alias mappings are well-formed:
//   - both names are valid topic names, and neither is on the disallowed list
//   - an alias does not map to itself
//   - mappings are flat: a canonical name must not itself be an alias (no chains or cycles)
func validateTopicAliases(aliases map[string]string, disallowed []string) error {
	for alias, canonical := range aliases {
		if !topicRegex.MatchString(alias) {
			return fmt.Errorf("invalid topic alias %q: not a valid topic name", alias)
		}
		if !topicRegex.MatchString(canonical) {
			return fmt.Errorf("invalid canonical topic %q for alias %q: not a valid topic name", canonical, alias)
		}
		if alias == canonical {
			return fmt.Errorf("invalid topic alias %q: an alias cannot map to itself", alias)
		}
		if util.Contains(disallowed, alias) {
			return fmt.Errorf("invalid topic alias %q: alias name is a disallowed/reserved topic", alias)
		}
		if util.Contains(disallowed, canonical) {
			return fmt.Errorf("invalid topic alias %q: canonical topic %q is a disallowed/reserved topic", alias, canonical)
		}
		if _, isAlias := aliases[canonical]; isAlias {
			return fmt.Errorf("invalid topic alias %q: canonical topic %q is itself an alias; chains of aliases are not supported", alias, canonical)
		}
	}
	return nil
}

// canonicalTopicID resolves a topic name through the configured topic aliases. Alias config is
// immutable after startup, so this needs no locking.
func (s *Server) canonicalTopicID(id string) string {
	if canonical, ok := s.config.TopicAliases[id]; ok {
		return canonical
	}
	return id
}

// migrateTopicAliases merges cached messages stored under alias topics into their canonical
// topics once, at startup. The underlying update is idempotent, so restarts (and other cluster
// nodes sharing the database) cannot lose or duplicate messages.
func migrateTopicAliases(cache *message.Cache, aliases map[string]string) error {
	if len(aliases) == 0 {
		return nil
	}
	names := make([]string, 0, len(aliases))
	for alias := range aliases {
		names = append(names, alias)
	}
	sort.Strings(names) // Deterministic migration order in logs
	for _, alias := range names {
		canonical := aliases[alias]
		moved, err := cache.RenameTopic(alias, canonical)
		if err != nil {
			return fmt.Errorf("cannot merge topic alias %q into %q: %w", alias, canonical, err)
		}
		if moved > 0 {
			log.
				Tag(tagAlias).
				Fields(log.Context{
					"topic_alias":     alias,
					"topic":           canonical,
					"messages_merged": moved,
				}).
				Info("Merged %d cached message(s) from alias topic %q into %q", moved, alias, canonical)
		} else {
			log.
				Tag(tagAlias).
				Fields(log.Context{
					"topic_alias": alias,
					"topic":       canonical,
				}).
				Debug("Topic alias %q mapped to %q: no cached messages to merge", alias, canonical)
		}
	}
	return nil
}
