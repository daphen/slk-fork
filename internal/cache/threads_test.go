package cache

import (
	"testing"
)

func TestThreadInvolvesUser_AuthoredParent(t *testing.T) {
	db := setupDBWithWorkspace(t)
	defer db.Close()
	db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel"})
	db.UpsertMessage(Message{TS: "1.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "USELF", Text: "parent", ThreadTS: "1.000000"})

	involved, err := db.ThreadInvolvesUser("T1", "C1", "1.000000", "USELF", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !involved {
		t.Error("self-authored parent should count as involved")
	}
}

func TestThreadInvolvesUser_RepliedToThread(t *testing.T) {
	db := setupDBWithWorkspace(t)
	defer db.Close()
	db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel"})
	db.UpsertMessage(Message{TS: "1.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "U2", Text: "parent", ThreadTS: "1.000000"})
	db.UpsertMessage(Message{TS: "2.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "USELF", Text: "my reply", ThreadTS: "1.000000"})

	involved, err := db.ThreadInvolvesUser("T1", "C1", "1.000000", "USELF", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !involved {
		t.Error("self reply should count as involved")
	}
}

func TestThreadInvolvesUser_MentionedAngleBracket(t *testing.T) {
	db := setupDBWithWorkspace(t)
	defer db.Close()
	db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel"})
	db.UpsertMessage(Message{TS: "1.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "U2", Text: "hey <@USELF> ping", ThreadTS: "1.000000"})

	involved, err := db.ThreadInvolvesUser("T1", "C1", "1.000000", "USELF", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !involved {
		t.Error("<@USELF> mention should count as involved")
	}
}

func TestThreadInvolvesUser_PlainTextNotInvolved(t *testing.T) {
	// Bare "USELF" without <@…> wrapping must NOT count as a mention.
	db := setupDBWithWorkspace(t)
	defer db.Close()
	db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel"})
	db.UpsertMessage(Message{TS: "1.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "U2", Text: "discussing USELF in plain text", ThreadTS: "1.000000"})

	involved, err := db.ThreadInvolvesUser("T1", "C1", "1.000000", "USELF", nil)
	if err != nil {
		t.Fatal(err)
	}
	if involved {
		t.Error("plain-text USELF should not count as involved")
	}
}

func TestThreadInvolvesUser_NoneMatch(t *testing.T) {
	db := setupDBWithWorkspace(t)
	defer db.Close()
	db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel"})
	db.UpsertMessage(Message{TS: "1.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "U2", Text: "parent", ThreadTS: "1.000000"})
	db.UpsertMessage(Message{TS: "2.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "U3", Text: "reply", ThreadTS: "1.000000"})

	involved, err := db.ThreadInvolvesUser("T1", "C1", "1.000000", "USELF", nil)
	if err != nil {
		t.Fatal(err)
	}
	if involved {
		t.Error("no self / no mention thread should not count")
	}
}

func TestThreadInvolvesUser_RespectsDeleted(t *testing.T) {
	// A deleted message should not count as involvement; the query
	// filters with is_deleted = 0.
	db := setupDBWithWorkspace(t)
	defer db.Close()
	db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel"})
	db.UpsertMessage(Message{TS: "1.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "U2", Text: "parent", ThreadTS: "1.000000"})
	db.UpsertMessage(Message{TS: "2.000000", ChannelID: "C1", WorkspaceID: "T1", UserID: "USELF", Text: "my reply", ThreadTS: "1.000000"})
	if err := db.DeleteMessage("C1", "2.000000"); err != nil {
		t.Fatal(err)
	}

	involved, err := db.ThreadInvolvesUser("T1", "C1", "1.000000", "USELF", nil)
	if err != nil {
		t.Fatal(err)
	}
	if involved {
		t.Error("deleted self reply should not count as involved")
	}
}

// --- ListSubscribedThreads tests ---

// seedSubscribedThreadFixtures wires up two subscribed threads: A in
// channel C1 (unread — last_reply > LastRead, last reply by other),
// and B in channel C2 (read — last_reply == LastRead).
// Plus one unsubscribed-but-still-cached thread D in C1 that must
// NOT appear in the result.
func seedSubscribedThreadFixtures(t *testing.T, db *DB, selfID string) {
	t.Helper()
	if err := db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel", IsMember: true}); err != nil {
		t.Fatalf("UpsertChannel C1: %v", err)
	}
	if err := db.UpsertChannel(Channel{ID: "C2", WorkspaceID: "T1", Name: "design", Type: "channel", IsMember: true}); err != nil {
		t.Fatalf("UpsertChannel C2: %v", err)
	}

	// Thread A in C1: parent by another user, one reply by another.
	// Subscribed; unread because last reply > LastRead and last reply by other.
	mustUpsertMsg(t, db, "1700000100.000000", "C1", "U2", "parent A", "1700000100.000000")
	mustUpsertMsg(t, db, "1700000200.000000", "C1", "U3", "reply A1", "1700000100.000000")
	if err := db.UpsertThreadSubscription("T1", "C1", "1700000100.000000", "1700000150.000000", true); err != nil {
		t.Fatalf("UpsertThreadSubscription A: %v", err)
	}

	// Thread B in C2: parent by self, one reply by other.
	// Subscribed; read because LastRead == last reply.
	mustUpsertMsg(t, db, "1700000300.000000", "C2", selfID, "parent B", "1700000300.000000")
	mustUpsertMsg(t, db, "1700000400.000000", "C2", "U2", "reply B1", "1700000300.000000")
	if err := db.UpsertThreadSubscription("T1", "C2", "1700000300.000000", "1700000400.000000", true); err != nil {
		t.Fatalf("UpsertThreadSubscription B: %v", err)
	}

	// Thread D in C1: parent + reply cached, but UNSUBSCRIBED.
	mustUpsertMsg(t, db, "1700000500.000000", "C1", "U2", "parent D", "1700000500.000000")
	mustUpsertMsg(t, db, "1700000600.000000", "C1", "U3", "reply D1", "1700000500.000000")
	if err := db.UpsertThreadSubscription("T1", "C1", "1700000500.000000", "1700000500.000000", false); err != nil {
		t.Fatalf("UpsertThreadSubscription D (tombstone): %v", err)
	}
}

func mustUpsertMsg(t *testing.T, db *DB, ts, channelID, userID, text, threadTS string) {
	t.Helper()
	if err := db.UpsertMessage(Message{
		TS: ts, ChannelID: channelID, WorkspaceID: "T1", UserID: userID, Text: text, ThreadTS: threadTS,
	}); err != nil {
		t.Fatalf("UpsertMessage %s: %v", ts, err)
	}
}

func TestListSubscribedThreads_OnlySubscribedShows(t *testing.T) {
	const selfID = "U1"
	db := setupDBWithWorkspace(t)
	seedSubscribedThreadFixtures(t, db, selfID)

	got, err := db.ListSubscribedThreads("T1", selfID)
	if err != nil {
		t.Fatalf("ListSubscribedThreads: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 subscribed threads (A and B), got %d: %+v", len(got), got)
	}
	keys := map[string]bool{}
	for _, s := range got {
		keys[s.ChannelID+":"+s.ThreadTS] = true
	}
	if !keys["C1:1700000100.000000"] || !keys["C2:1700000300.000000"] {
		t.Fatalf("missing expected threads, got keys: %v", keys)
	}
	if keys["C1:1700000500.000000"] {
		t.Fatalf("unsubscribed thread D leaked into result")
	}
}

func TestListSubscribedThreads_SortByLastReplyTSDesc(t *testing.T) {
	const selfID = "U1"
	db := setupDBWithWorkspace(t)
	seedSubscribedThreadFixtures(t, db, selfID)

	got, err := db.ListSubscribedThreads("T1", selfID)
	if err != nil {
		t.Fatalf("ListSubscribedThreads: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("want >=2, got %d", len(got))
	}
	// B has last_reply 1700000400 > A's 1700000200, so B sorts first.
	if got[0].ChannelID != "C2" {
		t.Fatalf("expected B (C2) first, got %s", got[0].ChannelID)
	}
}

func TestListSubscribedThreads_UnreadUsesPerThreadLastRead(t *testing.T) {
	const selfID = "U1"
	db := setupDBWithWorkspace(t)
	// The per-thread LastRead from thread_subscriptions is the sole
	// source of truth for thread unread state; the legacy channel-level
	// last_read_ts heuristic was removed.
	if err := db.UpsertChannel(Channel{
		ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel", IsMember: true,
	}); err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	mustUpsertMsg(t, db, "1700000100.000000", "C1", "U2", "parent", "1700000100.000000")
	mustUpsertMsg(t, db, "1700000200.000000", "C1", "U3", "reply", "1700000100.000000")
	if err := db.UpsertThreadSubscription("T1", "C1", "1700000100.000000", "1700000150.000000", true); err != nil {
		t.Fatalf("UpsertThreadSubscription: %v", err)
	}

	got, err := db.ListSubscribedThreads("T1", selfID)
	if err != nil {
		t.Fatalf("ListSubscribedThreads: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if !got[0].Unread {
		t.Fatalf("expected Unread=true (per-thread LastRead=...150 < LastReplyTS=...200), got Unread=false")
	}
}

func TestListSubscribedThreads_ParentMissingShowsEmpty(t *testing.T) {
	const selfID = "U1"
	db := setupDBWithWorkspace(t)
	// Subscription exists, but neither parent nor replies are cached.
	if err := db.UpsertChannel(Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Type: "channel"}); err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	if err := db.UpsertThreadSubscription("T1", "C1", "1700000100.000000", "1700000150.000000", true); err != nil {
		t.Fatalf("UpsertThreadSubscription: %v", err)
	}

	got, err := db.ListSubscribedThreads("T1", selfID)
	if err != nil {
		t.Fatalf("ListSubscribedThreads: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].ParentText != "" || got[0].ParentUserID != "" {
		t.Fatalf("expected empty parent fields for uncached thread, got %+v", got[0])
	}
	// LastReplyTS falls back to the subscription's LastRead when no
	// messages are cached for the thread.
	if got[0].LastReplyTS != "1700000150.000000" {
		t.Fatalf("expected LastReplyTS to fall back to subscription LastRead, got %q", got[0].LastReplyTS)
	}
}

func TestListSubscribedThreads_PerWorkspaceIsolation(t *testing.T) {
	const selfID = "U1"
	db := setupDBWithWorkspace(t)
	if err := db.UpsertWorkspace(Workspace{ID: "T2", Name: "T2"}); err != nil {
		t.Fatalf("UpsertWorkspace T2: %v", err)
	}
	if err := db.UpsertChannel(Channel{ID: "C9", WorkspaceID: "T2", Name: "other"}); err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	if err := db.UpsertThreadSubscription("T2", "C9", "1700000100.000000", "1700000150.000000", true); err != nil {
		t.Fatalf("UpsertThreadSubscription T2: %v", err)
	}

	got, err := db.ListSubscribedThreads("T1", selfID)
	if err != nil {
		t.Fatalf("ListSubscribedThreads: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("T1 should have 0 subscribed threads, got %d", len(got))
	}
}
