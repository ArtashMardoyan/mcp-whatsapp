package client

import (
	"context"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func lidGroup() *types.GroupInfo {
	return &types.GroupInfo{
		GroupName:      types.GroupName{Name: "Team"},
		AddressingMode: types.AddressingModeLID,
		Participants: []types.GroupParticipant{
			{
				JID:         types.JID{User: "111", Server: types.HiddenUserServer},
				LID:         types.JID{User: "111", Server: types.HiddenUserServer},
				PhoneNumber: types.JID{User: "447700000001", Server: types.DefaultUserServer},
			},
			// Member whose phone number the group hides.
			{JID: types.JID{User: "222", Server: types.HiddenUserServer}},
		},
	}
}

func TestResolveMentions_UsesGroupLID(t *testing.T) {
	lidFor := func(j types.JID) types.JID {
		if j.User == "447700000002" {
			return types.JID{User: "222", Server: types.HiddenUserServer}
		}
		return types.EmptyJID
	}
	got, err := resolveMentions(lidGroup(), []string{"+447700000001", "447700000002", "111@lid"}, lidFor)
	if err != nil {
		t.Fatalf("resolveMentions: %v", err)
	}
	// "+447700000001" and "111@lid" are the same member and collapse to one.
	if len(got) != 2 || got[0].String() != "111@lid" || got[1].String() != "222@lid" {
		t.Fatalf("got %v, want [111@lid 222@lid]", got)
	}
}

func TestResolveMentions_RejectsNonMember(t *testing.T) {
	_, err := resolveMentions(lidGroup(), []string{"447700000009"}, noLID)
	if err == nil || !strings.Contains(err.Error(), "not a participant") {
		t.Fatalf("want not-a-participant error, got %v", err)
	}
}

func TestResolveMentions_RejectsBadInput(t *testing.T) {
	if _, err := resolveMentions(lidGroup(), []string{"+44 7700"}, noLID); err == nil {
		t.Fatal("want parse error for non-digit mention")
	}
}

func TestWithMentionTokens(t *testing.T) {
	jids := []types.JID{{User: "111", Server: types.HiddenUserServer}, {User: "12", Server: types.DefaultUserServer}}
	cases := map[string]string{
		"hi":               "@111 @12 hi",
		"hi @111 and @12!": "hi @111 and @12!",
		"@123 hi @111":     "@12 @123 hi @111", // "@123" does not satisfy "@12"
	}
	for in, want := range cases {
		if got := withMentionTokens(in, jids); got != want {
			t.Errorf("withMentionTokens(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMentionContext_NoMentionsIsPassthrough(t *testing.T) {
	c := newDisconnectedClient()
	text, ci, err := c.mentionContext(context.Background(), types.JID{User: "1", Server: types.GroupServer}, "hi", nil)
	if err != nil || ci != nil || text != "hi" {
		t.Fatalf("got %q %v %v", text, ci, err)
	}
}

func TestMentionContext_RejectsDirectChat(t *testing.T) {
	c := newDisconnectedClient()
	_, _, err := c.mentionContext(context.Background(), types.JID{User: "1", Server: types.DefaultUserServer}, "hi", []string{"1"})
	if err == nil || !strings.Contains(err.Error(), "group") {
		t.Fatalf("want group-only error, got %v", err)
	}
}
