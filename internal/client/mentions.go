package client

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

// mentionContext validates mentions (phone digits or JIDs) against the group's
// live member list and returns the text with an "@<user>" token for every
// mention plus a ContextInfo carrying MentionedJID. With no mentions it
// returns text unchanged and a nil ContextInfo.
func (c *Client) mentionContext(ctx context.Context, chat types.JID, text string, mentions []string) (string, *waProto.ContextInfo, error) {
	if len(mentions) == 0 {
		return text, nil, nil
	}
	if chat.Server != types.GroupServer {
		return "", nil, fmt.Errorf("mentions are only supported in group chats (…@%s)", types.GroupServer)
	}
	info, err := c.wa.GetGroupInfo(ctx, chat)
	if err != nil {
		return "", nil, fmt.Errorf("get group info: %w", err)
	}
	resolved, err := resolveMentions(info, mentions, func(j types.JID) types.JID { return c.lidForPN(ctx, j) })
	if err != nil {
		return "", nil, err
	}
	mentioned := make([]string, len(resolved))
	for i, m := range resolved {
		mentioned[i] = m.jid.String()
	}
	return withMentionTokens(text, resolved), &waProto.ContextInfo{MentionedJID: mentioned}, nil
}

// mention is a resolved group member: jid is what goes into MentionedJID
// (the LID in LID-addressed groups), aliases are the other user parts the
// caller may have typed after "@" (phone number, LID).
type mention struct {
	jid     types.JID
	aliases []string
}

// resolveMentions maps each mention to the JID the group itself uses for that
// member (the LID in LID-addressed groups), and rejects non-members.
func resolveMentions(info *types.GroupInfo, mentions []string, lidFor func(types.JID) types.JID) ([]mention, error) {
	jids, err := parseParticipantJIDs(mentions)
	if err != nil {
		return nil, fmt.Errorf("mentions: %w", err)
	}
	var out []mention
	index := make(map[types.JID]int, len(jids))
	for i, jid := range jids {
		member := findParticipant(info, jid, lidFor(jid))
		if member == nil {
			return nil, fmt.Errorf("mention %q is not a participant of group %q; nothing was sent", mentions[i], info.Name)
		}
		id := member.JID.ToNonAD()
		k, ok := index[id]
		if !ok {
			k = len(out)
			index[id] = k
			out = append(out, mention{jid: id})
		}
		for _, alias := range []string{jid.User, member.PhoneNumber.User, member.LID.User} {
			if alias != "" && alias != id.User && !slices.Contains(out[k].aliases, alias) {
				out[k].aliases = append(out[k].aliases, alias)
			}
		}
	}
	return out, nil
}

// withMentionTokens makes text carry "@<user>" for each mention, where user is
// the JID user WhatsApp renders (the LID in LID groups). A token the caller
// wrote with another alias (e.g. "@<phone>" in a LID group) is rewritten;
// a mention with no token at all is prepended.
func withMentionTokens(text string, mentions []mention) string {
	var prefix strings.Builder
	for _, m := range mentions {
		want := "@" + m.jid.User
		if containsToken(text, want) {
			continue
		}
		found := false
		for _, alias := range m.aliases {
			if containsToken(text, "@"+alias) {
				text = replaceToken(text, "@"+alias, want)
				found = true
				break
			}
		}
		if !found {
			prefix.WriteString(want + " ")
		}
	}
	return prefix.String() + text
}

// replaceToken replaces every whole-token occurrence of tok (not followed by
// another digit) with repl.
func replaceToken(s, tok, repl string) string {
	var b strings.Builder
	for {
		k := tokenIndex(s, tok)
		if k < 0 {
			return b.String() + s
		}
		b.WriteString(s[:k] + repl)
		s = s[k+len(tok):]
	}
}

// containsToken reports whether tok occurs in s not followed by another digit,
// so "@123" does not count as present inside "@1234".
func containsToken(s, tok string) bool { return tokenIndex(s, tok) >= 0 }

// tokenIndex returns the index of the first occurrence of tok in s that is not
// followed by another digit, or -1.
func tokenIndex(s, tok string) int {
	for i := 0; ; {
		k := strings.Index(s[i:], tok)
		if k < 0 {
			return -1
		}
		end := i + k + len(tok)
		if end == len(s) || s[end] < '0' || s[end] > '9' {
			return i + k
		}
		i = end
	}
}

// mentionText builds a text message that carries mention metadata.
func mentionText(text string, ci *waProto.ContextInfo) *waProto.ExtendedTextMessage {
	return &waProto.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ci}
}
