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

// Notifier sends OS-level desktop notifications.
type Notifier struct {
	enabled bool
	conn    *dbus.Conn

	mu     sync.Mutex
	lastID map[string]uint32 // group key (channel) -> last notification id
}

// New creates a Notifier. If enabled is false, Notify is a no-op.
func New(enabled bool) *Notifier {
	n := &Notifier{enabled: enabled, lastID: map[string]uint32{}}
	if enabled {
		// Own session-bus connection so notifications carry AppName "slk".
		// beeep hardcodes "DefaultAppName", which downstream consumers (e.g.
		// a bar's per-app notification badge) can't attribute to slk. Fall
		// back to beeep if the session bus isn't reachable.
		if conn, err := dbus.SessionBus(); err == nil {
			n.conn = conn
		}
	}
	return n
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
		id, err := enotify.SendNotification(n.conn, enotify.Notification{
			AppName:       "slk",
			ReplacesID:    replaces,
			Summary:       title,
			Body:          body,
			ExpireTimeout: enotify.ExpireTimeoutSetByNotificationServer,
		})
		if err == nil {
			n.mu.Lock()
			n.lastID[key] = id
			n.mu.Unlock()
		}
		return err
	}
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
