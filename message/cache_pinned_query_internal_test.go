package message

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSQLitePinnedQueries(t *testing.T) {
	now, limit := int64(100), 10
	pinned := []string{"id1", "id2"}

	q, args := sqliteMarkExpiredAttachmentsDeletedPinned(now, limit, pinned)
	require.Equal(t,
		"UPDATE messages SET attachment_deleted = 1 WHERE mid IN (SELECT mid FROM messages WHERE attachment_expires > 0 AND attachment_expires <= ? AND attachment_deleted = 0 AND mid NOT IN (?,?) LIMIT ?)",
		strings.TrimSpace(q))
	require.Equal(t, []any{now, "id1", "id2", limit}, args)

	q, args = sqliteDeleteExpiredMessagesPinned(now, limit, pinned)
	require.Equal(t,
		"DELETE FROM messages WHERE mid IN (SELECT mid FROM messages WHERE expires <= ? AND published = 1 AND mid NOT IN (?,?) LIMIT ?)",
		strings.TrimSpace(q))
	require.Equal(t, []any{now, "id1", "id2", limit}, args)

	q, args = sqliteSelectAttachmentsWithSizesPinned(now, pinned)
	require.Equal(t,
		"SELECT mid, attachment_size FROM messages WHERE (attachment_expires > ? AND attachment_deleted = 0) OR mid IN (?,?)",
		strings.TrimSpace(q))
	require.Equal(t, []any{now, "id1", "id2"}, args)
}

func TestPostgresPinnedQueries(t *testing.T) {
	now, limit := int64(100), 10
	pinned := []string{"id1", "id2"}

	q, args := postgresMarkExpiredAttachmentsDeletedPinned(now, limit, pinned)
	require.Equal(t,
		"UPDATE message SET attachment_deleted = TRUE WHERE mid IN (SELECT mid FROM message WHERE attachment_expires > 0 AND attachment_expires <= $1 AND attachment_deleted = FALSE AND mid NOT IN ($2,$3) LIMIT $4)",
		strings.TrimSpace(q))
	require.Equal(t, []any{now, "id1", "id2", limit}, args)

	q, args = postgresDeleteExpiredMessagesPinned(now, limit, pinned)
	require.Equal(t,
		"DELETE FROM message WHERE mid IN (SELECT mid FROM message WHERE expires <= $1 AND published = TRUE AND mid NOT IN ($2,$3) LIMIT $4)",
		strings.TrimSpace(q))
	require.Equal(t, []any{now, "id1", "id2", limit}, args)

	q, args = postgresSelectAttachmentsWithSizesPinned(now, pinned)
	require.Equal(t,
		"SELECT mid, attachment_size FROM message WHERE (attachment_expires > $1 AND attachment_deleted = FALSE) OR mid IN ($2,$3)",
		strings.TrimSpace(q))
	require.Equal(t, []any{now, "id1", "id2"}, args)
}

func TestPinnedQueriesSingleAndManyPlaceholders(t *testing.T) {
	require.Equal(t, "?", sqliteQuestionMarks(1))
	require.Equal(t, "?,?,?", sqliteQuestionMarks(3))
	require.Equal(t, "$2", pgPlaceholders(2, 1))
	require.Equal(t, "$2,$3,$4", pgPlaceholders(2, 3))

	// With three pinned IDs the LIMIT placeholder must be $5 (now + 3 + limit).
	q, args := postgresDeleteExpiredMessagesPinned(1, 7, []string{"a", "b", "c"})
	require.True(t, strings.Contains(q, "NOT IN ($2,$3,$4)"), q)
	require.True(t, strings.HasSuffix(q, "LIMIT $5)"), q)
	require.Equal(t, []any{int64(1), "a", "b", "c", 7}, args)
}
