package attachment

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"heckel.io/ntfy/v2/log"
	"heckel.io/ntfy/v2/model"
)

const tagRetention = "attachment_retention"

// retentionWordRegex matches maximal runs of the characters that make up a message ID.
// Candidate runs are then validated with model.ValidMessageID, so the ID length does not
// have to be hard-coded here.
var retentionWordRegex = regexp.MustCompile(`[A-Za-z0-9]+`)

// Retention holds the set of attachment IDs that must never be auto-deleted. It is loaded
// from a plaintext manifest file (one ID per line) and can be hot-reloaded. All methods are
// safe for concurrent use. A nil *Retention disables pinning and reports no pinned IDs.
type Retention struct {
	file string
	mu   sync.RWMutex
	ids  map[string]struct{}
}

// NewRetention loads the retention manifest at file and returns a Retention. An empty path
// disables retention and returns nil, nil. If the file does not exist yet, the retention set
// starts empty (the file may be mounted later). Any other read/parse error is returned so the
// caller can fail closed at startup instead of silently unpinning everything.
func NewRetention(file string) (*Retention, error) {
	if file == "" {
		return nil, nil
	}
	r := &Retention{
		file: file,
		ids:  make(map[string]struct{}),
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// File returns the path the retention set is loaded from.
func (r *Retention) File() string {
	if r == nil {
		return ""
	}
	return r.file
}

// Reload re-reads the manifest and atomically swaps in the newly parsed set. On a read or
// parse error the previously loaded set is kept (fail-closed); a missing file is treated as
// an empty set.
func (r *Retention) Reload() error {
	if r == nil {
		return nil
	}
	ids, err := readRetentionFile(r.file)
	if err != nil {
		log.Tag(tagRetention).Err(err).Warn("Failed to reload attachment retention file %s; keeping %d pinned ID(s)", r.file, r.Len())
		return err
	}
	r.mu.Lock()
	r.ids = ids
	r.mu.Unlock()
	log.Tag(tagRetention).Debug("Attachment retention set now holds %d pinned ID(s) from %s", len(ids), r.file)
	return nil
}

// Contains reports whether the attachment id is pinned.
func (r *Retention) Contains(id string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.ids[id]
	return ok
}

// IDs returns a sorted snapshot of the pinned IDs. The deterministic order keeps SQL IN
// clauses and log output stable.
func (r *Retention) IDs() ([]string, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	ids := make([]string, 0, len(r.ids))
	for id := range r.ids {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	slices.Sort(ids)
	return ids, nil
}

// Len returns the number of pinned IDs.
func (r *Retention) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ids)
}

// readRetentionFile parses the manifest into a set of IDs. Blank lines and lines starting
// with "#" are ignored. Each remaining line may contain a bare message/attachment ID, an ID
// with a file extension, or a full /file/<ID> URL; the ID is extracted and validated. A
// missing file yields an empty set, while any other read error is returned.
func readRetentionFile(file string) (map[string]struct{}, error) {
	f, err := os.Open(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Tag(tagRetention).Warn("Attachment retention file %s does not exist yet; no attachments are pinned", file)
			return make(map[string]struct{}), nil
		}
		return nil, err
	}
	defer f.Close()
	ids := make(map[string]struct{})
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id := parseRetentionID(line)
		if !model.ValidMessageID(id) {
			log.Tag(tagRetention).Warn("Ignoring invalid attachment ID on line %d of %s: %q", lineNo, file, line)
			continue
		}
		ids[id] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// parseRetentionID extracts a bare attachment/message ID from a manifest line. The line may
// be just the ID, "<ID>.<ext>", or a full URL containing a "/file/<ID>" segment.
func parseRetentionID(line string) string {
	if model.ValidMessageID(line) {
		return line
	}
	if i := strings.Index(line, retentionFilePathMarker); i >= 0 {
		rest := strings.TrimSpace(line[i+len(retentionFilePathMarker):])
		if j := strings.IndexAny(rest, "/?#&."); j >= 0 {
			rest = rest[:j]
		}
		if model.ValidMessageID(rest) {
			return rest
		}
	}
	for _, word := range retentionWordRegex.FindAllString(line, -1) {
		if model.ValidMessageID(word) {
			return word
		}
	}
	return ""
}

const retentionFilePathMarker = "/file/"
