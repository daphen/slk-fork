// internal/ui/reducer_links.go
//
// Link-open routing (issue #62 + in-app permalink navigation).
//
// OpenLinkMsg is the single place every link open flows through:
//   - Slack archive permalinks whose subdomain matches the active
//     workspace AND whose channel resolves via ChannelService.Lookup
//     navigate in-app: dispatch ChannelSelectedMsg, then complete via
//     pendingLinkNav once the channel's messages are loaded (select
//     the target ts, or open the thread panel for thread_ts links).
//   - Everything else opens in the OS browser (a.browserOpener).
//
// Completion hooks live in reducer_channels.go (ChannelSelectedMsg
// and MessagesLoadedMsg arms call completePendingLinkNav).
package ui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/slackurl"
	"github.com/gammons/slk/internal/ui/messages"
)

// pendingLinkNav is the not-yet-completed tail of an in-app permalink
// navigation. Set by routeLink, consumed by completePendingLinkNav.
type pendingLinkNav struct {
	channelID string
	messageTS string
	threadTS  string // non-empty: open the thread panel instead of selecting
}

var reduceLinks reducerFunc = func(a *App, msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case OpenLinkMsg:
		return a.routeLink(m.URL), true
	case NotificationActivatedMsg:
		return a.activateNotification(m.ChannelID), true
	}
	return nil, false
}

// activateNotification switches the active channel to channelID in response
// to a desktop-notification default action. Resolves name/type via the same
// ChannelService.Lookup that link navigation uses, then dispatches the
// existing ChannelSelectedMsg. No-op when already viewing the channel or when
// it isn't in the active workspace (the WM still raises the window).
func (a *App) activateNotification(channelID string) tea.Cmd {
	if channelID == "" || channelID == a.activeChannelID {
		return nil
	}
	name, chType, ok := a.channels.Lookup(ids.ChannelID(channelID))
	if !ok {
		return nil
	}
	id, n, t := channelID, name, chType
	return func() tea.Msg {
		return ChannelSelectedMsg{ID: id, Name: n, Type: t}
	}
}

// routeLink decides between in-app navigation and the browser.
func (a *App) routeLink(rawURL string) tea.Cmd {
	pl, ok := slackurl.Parse(rawURL)
	if !ok {
		return a.browserOpener(rawURL)
	}
	domain := a.activeWorkspaceDomain()
	if domain == "" || pl.Subdomain != domain {
		return a.browserOpener(rawURL)
	}
	name, chType, found := a.channels.Lookup(pl.ChannelID)
	if !found {
		return a.browserOpener(rawURL)
	}
	a.pendingLinkNav = &pendingLinkNav{
		channelID: string(pl.ChannelID),
		messageTS: string(pl.MessageTS),
		threadTS:  string(pl.ThreadTS),
	}
	if string(pl.ChannelID) == a.activeChannelID {
		// Already viewing the channel; the loaded buffer is as good
		// as it gets, so complete authoritatively right now.
		return a.completePendingLinkNav(a.activeChannelID, true)
	}
	id, n, t := string(pl.ChannelID), name, chType
	return func() tea.Msg {
		return ChannelSelectedMsg{ID: id, Name: n, Type: t}
	}
}

// completePendingLinkNav finishes (or drops) the pending permalink
// navigation for channelID. authoritative=true means "no more message
// data is coming for this channel" — if the target ts still isn't in
// the buffer, dispatch ChannelService.FetchAround to load a history
// window centered on the target instead of waiting.
//
// Called from: routeLink (already-active channel, authoritative),
// reduceChannels' ChannelSelectedMsg arm (cache render, best-effort),
// and reduceChannels' MessagesLoadedMsg arm (authoritative).
func (a *App) completePendingLinkNav(channelID string, authoritative bool) tea.Cmd {
	p := a.pendingLinkNav
	if p == nil {
		return nil
	}
	if p.channelID != channelID {
		// The user navigated somewhere unrelated before the link
		// target finished loading; the pending nav is stale.
		a.pendingLinkNav = nil
		return nil
	}
	if p.threadTS != "" {
		a.pendingLinkNav = nil
		return a.openThreadForPermalink(p.channelID, p.threadTS)
	}
	if a.messagepane.SelectByTS(p.messageTS) {
		a.pendingLinkNav = nil
		return nil
	}
	if authoritative {
		a.pendingLinkNav = nil
		channels := a.channels
		chID, ts := p.channelID, p.messageTS
		return func() tea.Msg {
			return channels.FetchAround(ids.ChannelID(chID), ids.MessageTS(ts))
		}
	}
	return nil
}

// openThreadForPermalink opens the thread panel for a permalink that
// carried thread_ts. Unlike openThreadForSelectedMessage it does not
// require the parent message to be in the pane buffer (mirrors
// openSelectedThreadCmd, which builds the parent from a summary):
// the parent row is taken from the loaded buffer or the thread cache
// when available, else a minimal stub that the ThreadRepliesLoadedMsg
// handler backfills from cache once the fetch lands.
func (a *App) openThreadForPermalink(channelID, threadTS string) tea.Cmd {
	parent := messages.MessageItem{TS: threadTS, ThreadTS: threadTS}
	if channelID == a.activeChannelID {
		for _, m := range a.messagepane.Messages() {
			if m.TS == threadTS {
				parent = m
				break
			}
		}
	}
	if parent.Text == "" {
		if cached := a.threads.CacheRead(ids.ChannelID(channelID), ids.ThreadTS(threadTS)); len(cached) > 0 {
			parent = cached[0]
		}
	}

	return a.openThreadPanel(parent, channelID, threadTS)
}
