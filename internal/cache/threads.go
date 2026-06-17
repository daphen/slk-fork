package cache

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// ThreadSummary is one row in the Threads view: a thread the user is
// involved in (authored, replied to, or @-mentioned in). Computed from
// the local cache; v1 has no Slack-side authoritative data.
type ThreadSummary struct {
	ChannelID    string
	ChannelName  string
	ChannelType  string // "channel" | "private" | "dm" | "group_dm"
	ThreadTS     string
	ParentUserID string
	ParentText   string
	ParentTS     string
	ReplyCount   int // number of replies (does not count the parent)
	LastReplyTS  string
	LastReplyBy  string
	Unread       bool
}

// ListSubscribedThreads returns the workspace's subscribed-threads
// list — the authoritative set from thread_subscriptions joined
// against cached message/channel data for display. Replaces the v1
// "involved threads" heuristic with Slack-side subscription state.
//
// Threads with no cached messages still appear; their parent
// text/user fall back to "" and LastReplyTS falls back to the
// subscription's LastRead so sort still produces a sensible order.
//
// Ordering: newest LastReplyTS first.
//
// Unread is computed from the subscription's per-thread LastRead
// (not the per-channel channels.last_read_ts): unread iff a reply
// exists later than LastRead AND the last reply isn't by self.
func (db *DB) ListSubscribedThreads(workspaceID, selfUserID string) ([]ThreadSummary, error) {
	const q = `
SELECT
    s.channel_id,
    s.thread_ts,
    COALESCE(c.name, ''),
    COALESCE(c.type, ''),
    s.last_read,
    COALESCE((SELECT user_id FROM messages
              WHERE channel_id = s.channel_id AND ts = s.thread_ts AND is_deleted = 0), ''),
    COALESCE((SELECT text FROM messages
              WHERE channel_id = s.channel_id AND ts = s.thread_ts AND is_deleted = 0), ''),
    (SELECT COUNT(*) FROM messages
     WHERE channel_id = s.channel_id AND thread_ts = s.thread_ts
       AND ts != s.thread_ts AND is_deleted = 0)
        AS reply_count,
    COALESCE(
        (SELECT MAX(ts) FROM messages
         WHERE channel_id = s.channel_id AND thread_ts = s.thread_ts AND is_deleted = 0),
        s.last_read
    ) AS last_reply_ts,
    COALESCE(
        (SELECT user_id FROM messages
         WHERE channel_id = s.channel_id AND thread_ts = s.thread_ts AND is_deleted = 0
         ORDER BY ts DESC LIMIT 1),
        ''
    ) AS last_reply_by
FROM thread_subscriptions s
LEFT JOIN channels c ON c.id = s.channel_id
WHERE s.workspace_id = ? AND s.active = 1
`
	rows, err := db.conn.Query(q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("listing subscribed threads: %w", err)
	}
	defer rows.Close()

	var out []ThreadSummary
	for rows.Next() {
		var s ThreadSummary
		var lastRead string
		if err := rows.Scan(
			&s.ChannelID,
			&s.ThreadTS,
			&s.ChannelName,
			&s.ChannelType,
			&lastRead,
			&s.ParentUserID,
			&s.ParentText,
			&s.ReplyCount,
			&s.LastReplyTS,
			&s.LastReplyBy,
		); err != nil {
			return nil, fmt.Errorf("scanning subscribed thread row: %w", err)
		}
		s.ParentTS = s.ThreadTS
		s.Unread = s.LastReplyTS > lastRead && s.LastReplyBy != selfUserID && s.LastReplyBy != ""
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastReplyTS > out[j].LastReplyTS
	})
	return out, nil
}

// ThreadInvolvesUser reports whether the given thread (identified by
// workspaceID, channelID, threadTS) has any cached message authored
// by selfUserID or containing the angle-bracketed mention "<@selfUserID>".
// Used by the reconnect backfill to filter which threads warrant a
// conversations.replies catch-up call.
func (db *DB) ThreadInvolvesUser(workspaceID, channelID, threadTS, selfUserID string, subteamIDs []string) (bool, error) {
	// Involvement = the user posted, was @mentioned directly, or one of the
	// subteams they belong to was mentioned (e.g. an on-call group that
	// pulled them into the thread). subteamIDs may be empty.
	conds := []string{"user_id = ?", "text LIKE ?"}
	args := []any{workspaceID, channelID, threadTS, selfUserID, "%<@" + selfUserID + ">%"}
	for _, sid := range subteamIDs {
		conds = append(conds, "text LIKE ?")
		args = append(args, "%<!subteam^"+sid+">%")
	}
	q := "SELECT 1 FROM messages\n" +
		"WHERE workspace_id = ? AND channel_id = ? AND thread_ts = ?\n" +
		"  AND is_deleted = 0\n" +
		"  AND (" + strings.Join(conds, " OR ") + ")\nLIMIT 1"
	var one int
	err := db.conn.QueryRow(q, args...).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking thread involvement: %w", err)
	}
	return true, nil
}
