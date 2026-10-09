package attachment

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeRetentionFile writes content to a file named "retention" in a temp dir and returns path.
func writeRetentionFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "retention")
	require.Nil(t, os.WriteFile(path, []byte(content), 0600))
	return path
}

func TestRetention_DisabledWhenPathEmpty(t *testing.T) {
	r, err := NewRetention("")
	require.Nil(t, err)
	require.Nil(t, r)
}

func TestRetention_MissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	r, err := NewRetention(path)
	require.Nil(t, err)
	require.Equal(t, 0, r.Len())
	require.False(t, r.Contains("abcdefghijkl"))
	ids, err := r.IDs()
	require.Nil(t, err)
	require.Empty(t, ids)
}

func TestRetention_UnreadableFileFailsClosed(t *testing.T) {
	// Pointing the manifest at a directory makes reading it fail; NewRetention must return an
	// error rather than silently starting with an empty set.
	r, err := NewRetention(t.TempDir())
	require.Error(t, err)
	require.Nil(t, r)
}

func TestRetention_ParseManifest(t *testing.T) {
	content := `
# leading comment, then a blank line above

abcdefghijkl
Z9y8X7w6V5u4.jpg
https://ntfy.example.com/file/a1B2c3D4e5F6.png?x=1
abcdefghijkl
# a duplicate of the first ID is ignored above
too-short
0000000000000000000000000
`
	path := writeRetentionFile(t, content)
	r, err := NewRetention(path)
	require.Nil(t, err)

	require.True(t, r.Contains("abcdefghijkl"))
	require.True(t, r.Contains("Z9y8X7w6V5u4"))
	require.True(t, r.Contains("a1B2c3D4e5F6"))
	require.False(t, r.Contains("too-short"))
	require.False(t, r.Contains("00000000000"))

	ids, err := r.IDs()
	require.Nil(t, err)
	// Duplicate collapsed, invalid lines skipped; sorted snapshot
	require.Equal(t, []string{"Z9y8X7w6V5u4", "a1B2c3D4e5F6", "abcdefghijkl"}, ids)
}

func TestRetention_ReloadSwapsSet(t *testing.T) {
	path := writeRetentionFile(t, "abcdefghijkl\nZ9y8X7w6V5u4\n")
	r, err := NewRetention(path)
	require.Nil(t, err)
	require.True(t, r.Contains("abcdefghijkl"))
	require.True(t, r.Contains("Z9y8X7w6V5u4"))

	// Remove one ID and add another, then hot-reload
	require.Nil(t, os.WriteFile(path, []byte("Z9y8X7w6V5u4\nqR7sT8uV9wX0\n"), 0600))
	require.Nil(t, r.Reload())
	require.False(t, r.Contains("abcdefghijkl"))
	require.True(t, r.Contains("Z9y8X7w6V5u4"))
	require.True(t, r.Contains("qR7sT8uV9wX0"))
}

func TestRetention_ReloadKeepsOldSetOnError(t *testing.T) {
	path := writeRetentionFile(t, "abcdefghijkl\n")
	r, err := NewRetention(path)
	require.Nil(t, err)

	// Replace the file with a directory read error: reload must fail and keep the old set
	require.Nil(t, os.Remove(path))
	require.Nil(t, os.Mkdir(path, 0700))
	require.Error(t, r.Reload())
	require.True(t, r.Contains("abcdefghijkl"))
	require.Equal(t, 1, r.Len())
}

func TestRetention_ReloadEmptyFileUnpinsAll(t *testing.T) {
	path := writeRetentionFile(t, "abcdefghijkl\n")
	r, err := NewRetention(path)
	require.Nil(t, err)
	require.Equal(t, 1, r.Len())

	// A valid but empty manifest deliberately unpins everything
	require.Nil(t, os.WriteFile(path, []byte("# nothing pinned anymore\n"), 0600))
	require.Nil(t, r.Reload())
	require.Equal(t, 0, r.Len())
	require.False(t, r.Contains("abcdefghijkl"))
}

func TestParseRetentionID(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{"abcdefghijkl", "abcdefghijkl"},
		{"  abcdefghijkl  ", "abcdefghijkl"}, // a single bounded ID run is extracted
		{"abcdefghijkl.jpg", "abcdefghijkl"},
		{"/file/Z9y8X7w6V5u4.png", "Z9y8X7w6V5u4"},
		{"https://ntfy.example.com/file/a1B2c3D4e5F6?download=1", "a1B2c3D4e5F6"},
		{"see /file/qR7sT8uV9wX0.bin for the file", "qR7sT8uV9wX0"},
		{"nope", ""},
		{"abcdefghikl", ""},   // 11 chars
		{"abcdefghijklm", ""}, // 13 chars: no bounded 12-run
	} {
		require.Equal(t, tc.want, parseRetentionID(tc.line), tc.line)
	}
}
