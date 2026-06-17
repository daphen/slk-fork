package cache

import (
	"fmt"
	"time"
)

// ThreadSubscription is one row in the thread_subscriptions table.
// Mirrors Slack's authoritative per-thread subscription state:
// whether the user is "subscribed for unread updates" on this thread,
// and the last-read timestamp inside the thread.
type ThreadSubscription struct {
	WorkspaceID string
	ChannelID   string
	ThreadTS    string
	LastRead    string
	Active      bool
	UpdatedAt   int64 // unix seconds; bumped on every upsert
}

// UpsertThreadSubscription inserts or updates a thread_subscriptions
// row. Bumps updated_at to time.Now().Unix() on every call. Use
// active=false to tombstone a row (the row is kept so its LastRead
// survives later re-subscriptions).
func (db *DB) UpsertThreadSubscription(workspaceID, channelID, threadTS, lastRead string, active bool) error {
	if workspaceID == "" || channelID == "" || threadTS == "" {
		return fmt.Errorf("UpsertThreadSubscription: workspace/channel/thread_ts required")
	}
	activeInt := 0
	if active {
		activeInt = 1
	}
	const q = `
INSERT INTO thread_subscriptions
    (workspace_id, channel_id, thread_ts, last_read, active, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, channel_id, thread_ts) DO UPDATE SET
    last_read  = excluded.last_read,
    active     = excluded.active,
    updated_at = excluded.updated_at
`
	_, err := db.conn.Exec(q, workspaceID, channelID, threadTS, lastRead, activeInt, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("upserting thread_subscriptions: %w", err)
	}
	return nil
}

// AdvanceThreadSubscriptionLastRead moves an existing active subscription's
// last_read forward to ts, never backward, and is a no-op when the row is
// absent. Lets the threads view reflect a just-opened thread as read
// immediately instead of waiting for the server's thread_marked echo.
func (db *DB) AdvanceThreadSubscriptionLastRead(workspaceID, channelID, threadTS, ts string) error {
	if workspaceID == "" || channelID == "" || threadTS == "" || ts == "" {
		return nil
	}
	const q = `
UPDATE thread_subscriptions
SET last_read = ?, updated_at = ?
WHERE workspace_id = ? AND channel_id = ? AND thread_ts = ? AND last_read < ?
`
	if _, err := db.conn.Exec(q, ts, time.Now().Unix(), workspaceID, channelID, threadTS, ts); err != nil {
		return fmt.Errorf("advancing thread_subscriptions last_read: %w", err)
	}
	return nil
}

// DeleteThreadSubscription removes a thread_subscriptions row outright
// (not a tombstone). Used by tests; production callers prefer
// UpsertThreadSubscription with active=false to preserve LastRead.
func (db *DB) DeleteThreadSubscription(workspaceID, channelID, threadTS string) error {
	const q = `DELETE FROM thread_subscriptions WHERE workspace_id=? AND channel_id=? AND thread_ts=?`
	_, err := db.conn.Exec(q, workspaceID, channelID, threadTS)
	if err != nil {
		return fmt.Errorf("deleting thread_subscriptions: %w", err)
	}
	return nil
}

// ListActiveThreadSubscriptions returns every active subscription in
// the given workspace, in PRIMARY KEY order. Tombstoned rows
// (active=0) are filtered out.
func (db *DB) ListActiveThreadSubscriptions(workspaceID string) ([]ThreadSubscription, error) {
	const q = `
SELECT workspace_id, channel_id, thread_ts, last_read, active, updated_at
FROM thread_subscriptions
WHERE workspace_id = ? AND active = 1
ORDER BY channel_id, thread_ts
`
	rows, err := db.conn.Query(q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("listing thread_subscriptions: %w", err)
	}
	defer rows.Close()
	var out []ThreadSubscription
	for rows.Next() {
		var s ThreadSubscription
		var activeInt int
		if err := rows.Scan(&s.WorkspaceID, &s.ChannelID, &s.ThreadTS,
			&s.LastRead, &activeInt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning thread_subscriptions: %w", err)
		}
		s.Active = activeInt == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReconcileThreadSubscriptions replaces the workspace's local
// subscription set with the given fresh list. Upserts every fresh
// entry (active=1) and tombstones (active=0) any local active row
// whose (channel_id, thread_ts) doesn't appear in the fresh list.
//
// Used by the reconnect backfill: after fetching the full server-side
// list, calling this reconciles any subscribes/unsubscribes that
// happened while the WS was disconnected. Tombstoning preserves the
// row's LastRead so a later re-subscribe doesn't lose history.
func (db *DB) ReconcileThreadSubscriptions(workspaceID string, fresh []ThreadSubscription) error {
	if workspaceID == "" {
		return fmt.Errorf("ReconcileThreadSubscriptions: workspaceID required")
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("begin reconcile tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().Unix()

	// Build the set of keys present in the fresh list.
	type key struct{ ch, ts string }
	freshKeys := make(map[key]struct{}, len(fresh))
	for _, s := range fresh {
		freshKeys[key{s.ChannelID, s.ThreadTS}] = struct{}{}
	}

	// 1. Upsert each fresh entry as active=1.
	const upsertQ = `
INSERT INTO thread_subscriptions
    (workspace_id, channel_id, thread_ts, last_read, active, updated_at)
VALUES (?, ?, ?, ?, 1, ?)
ON CONFLICT(workspace_id, channel_id, thread_ts) DO UPDATE SET
    last_read  = excluded.last_read,
    active     = 1,
    updated_at = excluded.updated_at
`
	for _, s := range fresh {
		if _, err := tx.Exec(upsertQ, workspaceID, s.ChannelID, s.ThreadTS, s.LastRead, now); err != nil {
			return fmt.Errorf("upserting fresh subscription (%s/%s): %w", s.ChannelID, s.ThreadTS, err)
		}
	}

	// 2. Find currently-active rows that aren't in the fresh list and
	// tombstone them. Walk the existing active rows once; tombstone in
	// a second pass to avoid mutating during iteration.
	rows, err := tx.Query(
		`SELECT channel_id, thread_ts FROM thread_subscriptions WHERE workspace_id=? AND active=1`,
		workspaceID,
	)
	if err != nil {
		return fmt.Errorf("listing active for reconcile: %w", err)
	}
	var toTombstone []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.ch, &k.ts); err != nil {
			rows.Close()
			return fmt.Errorf("scanning active for reconcile: %w", err)
		}
		if _, ok := freshKeys[k]; !ok {
			toTombstone = append(toTombstone, k)
		}
	}
	rows.Close()

	for _, k := range toTombstone {
		if _, err := tx.Exec(
			`UPDATE thread_subscriptions SET active=0, updated_at=? WHERE workspace_id=? AND channel_id=? AND thread_ts=?`,
			now, workspaceID, k.ch, k.ts,
		); err != nil {
			return fmt.Errorf("tombstoning subscription (%s/%s): %w", k.ch, k.ts, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reconcile tx: %w", err)
	}
	return nil
}
