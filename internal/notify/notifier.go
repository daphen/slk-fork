// Package notify provides desktop notification support.
package notify

import (
	"regexp"
	"strings"
	"sync"

	enotify "github.com/esiqveland/notify"
	"github.com/gen2brain/beeep"
	"github.com/godbus/dbus/v5"
)

// routeKeySep separates the fields of a notification's group key (an ASCII
// unit separator, which can't appear in a Slack ID or ts).
const routeKeySep = "\x1f"

// RouteKey packs a notification's routing target (workspace, channel, thread)
// into its opaque group key. Empty threadTS for channel messages, which keeps
// channel and thread-reply notifications in separate dedup groups.
func RouteKey(teamID, channelID, threadTS string) string {
	return teamID + routeKeySep + channelID + routeKeySep + threadTS
}

// ParseRouteKey splits a RouteKey into its parts. A key with no separators is
// treated as a bare channel ID (teamID/threadTS empty) for forward safety.
func ParseRouteKey(key string) (teamID, channelID, threadTS string) {
	parts := strings.SplitN(key, routeKeySep, 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	default:
		return "", key, ""
	}
}

// Notifier sends OS-level desktop notifications.
type Notifier struct {
	enabled  bool
	conn     *dbus.Conn
	notifier enotify.Notifier // listens for ActionInvoked/Closed; nil ⇒ stateless send

	mu         sync.Mutex
	lastID     map[string]uint32 // group key (channel) -> last notification id
	idToKey    map[uint32]string // notification id -> group key (for ActionInvoked routing)
	onActivate func(key string)  // invoked when a notification's default action fires
}

// New creates a Notifier. If enabled is false, Notify is a no-op.
func New(enabled bool) *Notifier {
	n := &Notifier{enabled: enabled, lastID: map[string]uint32{}, idToKey: map[uint32]string{}}
	if !enabled {
		return n
	}
	// Own session-bus connection so notifications carry AppName "slk".
	// beeep hardcodes "DefaultAppName", which downstream consumers (e.g.
	// a bar's per-app notification badge) can't attribute to slk. Fall
	// back to beeep if the session bus isn't reachable.
	conn, err := dbus.SessionBus()
	if err != nil {
		return n
	}
	n.conn = conn
	// A stateful Notifier subscribes to ActionInvoked/NotificationClosed so
	// activating a notification can route back to its channel. If that
	// subscription fails we keep conn for stateless sends (no action routing).
	if en, err := enotify.New(conn,
		enotify.WithOnAction(n.handleAction),
		enotify.WithOnClosed(n.handleClosed),
	); err == nil {
		n.notifier = en
	}
	return n
}

// SetOnActivate registers the callback fired when a notification's default
// action is invoked. The argument is the group key passed to Notify (the
// channel ID). Called from the D-Bus signal goroutine — the callback must be
// thread-safe (e.g. post onto the program loop).
func (n *Notifier) SetOnActivate(fn func(key string)) {
	n.mu.Lock()
	n.onActivate = fn
	n.mu.Unlock()
}

// handleAction routes an ActionInvoked signal back to the channel it was sent
// for. dbus delivers signals for every app's notifications, so an id not in
// idToKey is one of ours to ignore.
func (n *Notifier) handleAction(sig *enotify.ActionInvokedSignal) {
	if sig == nil || sig.ActionKey != "default" {
		return
	}
	n.mu.Lock()
	key, ok := n.idToKey[sig.ID]
	fn := n.onActivate
	n.mu.Unlock()
	if ok && fn != nil {
		fn(key)
	}
}

// handleClosed prunes the id→key entry for a dismissed/expired notification.
func (n *Notifier) handleClosed(sig *enotify.NotificationClosedSignal) {
	if sig == nil {
		return
	}
	n.mu.Lock()
	delete(n.idToKey, sig.ID)
	n.mu.Unlock()
}

// Close shuts down the D-Bus signal loop. Safe to call when disabled.
func (n *Notifier) Close() error {
	if n.notifier != nil {
		return n.notifier.Close()
	}
	return nil
}

// Notify sends a desktop notification. key groups notifications by
// conversation: a new message for the same key replaces the prior
// notification in place (via ReplacesID) so the tray shows the latest
// message rather than piling up stale ones. Returns nil if disabled.
func (n *Notifier) Notify(key, title, body string) error {
	if !n.enabled {
		return nil
	}
	if n.conn != nil {
		n.mu.Lock()
		replaces := n.lastID[key]
		n.mu.Unlock()
		note := enotify.Notification{
			AppName:       "slk",
			ReplacesID:    replaces,
			Summary:       title,
			Body:          body,
			Actions:       []enotify.Action{enotify.NewDefaultAction("Open")},
			ExpireTimeout: enotify.ExpireTimeoutSetByNotificationServer,
		}
		var id uint32
		var err error
		if n.notifier != nil {
			id, err = n.notifier.SendNotification(note)
		} else {
			id, err = enotify.SendNotification(n.conn, note)
		}
		if err == nil {
			n.mu.Lock()
			if prev, had := n.lastID[key]; had && prev != id {
				delete(n.idToKey, prev) // replaced in place; drop the stale id
			}
			n.lastID[key] = id
			n.idToKey[id] = key
			n.mu.Unlock()
		}
		return err
	}
	// beeep fallback carries no actions — activation routing needs D-Bus.
	return beeep.Notify(title, body, "")
}

// NotifyContext holds the state needed to evaluate notification triggers.
type NotifyContext struct {
	CurrentUserID   string
	ActiveChannelID string
	IsActiveWS      bool
	OnMention       bool
	OnDM            bool
	OnThread        bool
	OnKeyword       []string
	NotifyChannels  []string
	IsDND           bool // when true, ShouldNotify always returns false

	// ChannelName is the human channel name, matched against NotifyChannels.
	ChannelName string
	// ThreadFollowed is true when this message is a reply in a thread the
	// user participates in (authored or was mentioned). Set by the caller.
	ThreadFollowed bool
	// GroupMention is true when the message tags a user-group (subteam) the
	// user belongs to — e.g. an on-call group. Set by the caller.
	GroupMention bool
}

// ShouldNotify returns true if a message should trigger a desktop notification.
func ShouldNotify(ctx NotifyContext, channelID, userID, text, channelType string) bool {
	// Never notify for own messages
	if userID == ctx.CurrentUserID {
		return false
	}

	// Suppress entirely while DND/snoozed.
	if ctx.IsDND {
		return false
	}

	// Suppress if viewing this channel on the active workspace
	if ctx.IsActiveWS && channelID == ctx.ActiveChannelID {
		return false
	}

	// Check DM trigger. "app" covers bot/app DMs (Swarmia, GitHub, …),
	// which are still direct messages the user wants surfaced.
	if ctx.OnDM && (channelType == "dm" || channelType == "group_dm" || channelType == "app") {
		return true
	}

	// Check thread trigger: a reply in a thread the user participates in.
	if ctx.OnThread && ctx.ThreadFollowed {
		return true
	}

	// Check mention trigger — direct (<@me>) or a user-group the user is in.
	if ctx.OnMention && (ctx.GroupMention || strings.Contains(text, "<@"+ctx.CurrentUserID+">")) {
		return true
	}

	// Watched channels: notify on any message in a channel whose name
	// matches one of NotifyChannels (case-insensitive substring).
	if ctx.ChannelName != "" && len(ctx.NotifyChannels) > 0 {
		lname := strings.ToLower(ctx.ChannelName)
		for _, pat := range ctx.NotifyChannels {
			if pat != "" && strings.Contains(lname, strings.ToLower(pat)) {
				return true
			}
		}
	}

	// Check keyword triggers
	if len(ctx.OnKeyword) > 0 {
		lower := strings.ToLower(text)
		for _, kw := range ctx.OnKeyword {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return true
			}
		}
	}

	return false
}

var (
	userMentionRe    = regexp.MustCompile(`<@([A-Z0-9]+)>`)
	channelMentionRe = regexp.MustCompile(`<#[A-Z0-9]+\|([^>]+)>`)
	subteamMentionRe = regexp.MustCompile(`<!subteam\^[A-Z0-9]+\|([^>]+)>`)
	broadcastRe      = regexp.MustCompile(`<!(here|channel|everyone)>`)
	// Match both http(s) URLs and mailto: addresses; Slack
	// auto-linkifies typed emails into <mailto:X|X>. Bare-link
	// substitution keeps the URL as-is for http(s) but strips the
	// mailto: prefix so the notification body reads as just the
	// address — see StripSlackMarkup below.
	linkWithLabelRe = regexp.MustCompile(`<((?:https?://|mailto:)[^|>]+)\|([^>]+)>`)
	linkBareRe      = regexp.MustCompile(`<((?:https?://|mailto:)[^>]+)>`)
)

// StripSlackMarkup converts Slack-formatted text to plain text suitable for
// OS notification bodies. User mentions are resolved against userNames; if
// a user ID is missing from the map (or the map is nil) the raw user ID is
// used as a fallback. Output is truncated to 100 characters with "..." suffix.
func StripSlackMarkup(text string, userNames map[string]string) string {
	text = channelMentionRe.ReplaceAllString(text, "#$1")
	text = linkWithLabelRe.ReplaceAllString(text, "$2")
	// Bare links: drop the mailto: scheme so notification bodies read
	// as just the address; http(s) URLs are kept whole.
	text = linkBareRe.ReplaceAllStringFunc(text, func(match string) string {
		url := linkBareRe.FindStringSubmatch(match)[1]
		return strings.TrimPrefix(url, "mailto:")
	})
	text = subteamMentionRe.ReplaceAllString(text, "$1")
	text = broadcastRe.ReplaceAllString(text, "@$1")
	text = userMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		userID := userMentionRe.FindStringSubmatch(match)[1]
		if name, ok := userNames[userID]; ok {
			return "@" + name
		}
		return "@" + userID
	})
	text = strings.ReplaceAll(text, "*", "")
	text = strings.ReplaceAll(text, "_", "")
	text = strings.ReplaceAll(text, "~", "")
	text = strings.ReplaceAll(text, "`", "")

	if len(text) > 100 {
		text = text[:100] + "..."
	}

	return text
}
