package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseSMTPAliases(t *testing.T) {
	domain, prefix := "ntfy.sh", "ntfy-"
	tests := []struct {
		name    string
		yaml    string
		want    map[string]string
		wantErr string
	}{
		{
			name: "bare local parts",
			yaml: "alert: ops-alerts\nbackup: nightly-backups\n",
			want: map[string]string{"alert": "ops-alerts", "backup": "nightly-backups"},
		},
		{
			name: "full addresses and prefix are normalized away",
			yaml: "alert@ntfy.sh: a\nntfy-backup@ntfy.sh: b\nALERT2: c\n",
			want: map[string]string{"alert": "a", "backup": "b", "alert2": "c"},
		},
		{
			name: "address with dots is allowed even though it is no topic name",
			yaml: "daily.alerts: ops-alerts\n",
			want: map[string]string{"daily.alerts": "ops-alerts"},
		},
		{
			name: "multiple addresses may share a topic",
			yaml: "alert: shared\nsupport: shared\n",
			want: map[string]string{"alert": "shared", "support": "shared"},
		},
		{
			name: "empty document is valid (no aliases)",
			yaml: "",
			want: map[string]string{},
		},
		{
			name:    "duplicate YAML key maps one address to two topics",
			yaml:    "alert: topic-a\nalert: topic-b\n",
			wantErr: "two topics",
		},
		{
			name:    "equivalent key spellings are a conflict",
			yaml:    "alert: topic-a\nalert@ntfy.sh: topic-b\n",
			wantErr: "two topics",
		},
		{
			name:    "case variants are a conflict",
			yaml:    "Alert: topic-a\nalert: topic-b\n",
			wantErr: "two topics",
		},
		{
			name:    "prefix spelling is a conflict",
			yaml:    "alert: topic-a\nntfy-alert@ntfy.sh: topic-b\n",
			wantErr: "two topics",
		},
		{
			name:    "invalid topic with slash",
			yaml:    "alert: team/alerts\n",
			wantErr: "invalid topic",
		},
		{
			name:    "invalid topic empty",
			yaml:    "alert: \"\"\n",
			wantErr: "invalid topic",
		},
		{
			name:    "non-string topic",
			yaml:    "alert: 123\n",
			wantErr: "topic string",
		},
		{
			name:    "wrong domain",
			yaml:    "alert@example.com: topic-a\n",
			wantErr: "does not match smtp-server-domain",
		},
		{
			name:    "plus is reserved for the token",
			yaml:    "alert+tk_x: topic-a\n",
			wantErr: "'+'",
		},
		{
			name:    "illegal characters in local part",
			yaml:    "al ert: topic-a\n",
			wantErr: "invalid local part",
		},
		{
			name:    "broken yaml",
			yaml:    "alert: [unclosed\n",
			wantErr: "yaml",
		},
		{
			name:    "top level list rejected",
			yaml:    "- alert: topic-a\n",
			wantErr: "must be a recipient address string",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSMTPAliases([]byte(tc.yaml), domain, prefix)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestSMTPAliasStore_LookupIsCaseInsensitive(t *testing.T) {
	file := writeAliasFile(t, "Alert: ops-alerts\n")
	s := newSMTPAliasStore(file, "ntfy.sh", "")
	defer s.Close()
	topic, ok := s.topicFor("alert")
	require.True(t, ok)
	require.Equal(t, "ops-alerts", topic)
	_, ok = s.topicFor("unknown")
	require.False(t, ok)
}

func TestSMTPAliasStore_HotReload(t *testing.T) {
	file := writeAliasFile(t, "alert: topic-a\n")
	s := newSMTPAliasStoreWithInterval(file, "ntfy.sh", "", 10*time.Millisecond)
	defer s.Close()

	// Initial mapping is active
	require.Equal(t, 1, s.count())
	waitFor(t, func() bool { tpc, ok := s.topicFor("alert"); return ok && tpc == "topic-a" })

	// Add an entry: takes effect without restart
	writeAliasFileAt(t, file, "alert: topic-a\nsupport: topic-b\n", time.Unix(100, 1))
	waitFor(t, func() bool {
		tpc, ok := s.topicFor("support")
		return ok && tpc == "topic-b" && s.count() == 2
	})

	// Remove an entry: address falls back to default parsing
	writeAliasFileAt(t, file, "support: topic-b\n", time.Unix(200, 1))
	waitFor(t, func() bool {
		_, ok := s.topicFor("alert")
		return !ok && s.count() == 1
	})

	// Conflicting file: whole file is rejected, previous mapping survives
	writeAliasFileAt(t, file, "support: topic-b\nsupport: topic-c\n", time.Unix(300, 1))
	waitFor(t, func() bool { mod, _ := s.fileState(); return mod.Equal(time.Unix(300, 1)) })
	tpc, ok := s.topicFor("support")
	require.True(t, ok)
	require.Equal(t, "topic-b", tpc)

	// Fix the file: new mapping applies
	writeAliasFileAt(t, file, "support: topic-c\n", time.Unix(400, 1))
	waitFor(t, func() bool { tpc, ok := s.topicFor("support"); return ok && tpc == "topic-c" })

	// Delete the file: all aliases clear
	require.NoError(t, os.Remove(file))
	waitFor(t, func() bool {
		_, exists := s.fileState()
		return s.count() == 0 && !exists
	})
	_, ok = s.topicFor("support")
	require.False(t, ok)
}

func TestSMTPAliasStore_InvalidInitialFileKeepsEmptyAndRecovers(t *testing.T) {
	file := writeAliasFile(t, "alert: topic-a\nalert: topic-b\n")
	s := newSMTPAliasStoreWithInterval(file, "ntfy.sh", "", 10*time.Millisecond)
	defer s.Close()
	require.Equal(t, 0, s.count())

	writeAliasFileAt(t, file, "alert: topic-a\n", time.Unix(100, 1))
	waitFor(t, func() bool { tpc, ok := s.topicFor("alert"); return ok && tpc == "topic-a" })
}

func TestSmtpBackend_AliasFile(t *testing.T) {
	file := writeAliasFile(t, "ntfy-alert@ntfy.sh: ops-alerts\n")
	aliases := newSMTPAliasStore(file, "ntfy.sh", "ntfy-")
	defer aliases.Close()
	email := `EHLO example.com
MAIL FROM: phil@example.com
RCPT TO: ntfy-alert@ntfy.sh
DATA
Subject: Aliased

what's up
.
`
	s, c, _, scanner := newTestSMTPServerWithAliases(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/ops-alerts", r.URL.Path)
		require.Equal(t, "Aliased", r.Header.Get("Title"))
		require.Equal(t, "what's up", readAll(t, r.Body))
	}, aliases)
	defer s.Close()
	defer c.Close()
	writeAndReadUntilLine(t, email, c, scanner, "250 2.0.0 OK: queued")
}

func TestSmtpBackend_AliasFile_UnmappedAddressFallsBackToTopic(t *testing.T) {
	file := writeAliasFile(t, "alert: ops-alerts\n")
	aliases := newSMTPAliasStore(file, "ntfy.sh", "ntfy-")
	defer aliases.Close()
	email := `EHLO example.com
MAIL FROM: phil@example.com
RCPT TO: ntfy-plain-topic@ntfy.sh
DATA
Subject: Not aliased

what's up
.
`
	s, c, _, scanner := newTestSMTPServerWithAliases(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/plain-topic", r.URL.Path) // Address parsed as topic name
	}, aliases)
	defer s.Close()
	defer c.Close()
	writeAndReadUntilLine(t, email, c, scanner, "250 2.0.0 OK: queued")
}

func TestSmtpBackend_AliasFile_DottedFixedAddress(t *testing.T) {
	// "daily.alerts" is not a legal topic name, so without an alias this recipient is rejected.
	file := writeAliasFile(t, "daily.alerts: ops-alerts\n")
	aliases := newSMTPAliasStore(file, "ntfy.sh", "ntfy-")
	defer aliases.Close()
	email := `EHLO example.com
MAIL FROM: phil@example.com
RCPT TO: ntfy-daily.alerts@ntfy.sh
DATA
Subject: Cron report

what's up
.
`
	s, c, _, scanner := newTestSMTPServerWithAliases(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/ops-alerts", r.URL.Path)
	}, aliases)
	defer s.Close()
	defer c.Close()
	writeAndReadUntilLine(t, email, c, scanner, "250 2.0.0 OK: queued")
}

func TestSmtpBackend_AliasFile_TokenStillWorks(t *testing.T) {
	file := writeAliasFile(t, "alert: ops-alerts\n")
	aliases := newSMTPAliasStore(file, "ntfy.sh", "ntfy-")
	defer aliases.Close()
	email := `EHLO example.com
MAIL FROM: phil@example.com
RCPT TO: ntfy-alert+tk_AbC123@ntfy.sh
DATA
Subject: Authenticated aliased mail

what's up
.
`
	s, c, _, scanner := newTestSMTPServerWithAliases(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/ops-alerts", r.URL.Path)
		require.Equal(t, "Bearer tk_AbC123", r.Header.Get("Authorization"))
	}, aliases)
	defer s.Close()
	defer c.Close()
	writeAndReadUntilLine(t, email, c, scanner, "250 2.0.0 OK: queued")
}

func TestSmtpBackend_AliasFile_HotReloadAndConflict(t *testing.T) {
	file := writeAliasFile(t, "alert: topic-a\n")
	aliases := newSMTPAliasStoreWithInterval(file, "ntfy.sh", "ntfy-", 10*time.Millisecond)
	defer aliases.Close()

	var mu sync.Mutex
	received := make([]string, 0)
	s, c, _, scanner := newTestSMTPServerWithAliases(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, r.URL.Path)
	}, aliases)
	defer s.Close()
	defer c.Close()

	mailToAlert := func() {
		email := `EHLO example.com
MAIL FROM: phil@example.com
RCPT TO: ntfy-alert@ntfy.sh
DATA
Subject: hi

what's up
.
`
		writeAndReadUntilLine(t, email, c, scanner, "250 2.0.0 OK: queued")
	}

	mailToAlert() // Initial mapping

	// Change the mapping: next mail goes to topic-b
	writeAliasFileAt(t, file, "alert: topic-b\n", time.Unix(100, 1))
	waitFor(t, func() bool { tpc, ok := aliases.topicFor("alert"); return ok && tpc == "topic-b" })
	mailToAlert()

	// A conflicting file must not apply: routing stays on topic-b
	writeAliasFileAt(t, file, "alert: topic-b\nalert: topic-c\n", time.Unix(200, 1))
	waitFor(t, func() bool { mod, _ := aliases.fileState(); return mod.Equal(time.Unix(200, 1)) })
	tpc, ok := aliases.topicFor("alert")
	require.True(t, ok)
	require.Equal(t, "topic-b", tpc)
	mailToAlert()

	require.Equal(t, []string{"/topic-a", "/topic-b", "/topic-b"}, received)
}

func TestSmtpBackend_DottedAddressWithoutAliasIsRejected(t *testing.T) {
	email := `EHLO example.com
MAIL FROM: phil@example.com
RCPT TO: ntfy-daily.alerts@ntfy.sh
DATA
Subject: Cron report

what's up
.
`
	s, c, _, scanner := newTestSMTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("This should not be called")
	})
	defer s.Close()
	defer c.Close()
	writeAndReadUntilLine(t, email, c, scanner, "451 4.0.0 invalid topic")
}

// writeAliasFile creates a temporary alias YAML file and returns its path.
func writeAliasFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "smtp-aliases.yml")
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))
	return file
}

// writeAliasFileAt writes content and pins the mtime so reload detection is deterministic even on
// filesystems with coarse mtime resolution.
func writeAliasFileAt(t *testing.T, file, content string, modTime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))
	require.NoError(t, os.Chtimes(file, modTime, modTime))
}
