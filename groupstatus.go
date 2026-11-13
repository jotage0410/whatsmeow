// Copyright (c) 2026 Verbeux AI
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"fmt"
	"maps"

	"go.mau.fi/util/random"
	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waStatusAttributions"
	"go.mau.fi/whatsmeow/types"
)

// Attributes of the <meta> child that marks a message stanza as a group status update.
const (
	groupStatusMetaFlag   = "is_group_status"
	groupStatusMetaOrigin = "origin"
	groupStatusOrigin     = "group_status"
)

// SendGroupStatus posts a status update to a group. The update shows up in the ring around the
// group's picture and every member of the group can see it, unlike a regular status, which goes
// to status@broadcast and is only shown to the sender's contacts.
//
// The inner message must be an image, video, audio or text (Conversation or ExtendedTextMessage)
// message, built the same way as for a regular status (see Upload for media). It is not modified:
// the envelope is built from a copy, see BuildGroupStatusMessage.
//
//	resp, err := cli.SendGroupStatus(ctx, groupJID, &waE2E.Message{
//		ImageMessage: &waE2E.ImageMessage{...},
//	})
//
// The sender must be a member of the group. Group status is a recent WhatsApp feature, so the
// server may still reject it for some accounts or groups.
func (cli *Client) SendGroupStatus(ctx context.Context, groupJID types.JID, inner *waE2E.Message, extra ...SendRequestExtra) (SendResponse, error) {
	if cli == nil {
		return SendResponse{}, ErrClientIsNil
	}
	if groupJID.Server != types.GroupServer || groupJID.User == "" {
		return SendResponse{}, fmt.Errorf("%w (got %q)", ErrGroupStatusNotGroup, groupJID.String())
	}
	if inner == nil {
		return SendResponse{}, fmt.Errorf("%w: message is nil", ErrGroupStatusUnsupportedContent)
	}
	author, err := cli.getGroupStatusAuthor(ctx, groupJID)
	if err != nil {
		return SendResponse{}, err
	}
	message, err := BuildGroupStatusMessage(inner, author)
	if err != nil {
		return SendResponse{}, err
	}
	return cli.SendMessage(ctx, groupJID, message, extra...)
}

// getGroupStatusAuthor returns the identity this account posts with in the given group,
// which is the LID in groups that use LID addressing, the same rule SendMessage uses.
func (cli *Client) getGroupStatusAuthor(ctx context.Context, groupJID types.JID) (types.JID, error) {
	ownID := cli.getOwnID()
	if ownID.IsEmpty() {
		return types.EmptyJID, ErrNotLoggedIn
	}
	groupData, err := cli.getCachedGroupData(ctx, groupJID)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("failed to get group info: %w", err)
	}
	if groupData.AddressingMode == types.AddressingModeLID {
		if ownLID := cli.getOwnLID(); !ownLID.IsEmpty() {
			return ownLID.ToNonAD(), nil
		}
	}
	return ownID.ToNonAD(), nil
}

// BuildGroupStatusMessage wraps a message in the envelope WhatsApp uses for group status updates.
// The result can be sent to the group with Client.SendMessage, which adds the
// <meta is_group_status="true" origin="group_status"/> marker to the stanza by itself;
// Client.SendGroupStatus does both steps.
//
// The envelope is a GroupStatusMessageV2 carrying a fresh 32-byte message secret. The copy of the
// inner message is flagged with contextInfo.isGroupStatus and, unless the caller already set
// them, gets the matching status source type and a GROUP_STATUS attribution naming authorJID
// (skipped when authorJID is empty). A Conversation becomes an ExtendedTextMessage, because a
// plain conversation has no context info to carry the flag.
func BuildGroupStatusMessage(inner *waE2E.Message, authorJID types.JID) (*waE2E.Message, error) {
	if inner == nil {
		return nil, fmt.Errorf("%w: message is nil", ErrGroupStatusUnsupportedContent)
	}
	msg := proto.Clone(inner).(*waE2E.Message)
	if msg.Conversation != nil && msg.ExtendedTextMessage == nil {
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: msg.Conversation}
		msg.Conversation = nil
	}

	var contextInfo **waE2E.ContextInfo
	var sourceType waE2E.ContextInfo_StatusSourceType
	switch {
	case msg.ImageMessage != nil:
		contextInfo, sourceType = &msg.ImageMessage.ContextInfo, waE2E.ContextInfo_IMAGE
	case msg.VideoMessage != nil:
		contextInfo, sourceType = &msg.VideoMessage.ContextInfo, waE2E.ContextInfo_VIDEO
		if msg.VideoMessage.GetGifPlayback() {
			sourceType = waE2E.ContextInfo_GIF
		}
	case msg.AudioMessage != nil:
		contextInfo, sourceType = &msg.AudioMessage.ContextInfo, waE2E.ContextInfo_AUDIO
	case msg.ExtendedTextMessage != nil:
		contextInfo, sourceType = &msg.ExtendedTextMessage.ContextInfo, waE2E.ContextInfo_TEXT
	default:
		return nil, ErrGroupStatusUnsupportedContent
	}
	if *contextInfo == nil {
		*contextInfo = &waE2E.ContextInfo{}
	}
	info := *contextInfo
	info.IsGroupStatus = new(true)
	if info.StatusSourceType == nil {
		info.StatusSourceType = sourceType.Enum()
	}
	if len(info.StatusAttributions) == 0 && !authorJID.IsEmpty() {
		info.StatusAttributions = []*waStatusAttributions.StatusAttribution{{
			Type: waStatusAttributions.StatusAttribution_GROUP_STATUS.Enum(),
			AttributionData: &waStatusAttributions.StatusAttribution_GroupStatus_{
				GroupStatus: &waStatusAttributions.StatusAttribution_GroupStatus{
					AuthorJID: new(authorJID.ToNonAD().String()),
				},
			},
		}}
	}

	return &waE2E.Message{
		GroupStatusMessageV2: &waE2E.FutureProofMessage{Message: msg},
		MessageContextInfo: &waE2E.MessageContextInfo{
			MessageSecret: random.Bytes(32),
		},
	}, nil
}

func isGroupStatusMessage(msg *waE2E.Message) bool {
	return msg.GetGroupStatusMessageV2().GetMessage() != nil || msg.GetGroupStatusMessage().GetMessage() != nil
}

// withGroupStatusMeta marks the children of a message stanza as a group status update. The flags
// are merged into the existing <meta> child when there is one, because a stanza carries a single
// <meta>; its attribute map is copied, so nodes passed in by the caller are left untouched.
func withGroupStatusMeta(content []waBinary.Node) []waBinary.Node {
	for i, node := range content {
		if node.Tag != "meta" {
			continue
		}
		attrs := make(waBinary.Attrs, len(node.Attrs)+2)
		maps.Copy(attrs, node.Attrs)
		attrs[groupStatusMetaFlag] = "true"
		attrs[groupStatusMetaOrigin] = groupStatusOrigin
		node.Attrs = attrs
		content[i] = node
		return content
	}
	return append(content, waBinary.Node{
		Tag: "meta",
		Attrs: waBinary.Attrs{
			groupStatusMetaFlag:   "true",
			groupStatusMetaOrigin: groupStatusOrigin,
		},
	})
}
