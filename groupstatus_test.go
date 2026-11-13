// Copyright (c) 2026 Verbeux AI
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"context"
	"errors"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waStatusAttributions"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

var testStatusGroup = types.NewJID("120363000000000001", types.GroupServer)

func wrapGroupStatus(inner *waE2E.Message) *waE2E.Message {
	return &waE2E.Message{GroupStatusMessageV2: &waE2E.FutureProofMessage{Message: inner}}
}

func TestStanzaTypesUnwrapGroupStatus(t *testing.T) {
	tests := []struct {
		name      string
		msg       *waE2E.Message
		wantType  string
		wantMedia string
	}{
		{
			name:      "image",
			msg:       wrapGroupStatus(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}),
			wantType:  "media",
			wantMedia: "image",
		},
		{
			name:      "video",
			msg:       wrapGroupStatus(&waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}),
			wantType:  "media",
			wantMedia: "video",
		},
		{
			name:      "gif",
			msg:       wrapGroupStatus(&waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: new(true)}}),
			wantType:  "media",
			wantMedia: "gif",
		},
		{
			name:      "voice note",
			msg:       wrapGroupStatus(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: new(true)}}),
			wantType:  "media",
			wantMedia: "ptt",
		},
		{
			name:      "text",
			msg:       wrapGroupStatus(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: new("hi")}}),
			wantType:  "text",
			wantMedia: "",
		},
		{
			name: "legacy wrapper",
			msg: &waE2E.Message{GroupStatusMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}},
			}},
			wantType:  "media",
			wantMedia: "image",
		},
		{
			// A group retry adds the sender key distribution message next to the
			// envelope; it must not hide the wrapped media.
			name: "retry with sender key",
			msg: &waE2E.Message{
				GroupStatusMessageV2:         &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}},
				SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: new(testStatusGroup.String())},
			},
			wantType:  "media",
			wantMedia: "image",
		},
		{
			name:      "empty envelope",
			msg:       &waE2E.Message{GroupStatusMessageV2: &waE2E.FutureProofMessage{}},
			wantType:  "text",
			wantMedia: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := getTypeFromMessage(test.msg); got != test.wantType {
				t.Fatalf("getTypeFromMessage = %q, want %q", got, test.wantType)
			}
			if got := getMediaTypeFromMessage(test.msg); got != test.wantMedia {
				t.Fatalf("getMediaTypeFromMessage = %q, want %q", got, test.wantMedia)
			}
		})
	}
}

func TestBuildGroupStatusMessageImage(t *testing.T) {
	author := types.NewJID("123456789", types.HiddenUserServer)
	inner := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:  new("Oferta https://example.com/p/1"),
		Mimetype: new("image/jpeg"),
	}}

	msg, err := BuildGroupStatusMessage(inner, author)
	if err != nil {
		t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
	}
	if secret := msg.GetMessageContextInfo().GetMessageSecret(); len(secret) != 32 {
		t.Fatalf("outer message secret has %d bytes, want 32", len(secret))
	}
	image := msg.GetGroupStatusMessageV2().GetMessage().GetImageMessage()
	if image == nil {
		t.Fatalf("envelope does not carry the image: %v", msg)
	}
	if image.GetCaption() != "Oferta https://example.com/p/1" || image.GetMimetype() != "image/jpeg" {
		t.Fatalf("image fields were not kept: %v", image)
	}
	info := image.GetContextInfo()
	if !info.GetIsGroupStatus() {
		t.Fatal("inner context info is not flagged as group status")
	}
	if info.StatusSourceType == nil || info.GetStatusSourceType() != waE2E.ContextInfo_IMAGE {
		t.Fatalf("status source type = %v, want IMAGE", info.StatusSourceType)
	}
	attributions := info.GetStatusAttributions()
	if len(attributions) != 1 {
		t.Fatalf("got %d status attributions, want 1", len(attributions))
	}
	if attributions[0].GetType() != waStatusAttributions.StatusAttribution_GROUP_STATUS {
		t.Fatalf("attribution type = %v, want GROUP_STATUS", attributions[0].GetType())
	}
	if got := attributions[0].GetGroupStatus().GetAuthorJID(); got != author.String() {
		t.Fatalf("attribution author = %q, want %q", got, author.String())
	}
	if msg.ImageMessage != nil {
		t.Fatalf("content must only be inside the envelope: %v", msg)
	}
	if inner.ImageMessage.ContextInfo != nil {
		t.Fatal("BuildGroupStatusMessage modified the caller's message")
	}
}

func TestBuildGroupStatusMessageSourceTypes(t *testing.T) {
	tests := []struct {
		name  string
		inner *waE2E.Message
		get   func(*waE2E.Message) *waE2E.ContextInfo
		want  waE2E.ContextInfo_StatusSourceType
	}{
		{
			name:  "video",
			inner: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{}},
			get:   func(m *waE2E.Message) *waE2E.ContextInfo { return m.GetVideoMessage().GetContextInfo() },
			want:  waE2E.ContextInfo_VIDEO,
		},
		{
			name:  "gif",
			inner: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: new(true)}},
			get:   func(m *waE2E.Message) *waE2E.ContextInfo { return m.GetVideoMessage().GetContextInfo() },
			want:  waE2E.ContextInfo_GIF,
		},
		{
			name:  "audio",
			inner: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: new(true)}},
			get:   func(m *waE2E.Message) *waE2E.ContextInfo { return m.GetAudioMessage().GetContextInfo() },
			want:  waE2E.ContextInfo_AUDIO,
		},
		{
			name: "text",
			inner: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:           new("Oferta do dia"),
				BackgroundArgb: new(uint32(0xFF25D366)),
			}},
			get:  func(m *waE2E.Message) *waE2E.ContextInfo { return m.GetExtendedTextMessage().GetContextInfo() },
			want: waE2E.ContextInfo_TEXT,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg, err := BuildGroupStatusMessage(test.inner, types.EmptyJID)
			if err != nil {
				t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
			}
			info := test.get(msg.GetGroupStatusMessageV2().GetMessage())
			if !info.GetIsGroupStatus() {
				t.Fatal("inner context info is not flagged as group status")
			}
			if info.GetStatusSourceType() != test.want {
				t.Fatalf("status source type = %v, want %v", info.GetStatusSourceType(), test.want)
			}
			if len(info.GetStatusAttributions()) != 0 {
				t.Fatalf("attribution added without an author: %v", info.GetStatusAttributions())
			}
		})
	}
}

func TestBuildGroupStatusMessageTurnsConversationIntoExtendedText(t *testing.T) {
	msg, err := BuildGroupStatusMessage(&waE2E.Message{Conversation: new("Bom dia")}, types.EmptyJID)
	if err != nil {
		t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
	}
	inner := msg.GetGroupStatusMessageV2().GetMessage()
	if inner.Conversation != nil {
		t.Fatalf("conversation was kept next to the extended text: %v", inner)
	}
	if inner.GetExtendedTextMessage().GetText() != "Bom dia" {
		t.Fatalf("text = %q, want %q", inner.GetExtendedTextMessage().GetText(), "Bom dia")
	}
	if !inner.GetExtendedTextMessage().GetContextInfo().GetIsGroupStatus() {
		t.Fatal("text status is not flagged as group status")
	}
}

func TestBuildGroupStatusMessageKeepsCallerContext(t *testing.T) {
	preset := &waStatusAttributions.StatusAttribution{Type: waStatusAttributions.StatusAttribution_MUSIC.Enum()}
	inner := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{ContextInfo: &waE2E.ContextInfo{
		MentionedJID:       []string{"5511999999999@s.whatsapp.net"},
		StatusSourceType:   waE2E.ContextInfo_GIF.Enum(),
		StatusAttributions: []*waStatusAttributions.StatusAttribution{preset},
	}}}

	msg, err := BuildGroupStatusMessage(inner, types.NewJID("5511888888888", types.DefaultUserServer))
	if err != nil {
		t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
	}
	info := msg.GetGroupStatusMessageV2().GetMessage().GetImageMessage().GetContextInfo()
	if !info.GetIsGroupStatus() {
		t.Fatal("inner context info is not flagged as group status")
	}
	if len(info.GetMentionedJID()) != 1 {
		t.Fatalf("mentions were dropped: %v", info.GetMentionedJID())
	}
	if info.GetStatusSourceType() != waE2E.ContextInfo_GIF {
		t.Fatalf("caller's status source type was replaced: %v", info.GetStatusSourceType())
	}
	if len(info.GetStatusAttributions()) != 1 || info.GetStatusAttributions()[0].GetType() != waStatusAttributions.StatusAttribution_MUSIC {
		t.Fatalf("caller's attributions were replaced: %v", info.GetStatusAttributions())
	}
	if inner.GetImageMessage().GetContextInfo().IsGroupStatus != nil {
		t.Fatal("BuildGroupStatusMessage modified the caller's context info")
	}
}

func TestBuildGroupStatusMessageUsesFreshSecrets(t *testing.T) {
	inner := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}
	first, err := BuildGroupStatusMessage(inner, types.EmptyJID)
	if err != nil {
		t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
	}
	second, err := BuildGroupStatusMessage(inner, types.EmptyJID)
	if err != nil {
		t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
	}
	if bytes.Equal(first.GetMessageContextInfo().GetMessageSecret(), second.GetMessageContextInfo().GetMessageSecret()) {
		t.Fatal("two envelopes share the same message secret")
	}
}

func TestBuildGroupStatusMessageRejectsUnsupportedContent(t *testing.T) {
	for name, inner := range map[string]*waE2E.Message{
		"nil":      nil,
		"empty":    {},
		"document": {DocumentMessage: &waE2E.DocumentMessage{}},
		"sticker":  {StickerMessage: &waE2E.StickerMessage{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildGroupStatusMessage(inner, types.EmptyJID); !errors.Is(err, ErrGroupStatusUnsupportedContent) {
				t.Fatalf("error = %v, want ErrGroupStatusUnsupportedContent", err)
			}
		})
	}
}

func TestSendGroupStatusValidatesBeforeTouchingTheNetwork(t *testing.T) {
	image := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}
	// The client has no store and no connection: reaching anything past the
	// validation would panic or fail with a different error.
	cli := &Client{}
	for name, jid := range map[string]types.JID{
		"phone number": types.NewJID("5511999999999", types.DefaultUserServer),
		"lid":          types.NewJID("123456789", types.HiddenUserServer),
		"status":       types.StatusBroadcastJID,
		"newsletter":   types.NewJID("120363000000000002", types.NewsletterServer),
		"empty":        types.EmptyJID,
		"server only":  types.NewJID("", types.GroupServer),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cli.SendGroupStatus(context.Background(), jid, image); !errors.Is(err, ErrGroupStatusNotGroup) {
				t.Fatalf("error = %v, want ErrGroupStatusNotGroup", err)
			}
		})
	}
	if _, err := cli.SendGroupStatus(context.Background(), testStatusGroup, nil); !errors.Is(err, ErrGroupStatusUnsupportedContent) {
		t.Fatalf("nil message error = %v, want ErrGroupStatusUnsupportedContent", err)
	}
	var nilClient *Client
	if _, err := nilClient.SendGroupStatus(context.Background(), testStatusGroup, image); !errors.Is(err, ErrClientIsNil) {
		t.Fatalf("nil client error = %v, want ErrClientIsNil", err)
	}
}

func TestGroupStatusAuthorFollowsGroupAddressing(t *testing.T) {
	pn := types.JID{User: "5511999999999", Server: types.DefaultUserServer, Device: 3}
	lid := types.JID{User: "123456789", Server: types.HiddenUserServer, Device: 3}
	pnGroup := types.NewJID("120363000000000003", types.GroupServer)
	cli := &Client{
		Store: &store.Device{ID: &pn, LID: lid},
		groupCache: map[types.JID]*groupMetaCache{
			testStatusGroup: {AddressingMode: types.AddressingModeLID},
			pnGroup:         {AddressingMode: types.AddressingModePN},
		},
	}

	author, err := cli.getGroupStatusAuthor(context.Background(), testStatusGroup)
	if err != nil {
		t.Fatalf("getGroupStatusAuthor returned an error: %v", err)
	}
	if author != lid.ToNonAD() {
		t.Fatalf("author in LID group = %s, want %s", author, lid.ToNonAD())
	}
	author, err = cli.getGroupStatusAuthor(context.Background(), pnGroup)
	if err != nil {
		t.Fatalf("getGroupStatusAuthor returned an error: %v", err)
	}
	if author != pn.ToNonAD() {
		t.Fatalf("author in PN group = %s, want %s", author, pn.ToNonAD())
	}

	if _, err = (&Client{Store: &store.Device{}}).getGroupStatusAuthor(context.Background(), testStatusGroup); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("logged out error = %v, want ErrNotLoggedIn", err)
	}
}

func countMetaNodes(content []waBinary.Node) (int, waBinary.Attrs) {
	count := 0
	var attrs waBinary.Attrs
	for _, node := range content {
		if node.Tag == "meta" {
			count++
			attrs = node.Attrs
		}
	}
	return count, attrs
}

func TestMessageContentMarksGroupStatus(t *testing.T) {
	msg, err := BuildGroupStatusMessage(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, types.EmptyJID)
	if err != nil {
		t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
	}
	// Retries call getMessageContent with empty extra params, so this also
	// covers the stanza rebuilt for a retry receipt.
	base := waBinary.Node{Tag: "participants"}
	content := (&Client{}).getMessageContent(base, msg, waBinary.Attrs{"type": getTypeFromMessage(msg)}, false, nodeExtraParams{})
	count, attrs := countMetaNodes(content)
	if count != 1 {
		t.Fatalf("got %d <meta> nodes, want 1: %v", count, content)
	}
	if attrs[groupStatusMetaFlag] != "true" || attrs[groupStatusMetaOrigin] != groupStatusOrigin {
		t.Fatalf("meta attrs = %v", attrs)
	}
	if content[0].Tag != "participants" {
		t.Fatalf("first child = %q, want participants", content[0].Tag)
	}
}

func TestMessageContentMergesGroupStatusIntoExistingMeta(t *testing.T) {
	msg, err := BuildGroupStatusMessage(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, types.EmptyJID)
	if err != nil {
		t.Fatalf("BuildGroupStatusMessage returned an error: %v", err)
	}
	cli := &Client{}

	threadMeta := &waBinary.Node{Tag: "meta", Attrs: waBinary.Attrs{"thread_msg_id": "ABC"}}
	content := cli.getMessageContent(waBinary.Node{Tag: "participants"}, msg, waBinary.Attrs{}, false, nodeExtraParams{metaNode: threadMeta})
	count, attrs := countMetaNodes(content)
	if count != 1 {
		t.Fatalf("got %d <meta> nodes, want 1: %v", count, content)
	}
	if attrs["thread_msg_id"] != "ABC" || attrs[groupStatusMetaFlag] != "true" || attrs[groupStatusMetaOrigin] != groupStatusOrigin {
		t.Fatalf("merged meta attrs = %v", attrs)
	}
	if _, changed := threadMeta.Attrs[groupStatusMetaFlag]; changed {
		t.Fatal("the caller's meta node was modified")
	}

	// A caller that already adds the marker through AdditionalNodes must not
	// end up with a second <meta>.
	additional := []waBinary.Node{{Tag: "meta", Attrs: waBinary.Attrs{groupStatusMetaFlag: "true", groupStatusMetaOrigin: groupStatusOrigin}}}
	content = cli.getMessageContent(waBinary.Node{Tag: "participants"}, msg, waBinary.Attrs{}, false, nodeExtraParams{additionalNodes: &additional})
	if count, _ = countMetaNodes(content); count != 1 {
		t.Fatalf("got %d <meta> nodes with additional nodes, want 1: %v", count, content)
	}
}

func TestMessageContentLeavesRegularMessagesAlone(t *testing.T) {
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}
	content := (&Client{}).getMessageContent(waBinary.Node{Tag: "participants"}, msg, waBinary.Attrs{"type": "media"}, false, nodeExtraParams{})
	if count, _ := countMetaNodes(content); count != 0 {
		t.Fatalf("regular message got %d <meta> nodes: %v", count, content)
	}
}
