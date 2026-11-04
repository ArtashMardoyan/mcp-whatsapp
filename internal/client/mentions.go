package client

import (
	"context"
	"fmt"
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
	jids, err := resolveMentions(info, mentions, func(j types.JID) types.JID { return c.lidForPN(ctx, j) })
	if err != nil {
		return "", nil, err
	}
	mentioned := make([]string, len(jids))
	for i, j := range jids {
		mentioned[i] = j.String()
	}
	return withMentionTokens(text, jids), &waProto.ContextInfo{MentionedJID: mentioned}, nil
}

// resolveMentions maps each mention to the JID the group itself uses for that
// member (the LID in LID-addressed groups), and rejects non-members.
func resolveMentions(info *types.GroupInfo, mentions []string, lidFor func(types.JID) types.JID) ([]types.JID, error) {
	jids, err := parseParticipantJIDs(mentions)
	if err != nil {
		return nil, fmt.Errorf("mentions: %w", err)
	}
	out := make([]types.JID, 0, len(jids))
	seen := make(map[types.JID]bool, len(jids))
	for i, jid := range jids {
		member := findParticipant(info, jid, lidFor(jid))
		if member == nil {
			return nil, fmt.Errorf("mention %q is not a participant of group %q; nothing was sent", mentions[i], info.Name)
		}
		id := member.JID.ToNonAD()
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// withMentionTokens prepends "@<user> " for each JID whose token text lacks.
// WhatsApp renders a mention only where the text contains "@<user>".
func withMentionTokens(text string, jids []types.JID) string {
	var prefix strings.Builder
	for _, j := range jids {
		if !containsToken(text, "@"+j.User) {
			prefix.WriteString("@" + j.User + " ")
		}
	}
	return prefix.String() + text
}

// containsToken reports whether tok occurs in s not followed by another digit,
// so "@123" does not count as present inside "@1234".
func containsToken(s, tok string) bool {
	for i := 0; ; {
		k := strings.Index(s[i:], tok)
		if k < 0 {
			return false
		}
		end := i + k + len(tok)
		if end == len(s) || s[end] < '0' || s[end] > '9' {
			return true
		}
		i = end
	}
}

// mentionText builds a text message that carries mention metadata.
func mentionText(text string, ci *waProto.ContextInfo) *waProto.ExtendedTextMessage {
	return &waProto.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ci}
}
