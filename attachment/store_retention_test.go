package attachment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// forEachPinnedBackend runs f against a file and an S3 store that share the same retention
// manifest content. The callback receives the store, a makeOld helper (see forEachBackend),
// the loaded Retention, and a function to rewrite the manifest. The S3 subtest is skipped if
// NTFY_TEST_S3_URL is not set.
func forEachPinnedBackend(t *testing.T, totalSizeLimit int64, manifest string, f func(t *testing.T, s *Store, makeOld func(string), r *Retention, rewrite func(string))) {
	t.Run("file", func(t *testing.T) {
		attDir := t.TempDir()
		manifestPath := writeRetentionFile(t, manifest)
		r, err := NewRetention(manifestPath)
		require.Nil(t, err)
		s, err := NewFileStore(attDir, totalSizeLimit, time.Hour, nil, r)
		require.Nil(t, err)
		t.Cleanup(func() { s.Close() })
		makeOld := func(id string) {
			oldTime := time.Unix(1, 0)
			require.Nil(t, os.Chtimes(filepath.Join(attDir, id), oldTime, oldTime))
		}
		rewrite := func(content string) {
			require.Nil(t, os.WriteFile(manifestPath, []byte(content), 0600))
		}
		f(t, s, makeOld, r, rewrite)
	})
	t.Run("s3", func(t *testing.T) {
		s, wrapper, r, rewrite := newTestRealS3StoreWithRetention(t, totalSizeLimit, manifest)
		makeOld := func(id string) {
			wrapper.setModTime(id, time.Unix(1, 0))
		}
		f(t, s, makeOld, r, rewrite)
	})
}

func TestStore_SyncKeepsPinnedOrphan(t *testing.T) {
	// k1 is pinned but absent from the database active-set (expired/detached); k3 is an
	// ordinary, unpinned orphan. Both are made old enough for cleanup.
	forEachPinnedBackend(t, testSizeLimit, "abcdefghijk1\n", func(t *testing.T, s *Store, makeOld func(string), _ *Retention, _ func(string)) {
		for i, body := range []string{"file0", "file1", "file2", "file3"} {
			_, err := s.Write("abcdefghijk"+string(rune('0'+i)), strings.NewReader(body), 0)
			require.Nil(t, err)
		}
		s.attachmentsWithSizes = func() (map[string]int64, error) {
			// Database only still knows k0 and k2; k1 and k3 are gone from its perspective.
			return map[string]int64{"abcdefghijk0": 5, "abcdefghijk2": 5}, nil
		}
		makeOld("abcdefghijk1")
		makeOld("abcdefghijk3")

		require.Nil(t, s.sync())

		// Pinned attachment survives despite being an old orphan ...
		r, _, err := s.Read("abcdefghijk1")
		require.Nil(t, err)
		r.Close()

		// ... while the unpinned orphan is deleted, and active attachments remain.
		_, _, err = s.Read("abcdefghijk3")
		require.Error(t, err)
		for _, id := range []string{"abcdefghijk0", "abcdefghijk2"} {
			rr, _, err := s.Read(id)
			require.Nil(t, err)
			rr.Close()
		}
		// Pinned detached object is accounted at its real size (5 + 5 + 5).
		require.Equal(t, int64(15), s.Size())
	})
}

func TestStore_RemoveSkipsPinned(t *testing.T) {
	forEachPinnedBackend(t, testSizeLimit, "abcdefghijk1\n", func(t *testing.T, s *Store, _ func(string), _ *Retention, _ func(string)) {
		_, err := s.Write("abcdefghijk0", strings.NewReader(strings.Repeat("a", 100)), 0)
		require.Nil(t, err)
		_, err = s.Write("abcdefghijk1", strings.NewReader(strings.Repeat("b", 100)), 0)
		require.Nil(t, err)

		require.Nil(t, s.Remove("abcdefghijk0", "abcdefghijk1"))

		_, _, err = s.Read("abcdefghijk0")
		require.Error(t, err) // unpinned was deleted
		r, _, err := s.Read("abcdefghijk1")
		require.Nil(t, err) // pinned was kept
		r.Close()
		require.Equal(t, int64(100), s.Size())

		// Removing the pinned ID on its own is also a no-op.
		require.Nil(t, s.Remove("abcdefghijk1"))
		r, _, err = s.Read("abcdefghijk1")
		require.Nil(t, err)
		r.Close()
	})
}

func TestStore_UnpinRevertsToNormalExpiry(t *testing.T) {
	forEachPinnedBackend(t, testSizeLimit, "abcdefghijk1\n", func(t *testing.T, s *Store, makeOld func(string), _ *Retention, rewrite func(string)) {
		_, err := s.Write("abcdefghijk0", strings.NewReader("file0"), 0)
		require.Nil(t, err)
		_, err = s.Write("abcdefghijk1", strings.NewReader("file1"), 0)
		require.Nil(t, err)
		_, err = s.Write("abcdefghijk2", strings.NewReader("file2"), 0)
		require.Nil(t, err)
		s.attachmentsWithSizes = func() (map[string]int64, error) {
			return map[string]int64{"abcdefghijk0": 5, "abcdefghijk2": 5}, nil
		}
		makeOld("abcdefghijk1")

		// While pinned, sync keeps k1.
		require.Nil(t, s.sync())
		r, _, err := s.Read("abcdefghijk1")
		require.Nil(t, err)
		r.Close()

		// Remove k1 from the manifest; the next sync hot-reloads and deletes it as a normal
		// expired orphan.
		rewrite("# no attachments pinned\n")
		require.Nil(t, s.sync())
		_, _, err = s.Read("abcdefghijk1")
		require.Error(t, err)
		require.Equal(t, int64(10), s.Size())
	})
}

// TestStore_PinnedSurvivesRestart verifies that pinning is derived solely from the manifest
// file, so a brand-new store (simulating a server restart) re-protects already-pinned objects
// without any in-memory state.
func TestStore_PinnedSurvivesRestart(t *testing.T) {
	attDir := t.TempDir()
	manifestPath := writeRetentionFile(t, "abcdefghijkl\n")

	// First "process": upload then let the object look expired/detached.
	s1, err := NewFileStore(attDir, testSizeLimit, time.Hour, nil, nil)
	require.Nil(t, err)
	_, err = s1.Write("abcdefghijkl", strings.NewReader(strings.Repeat("z", 100)), 0)
	require.Nil(t, err)
	oldTime := time.Unix(1, 0)
	require.Nil(t, os.Chtimes(filepath.Join(attDir, "abcdefghijkl"), oldTime, oldTime))
	s1.Close()

	// Simulate a restart: new store, same attachment dir and retention manifest.
	r, err := NewRetention(manifestPath)
	require.Nil(t, err)
	s2, err := NewFileStore(attDir, testSizeLimit, time.Hour, nil, r)
	require.Nil(t, err)
	defer s2.Close()
	s2.attachmentsWithSizes = func() (map[string]int64, error) {
		return map[string]int64{}, nil // database no longer reports the attachment
	}
	require.Nil(t, s2.Sync())

	reader, _, err := s2.Read("abcdefghijkl")
	require.Nil(t, err)
	reader.Close()
	require.Equal(t, int64(100), s2.Size())
}
