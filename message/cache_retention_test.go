package message_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"heckel.io/ntfy/v2/message"
	"heckel.io/ntfy/v2/model"
)

// newAttachmentMessage builds a published message carrying an attachment with explicit
// message and attachment expiry timestamps.
func newAttachmentMessage(id, topic, text string, messageExpires, attachmentExpires, size int64) *model.Message {
	m := model.NewDefaultMessage(topic, text)
	m.ID = id
	m.SequenceID = id
	m.Expires = messageExpires
	m.Attachment = &model.Attachment{
		Name:    id + ".jpg",
		Type:    "image/jpeg",
		Size:    size,
		Expires: attachmentExpires,
		URL:     "https://ntfy.sh/file/" + id + ".jpg",
	}
	return m
}

// TestStore_PinnedAttachmentsRetained verifies end to end that pinned rows are excluded from
// both attachment expiry and message pruning, still count as live attachments, and revert to
// normal expiry once unpinned.
func TestStore_PinnedAttachmentsRetained(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *message.Cache) {
		now := time.Now().Unix()

		pinned := newAttachmentMessage("pa0000000001", "mytopic", "pinned, expired", now-5, now-3600, 5000)
		normal := newAttachmentMessage("pa0000000002", "mytopic", "not pinned, expired", now-4, now-3600, 7000)
		future := newAttachmentMessage("pa0000000003", "mytopic", "not pinned, fresh", now+3600, now+3600, 9000)
		require.Nil(t, s.AddMessage(pinned))
		require.Nil(t, s.AddMessage(normal))
		require.Nil(t, s.AddMessage(future))

		s.SetPinnedIDs(func() ([]string, error) {
			return []string{"pa0000000001"}, nil
		})

		// Only the unpinned expired attachment (m2) is marked deleted; the pinned one survives.
		marked, err := s.MarkExpiredAttachmentsDeleted(10)
		require.Nil(t, err)
		require.Equal(t, int64(1), marked)

		// Live attachment set keeps the pinned-but-expired attachment and the fresh one.
		sizes, err := s.AttachmentsWithSizes()
		require.Nil(t, err)
		require.Equal(t, map[string]int64{
			"pa0000000001": 5000, // pinned, included despite expiry
			"pa0000000003": 9000, // still fresh
		}, sizes)

		// Message pruning removes only the unpinned expired row; the pinned row stays so its
		// download link keeps working.
		deleted, err := s.DeleteExpiredMessages(10)
		require.Nil(t, err)
		require.Equal(t, int64(1), deleted)

		count, err := s.MessagesCount()
		require.Nil(t, err)
		require.Equal(t, 2, count)

		_, err = s.Message("pa0000000001")
		require.Nil(t, err)
		_, err = s.Message("pa0000000002")
		require.Equal(t, model.ErrMessageNotFound, err)

		// Unpin: the previously pinned attachment/message now expire normally.
		s.SetPinnedIDs(nil)

		marked, err = s.MarkExpiredAttachmentsDeleted(10)
		require.Nil(t, err)
		require.Equal(t, int64(1), marked)

		sizes, err = s.AttachmentsWithSizes()
		require.Nil(t, err)
		require.Equal(t, map[string]int64{"pa0000000003": 9000}, sizes)

		deleted, err = s.DeleteExpiredMessages(10)
		require.Nil(t, err)
		require.Equal(t, int64(1), deleted)

		count, err = s.MessagesCount()
		require.Nil(t, err)
		require.Equal(t, 1, count)
		_, err = s.Message("pa0000000001")
		require.Equal(t, model.ErrMessageNotFound, err)
	})
}

// TestStore_NoPinnedIDsUsesDefaultBehavior ensures the cache behaves exactly as before when no
// provider is installed (expired attachments are marked and rows deleted).
func TestStore_NoPinnedIDsUsesDefaultBehavior(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *message.Cache) {
		now := time.Now().Unix()
		require.Nil(t, s.AddMessage(newAttachmentMessage("pb0000000001", "mytopic", "expired", now-5, now-3600, 5000)))

		marked, err := s.MarkExpiredAttachmentsDeleted(10)
		require.Nil(t, err)
		require.Equal(t, int64(1), marked)

		sizes, err := s.AttachmentsWithSizes()
		require.Nil(t, err)
		require.Empty(t, sizes)

		deleted, err := s.DeleteExpiredMessages(10)
		require.Nil(t, err)
		require.Equal(t, int64(1), deleted)
	})
}
