package server

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v2"

	"heckel.io/ntfy/v2/log"
)

// smtpAliasReloadInterval is how often the SMTP recipient alias file is checked for changes.
const smtpAliasReloadInterval = 5 * time.Second

// smtpAliasLocalPartRegex restricts which recipient local parts may be aliased. The local part is
// taken after stripping the domain and smtp-server-addr-prefix, and deliberately allows characters
// that are not legal topic names (dots), so fixed addresses need not look like topic names.
var smtpAliasLocalPartRegex = regexp.MustCompile(`^[-_.A-Za-z0-9]{1,64}$`)

// smtpAliasStore maintains the recipient-address -> topic mapping loaded from a YAML file. The file
// is reloaded automatically when it changes: a valid new file replaces the active mapping atomically,
// while an invalid or conflicting file is rejected entirely (with an error logged) and the previously
// loaded mapping stays in effect. Lookup is case-insensitive on the recipient local part.
type smtpAliasStore struct {
	file     string
	domain   string
	prefix   string
	interval time.Duration

	mu      sync.RWMutex
	aliases map[string]string // Normalized recipient local part -> topic

	lastExists bool
	lastMod    time.Time
	lastSize   int64

	closeChan chan struct{}
	closeOnce sync.Once
}

// newSMTPAliasStore loads the alias file once and starts a background reloader. The initial load is
// best-effort: if the file is missing or invalid, the store starts with an empty mapping and keeps
// watching, so fixing (or creating) the file takes effect without a restart.
func newSMTPAliasStore(file, domain, prefix string) *smtpAliasStore {
	return newSMTPAliasStoreWithInterval(file, domain, prefix, smtpAliasReloadInterval)
}

// newSMTPAliasStoreWithInterval is newSMTPAliasStore with a custom reload-check interval (tests use
// a short one). The interval is fixed before the reloader goroutine starts.
func newSMTPAliasStoreWithInterval(file, domain, prefix string, interval time.Duration) *smtpAliasStore {
	s := &smtpAliasStore{
		file:      file,
		domain:    strings.ToLower(strings.TrimSpace(domain)),
		prefix:    prefix,
		interval:  interval,
		aliases:   make(map[string]string),
		closeChan: make(chan struct{}),
	}
	s.reload()
	go s.runReloadLoop()
	return s
}

// topicFor resolves a recipient local part (already stripped of domain, addr-prefix and token) to a
// mapped topic. It returns false when no alias exists, in which case the caller falls back to the
// default behavior of treating the address as a topic name.
func (s *smtpAliasStore) topicFor(localPart string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	topic, ok := s.aliases[strings.ToLower(localPart)]
	return topic, ok
}

// count returns the number of active aliases.
func (s *smtpAliasStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.aliases)
}

// fileState reports the modification time and existence of the file as last observed by the reloader.
func (s *smtpAliasStore) fileState() (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastMod, s.lastExists
}

// Close stops the background reloader. Idempotent.
func (s *smtpAliasStore) Close() {
	s.closeOnce.Do(func() {
		close(s.closeChan)
	})
}

func (s *smtpAliasStore) runReloadLoop() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.reload()
		case <-s.closeChan:
			return
		}
	}
}

// reload re-reads the file when it changed (or appeared/disappeared). Any parse or validation error
// is logged and the active mapping is left untouched, i.e. a broken file never partially applies.
func (s *smtpAliasStore) reload() {
	s.mu.RLock()
	prevExists := s.lastExists
	prevMod, prevSize := s.lastMod, s.lastSize
	prevCount := len(s.aliases)
	s.mu.RUnlock()

	stat, err := os.Stat(s.file)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Tag(tagSMTP).Err(err).Error("Cannot stat SMTP alias file %s; keeping %d alias(es)", s.file, prevCount)
			return
		}
		if !prevExists {
			return // Still absent, nothing to clear
		}
		s.mu.Lock()
		s.aliases = make(map[string]string)
		s.lastExists = false
		s.lastMod, s.lastSize = time.Time{}, 0
		s.mu.Unlock()
		if prevCount > 0 {
			log.Tag(tagSMTP).Warn("SMTP alias file %s was removed; all recipient aliases are back to default address parsing", s.file)
		}
		return
	}
	if prevExists && stat.ModTime().Equal(prevMod) && stat.Size() == prevSize {
		return // Unchanged
	}
	b, err := os.ReadFile(s.file)
	if err != nil {
		log.Tag(tagSMTP).Err(err).Error("Cannot read SMTP alias file %s; keeping %d alias(es)", s.file, prevCount)
		return
	}
	aliases, err := parseSMTPAliases(b, s.domain, s.prefix)
	if err != nil {
		log.Tag(tagSMTP).Err(err).Error("SMTP alias file %s is invalid and was NOT applied; keeping %d existing alias(es)", s.file, prevCount)
		// Remember the file state anyway: once the user edits again (new mtime) we retry.
		s.mu.Lock()
		s.lastExists, s.lastMod, s.lastSize = true, stat.ModTime(), stat.Size()
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.aliases = aliases
	s.lastExists, s.lastMod, s.lastSize = true, stat.ModTime(), stat.Size()
	s.mu.Unlock()
	log.Tag(tagSMTP).Info("Loaded %d SMTP recipient alias(es) from %s", len(aliases), s.file)
}

// parseSMTPAliases parses the alias YAML document and validates it as a whole. It unmarshals into a
// yaml.MapSlice rather than a map so that the same address being mapped twice (a YAML duplicate key,
// or two equivalent key spellings) is detected instead of silently overwritten. Any error rejects
// the entire document.
func parseSMTPAliases(b []byte, domain, prefix string) (map[string]string, error) {
	var entries yaml.MapSlice
	if err := yaml.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	aliases := make(map[string]string, len(entries))
	for _, entry := range entries {
		rawKey, ok := entry.Key.(string)
		if !ok {
			return nil, fmt.Errorf("alias key %v must be a recipient address string", entry.Key)
		}
		topic, ok := entry.Value.(string)
		if !ok {
			return nil, fmt.Errorf("recipient %q must map to a topic string, got %T", rawKey, entry.Value)
		}
		localPart, err := normalizeSMTPAliasKey(rawKey, domain, prefix)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient %q: %s", rawKey, err.Error())
		}
		topic = strings.TrimSpace(topic)
		if !topicRegex.MatchString(topic) {
			return nil, fmt.Errorf("recipient %q maps to invalid topic %q (allowed: %s)", rawKey, topic, topicRegex.String())
		}
		if existing, dup := aliases[localPart]; dup {
			return nil, fmt.Errorf("recipient %q is mapped to two topics (%q and %q); each address may map to only one topic", rawKey, existing, topic)
		}
		aliases[localPart] = topic
	}
	return aliases, nil
}

// normalizeSMTPAliasKey turns an alias-file key into the normalized recipient local part used for
// lookups. Keys may be written as a bare local part ("alert") or a full address ("alert@ntfy.sh",
// or "ntfy-alert@ntfy.sh" when smtp-server-addr-prefix is set); the configured domain and prefix are
// stripped so all spellings of the same address collide during duplicate detection.
func normalizeSMTPAliasKey(key, domain, prefix string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return "", fmt.Errorf("must not be empty")
	}
	if at := strings.LastIndex(k, "@"); at >= 0 {
		localPart, keyDomain := k[:at], strings.ToLower(strings.TrimSpace(k[at+1:]))
		if domain == "" {
			return "", fmt.Errorf("contains domain %q, but no smtp-server-domain is configured", keyDomain)
		}
		if keyDomain != domain {
			return "", fmt.Errorf("domain %q does not match smtp-server-domain %q", keyDomain, domain)
		}
		k = localPart
	}
	if prefix != "" && strings.HasPrefix(k, prefix) {
		k = strings.TrimPrefix(k, prefix)
	}
	k = strings.ToLower(k)
	if strings.Contains(k, "+") {
		return "", fmt.Errorf("must not contain '+' (the part after '+' in a recipient address is the access token)")
	}
	if !smtpAliasLocalPartRegex.MatchString(k) {
		return "", fmt.Errorf("invalid local part %q; allowed characters are letters, digits, '-', '_' and '.'", k)
	}
	return k, nil
}
