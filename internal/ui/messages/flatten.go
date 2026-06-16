package messages

import (
	"regexp"
	"strings"
)

// Regexes for the entity forms FlattenMrkdwn handles beyond the shared
// ones in render.go. They live here (not render.go's var block) because
// the styled renderer never needed them: Slack normally emits bare
// <@UID> mentions, but search.messages snippets can carry the labeled
// form, and <!here>-style broadcasts appear in snippets as raw tokens.
var (
	userMentionWithLabelRe = regexp.MustCompile(`<@([A-Z0-9]+)\|([^>]+)>`)
	specialMentionRe       = regexp.MustCompile(`<!(here|channel|everyone)(?:\|[^>]*)?>`)
	// Usergroup mentions: <!subteam^SID|@label> or bare <!subteam^SID>.
	// Group 1 is the subteam ID; group 2 is the optional label
	// (conventionally "@"-prefixed on the wire, but not guaranteed).
	usergroupMentionRe = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)(?:\|([^>]+))?>`)
)

// FlattenMrkdwn converts Slack mrkdwn entity tokens into plain text for
// single-style contexts (e.g. search-result snippets) where the styled
// renderer in render.go would be overkill:
//
//	<@U1|label>      -> @label
//	<@U1>            -> @name via resolveUser, or @U1 when unknown
//	<#C1|name>       -> #name
//	<#C1>            -> #name via resolveChannel, or #C1 when unknown
//	<url|label>      -> label
//	<url>            -> url (mailto: scheme dropped from display)
//	<!here> etc.     -> @here / @channel / @everyone
//	<!subteam^S1|@eng> -> @eng (unlabeled form -> @group)
//	<!date^..|fall>  -> fall
//	&amp; &lt; &gt;  -> & < >
//
// Either resolver may be nil; unknown IDs fall back to the raw ID.
func FlattenMrkdwn(text string, resolveUser, resolveChannel func(id string) (string, bool)) string {
	// Date tokens first: their payload never contains other entity
	// forms, and handling them up front keeps specialMentionRe from
	// having to exclude `<!date...>`.
	text = dateTokenRe.ReplaceAllStringFunc(text, func(match string) string {
		return dateTokenRe.FindStringSubmatch(match)[1]
	})

	text = linkWithLabelRe.ReplaceAllStringFunc(text, func(match string) string {
		return linkWithLabelRe.FindStringSubmatch(match)[2]
	})
	text = linkBareRe.ReplaceAllStringFunc(text, func(match string) string {
		url := linkBareRe.FindStringSubmatch(match)[1]
		return strings.TrimPrefix(url, "mailto:")
	})

	// Labeled user mentions before bare ones: userMentionRe's
	// ([A-Z0-9]+) would otherwise stop at the `|` and not match.
	text = userMentionWithLabelRe.ReplaceAllStringFunc(text, func(match string) string {
		return "@" + userMentionWithLabelRe.FindStringSubmatch(match)[2]
	})
	text = userMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		id := userMentionRe.FindStringSubmatch(match)[1]
		if resolveUser != nil {
			if name, ok := resolveUser(id); ok && name != "" {
				return "@" + name
			}
		}
		return "@" + id
	})

	text = channelMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		groups := channelMentionRe.FindStringSubmatch(match)
		id, name := groups[1], groups[2]
		if name == "" && resolveChannel != nil {
			if resolved, ok := resolveChannel(id); ok && resolved != "" {
				name = resolved
			}
		}
		if name == "" {
			name = id
		}
		return "#" + name
	})

	text = specialMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		return "@" + specialMentionRe.FindStringSubmatch(match)[1]
	})

	text = usergroupMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		label := usergroupMentionRe.FindStringSubmatch(match)[2]
		if label == "" {
			// Bare token: there's no local usergroup cache to resolve
			// SIDs against, so a generic placeholder beats leaking the
			// raw <!subteam^S...> wire form into a snippet.
			return "@group"
		}
		return "@" + strings.TrimPrefix(label, "@")
	})

	// Wire-format escapes, decoded last so they can't be reinterpreted
	// as entity delimiters (same ordering rule as render.go).
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	return text
}
