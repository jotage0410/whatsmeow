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
	"go.mau.fi/whatsmeow/proto/waCommon"
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
// The sender must be a member of the group, and an admin when only admins can send messages there.
// The group info is fetched before sending: the error wraps ErrNotInGroup when the server says the
// account is not a member, and is ErrGroupStatusAdminsOnly when only admins can send messages and
// this account is not one (the server would accept the status and deliver it to nobody). Group
// status is a recent WhatsApp feature, so the server may still reject it for some accounts or
// groups.
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
	ownPN, ownLID := cli.getOwnID(), cli.getOwnLID()
	if ownPN.IsEmpty() {
		return SendResponse{}, ErrNotLoggedIn
	}
	// Fresh group info, also for the author below: when only admins can send messages, the server
	// accepts a group status from any other member and delivers it to nobody, without an error.
	info, err := cli.getGroupInfo(ctx, groupJID, true)
	if err != nil {
		return SendResponse{}, fmt.Errorf("failed to get group info: %w", err)
	}
	if err = checkGroupStatusPermission(info, ownPN, ownLID); err != nil {
		return SendResponse{}, err
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

// checkGroupStatusPermission returns ErrGroupStatusAdminsOnly when only admins can send messages
// in the group and this account is not an admin. The server accepts a group status in that case
// but does not deliver it to anyone (checked on a device on 2026-11-14). Restricting who can edit
// the group info does not matter. When the account is not in the member list, the server decides.
func checkGroupStatusPermission(info *types.GroupInfo, ownPN, ownLID types.JID) error {
	if info == nil || !info.IsAnnounce {
		return nil
	}
	for _, p := range info.Participants {
		for _, member := range []types.JID{p.JID, p.PhoneNumber, p.LID} {
			if member.IsEmpty() {
				continue
			}
			if (!ownPN.IsEmpty() && member.User == ownPN.User && member.Server == ownPN.Server) ||
				(!ownLID.IsEmpty() && member.User == ownLID.User && member.Server == ownLID.Server) {
				if p.IsAdmin || p.IsSuperAdmin {
					return nil
				}
				return ErrGroupStatusAdminsOnly
			}
		}
	}
	return nil
}

// getGroupStatusAuthor returns the identity this account posts with in the given group, after
// checking that the account is still a member (see groupStatusAuthor).
func (cli *Client) getGroupStatusAuthor(ctx context.Context, groupJID types.JID) (types.JID, error) {
	return groupStatusAuthor(ctx, cli, groupJID, cli.getOwnID(), cli.getOwnLID())
}

// groupStatusGroupSource is what SendGroupStatus needs to know about the target group;
// *Client implements it.
type groupStatusGroupSource interface {
	getCachedGroupData(ctx context.Context, jid types.JID) (*groupMetaCache, error)
	refreshGroupData(ctx context.Context, jid types.JID) (*groupMetaCache, error)
}

// refreshGroupData asks the server for the group info, bypassing the cache, and returns the
// refreshed cache entry.
func (cli *Client) refreshGroupData(ctx context.Context, jid types.JID) (*groupMetaCache, error) {
	if _, err := cli.getGroupInfo(ctx, jid, true); err != nil {
		return nil, err
	}
	cli.groupCacheLock.Lock()
	defer cli.groupCacheLock.Unlock()
	return cli.groupCache[jid], nil
}

// groupStatusAuthor returns the identity this account posts with in the group, which is the LID
// in groups that use LID addressing, the same rule SendMessage uses.
//
// It also makes sure the account is still a member. The group cache only follows the group
// notifications this session received: removing the account from a group deletes it from the
// cached members but keeps the entry, so sending from the cache would leave the outcome to the
// server's ack. When the cached members do not include the account, the group info is fetched
// again; the server answers that query with 403 (ErrNotInGroup) when the account is not a
// member. If the fresh member list still does not name the account, the status is sent anyway
// and the server decides, rather than refusing on a member list that may be incomplete.
func groupStatusAuthor(ctx context.Context, source groupStatusGroupSource, groupJID, ownPN, ownLID types.JID) (types.JID, error) {
	if ownPN.IsEmpty() {
		return types.EmptyJID, ErrNotLoggedIn
	}
	groupData, err := source.getCachedGroupData(ctx, groupJID)
	if err == nil && !groupData.hasMember(ownPN, ownLID) {
		groupData, err = source.refreshGroupData(ctx, groupJID)
	}
	if err != nil {
		return types.EmptyJID, fmt.Errorf("failed to get group info: %w", err)
	} else if groupData == nil {
		return types.EmptyJID, fmt.Errorf("failed to get group info: no data for %s", groupJID)
	}
	if groupData.AddressingMode == types.AddressingModeLID && !ownLID.IsEmpty() {
		return ownLID.ToNonAD(), nil
	}
	return ownPN.ToNonAD(), nil
}

// hasMember reports whether any of the given identities is in the cached member list, ignoring
// devices. Empty JIDs never match.
func (gmc *groupMetaCache) hasMember(ids ...types.JID) bool {
	if gmc == nil {
		return false
	}
	for _, member := range gmc.Members {
		for _, id := range ids {
			if !id.IsEmpty() && member.User == id.User && member.Server == id.Server {
				return true
			}
		}
	}
	return false
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

// BuildGroupStatusRevoke builds the message that deletes one of this account's group status
// updates. It is a regular revoke wrapped in the group status envelope (GroupStatusMessageV2), so
// that the stanza carries the <meta is_group_status="true" origin="group_status"/> marker, also
// when it is sent again on a retry receipt. A plain revoke (BuildRevoke) sent to the group reaches
// the members as a revoke of a chat message and leaves the status in place.
func BuildGroupStatusRevoke(groupJID types.JID, id types.MessageID) *waE2E.Message {
	return &waE2E.Message{
		GroupStatusMessageV2: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ProtocolMessage: &waE2E.ProtocolMessage{
					Type: waE2E.ProtocolMessage_REVOKE.Enum(),
					Key: &waCommon.MessageKey{
						RemoteJID: new(groupJID.String()),
						FromMe:    new(true),
						ID:        new(id),
					},
				},
			},
		},
	}
}

// RevokeGroupStatus deletes one of this account's group status updates for every member.
//
//	resp, err := cli.SendGroupStatus(ctx, groupJID, content)
//	...
//	_, err = cli.RevokeGroupStatus(ctx, groupJID, resp.ID)
func (cli *Client) RevokeGroupStatus(ctx context.Context, groupJID types.JID, id types.MessageID, extra ...SendRequestExtra) (SendResponse, error) {
	if cli == nil {
		return SendResponse{}, ErrClientIsNil
	}
	if groupJID.Server != types.GroupServer || groupJID.User == "" {
		return SendResponse{}, fmt.Errorf("%w (got %q)", ErrGroupStatusNotGroup, groupJID.String())
	}
	return cli.SendMessage(ctx, groupJID, BuildGroupStatusRevoke(groupJID, id), extra...)
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
