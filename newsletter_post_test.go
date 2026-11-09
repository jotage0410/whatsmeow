// Copyright (c) 2026 Verbeux AI
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var testNewsletterJID = types.NewJID("120363000000000000", types.NewsletterServer)

func testPoll(name string, options ...string) *waE2E.PollCreationMessage {
	poll := &waE2E.PollCreationMessage{Name: proto.String(name), SelectableOptionsCount: proto.Uint32(1)}
	for _, option := range options {
		poll.Options = append(poll.Options, &waE2E.PollCreationMessage_Option{OptionName: proto.String(option)})
	}
	return poll
}

func metaChild(t *testing.T, node waBinary.Node) (waBinary.Node, bool) {
	t.Helper()
	children := node.GetChildren()
	if len(children) == 0 || children[0].Tag != "plaintext" {
		t.Fatalf("expected <plaintext> as the first child, got %s", node.String())
	}
	return node.GetOptionalChildByTag("meta")
}

// The expected nodes match posts made from the official app, read back from a
// real channel: polls and quizzes are type="poll" with a polltype meta, and
// questions are type="text" with a questiontype meta.
func TestBuildNewsletterMessageNodeMarksPollsQuizzesAndQuestions(t *testing.T) {
	quiz := testPoll("Which store has free shipping?", "Store A", "Store B")
	quiz.PollType = waE2E.PollType_QUIZ.Enum()
	quiz.CorrectAnswer = &waE2E.PollCreationMessage_Option{OptionName: proto.String("Store B")}

	tests := []struct {
		name     string
		message  *waE2E.Message
		wantType string
		wantMeta waBinary.Attrs
	}{
		{"v3 poll", &waE2E.Message{PollCreationMessageV3: testPoll("Lunch?", "Yes", "No")}, "poll",
			waBinary.Attrs{"contenttype": "text", "polltype": "creation"}},
		{"v1 poll", &waE2E.Message{PollCreationMessage: testPoll("Lunch?", "Yes", "No")}, "poll",
			waBinary.Attrs{"contenttype": "text", "polltype": "creation"}},
		{"v5 quiz", &waE2E.Message{PollCreationMessageV5: quiz}, "poll",
			waBinary.Attrs{"contenttype": "text", "polltype": "quiz_creation"}},
		{"question", &waE2E.Message{QuestionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("What next?")},
		}}}, "text", waBinary.Attrs{"questiontype": "question"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, err := buildNewsletterMessageNode(testNewsletterJID, "ID", test.message, "")
			if err != nil {
				t.Fatalf("buildNewsletterMessageNode returned an error: %v", err)
			}
			if node.Attrs["type"] != test.wantType {
				t.Fatalf("expected type %q, got %v", test.wantType, node.Attrs["type"])
			}
			meta, ok := metaChild(t, node)
			if !ok {
				t.Fatalf("expected a <meta> child, got %s", node.String())
			}
			if len(meta.Attrs) != len(test.wantMeta) {
				t.Fatalf("expected meta %v, got %v", test.wantMeta, meta.Attrs)
			}
			for key, value := range test.wantMeta {
				if meta.Attrs[key] != value {
					t.Fatalf("expected meta %v, got %v", test.wantMeta, meta.Attrs)
				}
			}
		})
	}
}

func TestBuildNewsletterMessageNodeLeavesOtherPostsUnchanged(t *testing.T) {
	text := "hello"
	tests := []struct {
		name     string
		message  *waE2E.Message
		mediaID  string
		wantType string
	}{
		{"text", &waE2E.Message{Conversation: &text}, "", "text"},
		{"image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")}}, "handle", "media"},
		{"question edit", &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
				EditedMessage: &waE2E.Message{QuestionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
					ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("Edited?")},
				}}},
			},
		}}}, "", "text"},
		{"revoke", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}}, "", "text"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, err := buildNewsletterMessageNode(testNewsletterJID, "ID", test.message, test.mediaID)
			if err != nil {
				t.Fatalf("buildNewsletterMessageNode returned an error: %v", err)
			}
			if node.Attrs["type"] != test.wantType {
				t.Fatalf("expected type %q, got %v", test.wantType, node.Attrs["type"])
			}
			if _, ok := metaChild(t, node); ok {
				t.Fatalf("expected no <meta> child, got %s", node.String())
			}
			if test.mediaID != "" && node.Attrs["media_id"] != test.mediaID {
				t.Fatalf("expected media_id %q, got %v", test.mediaID, node.Attrs["media_id"])
			}
		})
	}
}

// The node mirrors a live update captured from a real channel after a
// follower reacted, voted on a poll and a quiz, and answered a question.
func TestParseNewsletterLiveUpdateCounts(t *testing.T) {
	hash := func(option string) []byte {
		sum := sha256.Sum256([]byte(option))
		return sum[:]
	}
	message := func(serverID string, children ...waBinary.Node) waBinary.Node {
		return waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{"server_id": serverID}, Content: children}
	}
	count := func(tag, value string, children ...waBinary.Node) waBinary.Node {
		node := waBinary.Node{Tag: tag, Attrs: waBinary.Attrs{"count": value}}
		if len(children) > 0 {
			node.Attrs = nil
			node.Content = children
		}
		return node
	}
	vote := func(option, value string) waBinary.Node {
		return waBinary.Node{Tag: "vote", Attrs: waBinary.Attrs{"count": value}, Content: hash(option)}
	}
	messages := waBinary.Node{Tag: "messages", Content: []waBinary.Node{
		message("100",
			waBinary.Node{Tag: "views_count", Attrs: waBinary.Attrs{"count": "2", "type": "views"}},
			count("reactions", "", waBinary.Node{Tag: "reaction", Attrs: waBinary.Attrs{"code": "😂", "count": "1"}}),
		),
		message("105", count("votes", "", vote("Um", "2"), vote("Dois", "1"))),
		message("106", count("votes", "", vote("Certo", "1"))),
		message("104", count("responses_count", "4")),
		message("101"),
	}}

	cli := &Client{Log: waLog.Noop}
	parsed := cli.parseNewsletterMessages(&messages)
	if len(parsed) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(parsed))
	}
	byID := make(map[types.MessageServerID]*types.NewsletterMessage)
	for _, msg := range parsed {
		byID[msg.MessageServerID] = msg
	}

	if got := byID[100]; got.ReactionCounts["😂"] != 1 || got.ViewsCount != 2 || got.VoteCounts != nil || got.ResponsesCount != 0 {
		t.Fatalf("unexpected reaction update: %+v", got)
	}
	wantVotes := map[types.MessageServerID]map[string]int{
		105: {hex.EncodeToString(hash("Um")): 2, hex.EncodeToString(hash("Dois")): 1},
		106: {hex.EncodeToString(hash("Certo")): 1},
	}
	for id, want := range wantVotes {
		got := byID[id].VoteCounts
		if len(got) != len(want) {
			t.Fatalf("message %d: expected votes %v, got %v", id, want, got)
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("message %d: expected votes %v, got %v", id, want, got)
			}
		}
	}
	if got := byID[104].ResponsesCount; got != 4 {
		t.Fatalf("expected 4 responses, got %d", got)
	}
	if got := byID[101]; got.VoteCounts != nil || got.ReactionCounts != nil || got.ResponsesCount != 0 {
		t.Fatalf("expected an empty update, got %+v", got)
	}
}
