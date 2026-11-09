// Copyright (c) 2026 Verbeux AI
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"encoding/hex"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

// newsletterPostMeta returns the message type and the <meta> node that the
// official clients attach when posting a poll, quiz or question to a channel.
// Without them the server stores the post as plain text: followers can still
// see it, but their votes and answers are not recorded, and the channel owner
// gets no vote or response counts. It returns a nil node for other messages.
func newsletterPostMeta(message *waE2E.Message) (string, *waBinary.Node) {
	meta := func(attrs waBinary.Attrs) *waBinary.Node {
		return &waBinary.Node{Tag: "meta", Attrs: attrs}
	}
	if poll := newsletterPollCreation(message); poll != nil {
		pollType := "creation"
		if poll.GetPollType() == waE2E.PollType_QUIZ {
			pollType = "quiz_creation"
		}
		return "poll", meta(waBinary.Attrs{"contenttype": "text", "polltype": pollType})
	}
	if message.GetQuestionMessage().GetMessage() != nil {
		return "text", meta(waBinary.Attrs{"questiontype": "question"})
	}
	return "", nil
}

func newsletterPollCreation(message *waE2E.Message) *waE2E.PollCreationMessage {
	for _, poll := range []*waE2E.PollCreationMessage{
		message.GetPollCreationMessage(),
		message.GetPollCreationMessageV2(),
		message.GetPollCreationMessageV3(),
		message.GetPollCreationMessageV5(),
		message.GetPollCreationMessageV6(),
	} {
		if poll != nil {
			return poll
		}
	}
	return nil
}

// parseNewsletterVotes reads the <votes> child of a channel message update.
// Each <vote> holds the raw SHA-256 hash of an option name, the same hash used
// for poll votes in other chats; it is returned hex-encoded.
func parseNewsletterVotes(node waBinary.Node) map[string]int {
	votes := make(map[string]int)
	for _, vote := range node.GetChildren() {
		if vote.Tag != "vote" {
			continue
		}
		hash, ok := vote.Content.([]byte)
		if !ok || len(hash) == 0 {
			continue
		}
		votes[hex.EncodeToString(hash)] = vote.AttrGetter().Int("count")
	}
	return votes
}
