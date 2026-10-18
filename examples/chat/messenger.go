// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

const (
	botName       = "main"
	bridgeName    = "Muster example bridge"
	messengerName = "Example chat"
	buttonsPerRow = 3
)

// featureNames are the members of info.capabilities, which BRIDGE_FEATURES picks from.
var featureNames = []string{"buttons", "thread_messages", "direct_messages", "private_answers", "native_mentions",
	"quiet_messages", "status_color", "deletion_events", "delete"}

// features are the features the example offers, by their names in info.capabilities.
type features map[string]bool

// parseFeatures reads BRIDGE_FEATURES: every feature when it is not set, else those it lists, comma-separated.
func parseFeatures(list string, set bool) (features, error) {
	on := features{}
	if !set {
		for _, name := range featureNames {
			on[name] = true
		}
		return on, nil
	}
	for name := range strings.SplitSeq(list, ",") {
		switch name = strings.TrimSpace(name); {
		case name == "":
		case slices.Contains(featureNames, name):
			on[name] = true
		default:
			return nil, fmt.Errorf("BRIDGE_FEATURES: unknown feature %q; the features are %s", name,
				strings.Join(featureNames, ", "))
		}
	}
	return on, nil
}

// names are the features that are on, in the order of featureNames.
func (f features) names() []string {
	var on []string
	for _, name := range featureNames {
		if f[name] {
			on = append(on, name)
		}
	}
	return on
}

func capabilities(on features) bridge.Capabilities {
	var caps bridge.Capabilities
	if on["buttons"] {
		caps.Buttons = &bridge.Capabilities_Buttons{MaxPerMessage: 5, MaxPerRow: buttonsPerRow, DataMaxBytes: 64,
			Styles: []bridge.ButtonStyle{bridge.ButtonStylePrimary, bridge.ButtonStyleSuccess,
				bridge.ButtonStyleNeutral, bridge.ButtonStyleDanger}}
	}
	if on["thread_messages"] {
		caps.ThreadMessages = new(bridge.CapabilitiesThreadMessagesAll)
	}
	if on["direct_messages"] {
		caps.DirectMessages = &bridge.Capabilities_DirectMessages{BotCanStart: true}
	}
	if on["private_answers"] {
		caps.PrivateAnswers = &[]bridge.CapabilitiesPrivateAnswers{bridge.CapabilitiesPrivateAnswersEphemeral}
	}
	for name, member := range map[string]**bool{"native_mentions": &caps.NativeMentions,
		"quiet_messages": &caps.QuietMessages, "status_color": &caps.StatusColor,
		"deletion_events": &caps.DeletionEvents} {
		if on[name] {
			*member = new(true)
		}
	}
	if on["delete"] {
		caps.Delete = &bridge.Capabilities_Delete{}
	}
	return caps
}

// userLookup finds people by login while the bot can write to them.
func userLookup(on features) bridge.InfoUserLookup {
	if on["direct_messages"] {
		return bridge.InfoUserLookupLogin
	}
	return bridge.InfoUserLookupNone
}

func limits(s settings) bridge.Limits {
	l := bridge.Limits{MaxBodyLength: s.maxBodyLength}
	if s.editMaxAge != 0 {
		l.EditMaxAgeSeconds = new(s.editMaxAge)
	}
	return l
}

// bridgeConfig is the bridge the settings describe, with both event delivery modes.
func bridgeConfig(s settings, log *slog.Logger) bridge.Config {
	return bridge.Config{
		Name:          bridgeName,
		Version:       s.version,
		MessengerName: messengerName,
		Bots:          []bridge.Bot{{Name: botName, Token: s.token, Push: s.push}},
		Capabilities:  capabilities(s.features),
		Limits:        limits(s),
		UserLookup:    userLookup(s.features),
		Logger:        log,
	}
}

// messenger is the Messenger of the bridge over the chat. The library checks every request against the limits and
// capabilities before it calls a method, so the methods only find what they act on.
type messenger struct {
	chat       *chat
	on         features
	editMaxAge time.Duration // 0 for none
}

var (
	_ bridge.Messenger       = (*messenger)(nil)
	_ bridge.UserFinder      = (*messenger)(nil)
	_ bridge.DirectMessenger = (*messenger)(nil)
	_ bridge.Deleter         = (*messenger)(nil)
)

func notMember() *bridge.Error {
	return bridge.ForbiddenError("The bot is not a member of this channel.")
}

func noMessage() *bridge.Error {
	return bridge.GoneError("No message of the bot has this id; it was deleted or never existed.")
}

func (m *messenger) PostMessage(_ context.Context, _ string, in bridge.MessageInput) (bridge.MessagePosted, error) {
	p := m.content(bridge.MessageContent{Status: in.Status, Color: in.Color, Emoji: in.Emoji, Title: in.Title,
		Link: in.Link, Body: in.Body, Buttons: in.Buttons}, in.Mentions)
	p.Channel, p.Kind, p.Quiet = in.ChannelId, in.Kind, !in.Notify && m.on["quiet_messages"]
	id, err := m.chat.postRoot(p)
	if err != nil {
		return bridge.MessagePosted{}, notMember()
	}
	return bridge.MessagePosted{MessageId: id, ThreadRef: new(id)}, nil
}

func (m *messenger) EditMessage(_ context.Context, _, messageID string, in bridge.MessageContent) (
	bridge.MessageEdited, error) {
	switch err := m.chat.edit(messageID, m.content(in, nil), m.editMaxAge); {
	case errors.Is(err, errEditWindow):
		return bridge.MessageEdited{}, bridge.GoneError("The message is older than -edit-max-age allows.",
			bridge.ReasonEditWindowClosed)
	case err != nil:
		return bridge.MessageEdited{}, noMessage()
	}
	return bridge.MessageEdited{}, nil
}

func (m *messenger) ReplyInThread(_ context.Context, _, messageID string, in bridge.ThreadReplyInput) (
	bridge.ThreadReplyPosted, error) {
	if in.ThreadRef != nil {
		if root, err := m.chat.rootOf(messageID); err != nil || root != *in.ThreadRef {
			return bridge.ThreadReplyPosted{}, bridge.GoneError("This Thread does not exist.")
		}
	}
	p := post{Bot: true, Body: in.Body, HTML: renderBody(in.Body, in.Mentions, m.on["native_mentions"]),
		Quiet: !in.Notify && m.on["quiet_messages"]}
	p.Buttons, p.data = buttons(in.Buttons)
	at, err := m.chat.postInThread(messageID, "", p)
	if err != nil {
		return bridge.ThreadReplyPosted{}, noMessage()
	}
	return bridge.ThreadReplyPosted{MessageId: at.id, ThreadRef: at.root}, nil
}

func (m *messenger) CheckChannel(_ context.Context, _, channelID string) (string, error) {
	if !m.chat.hasChannel(channelID) {
		return "", notMember()
	}
	return "#" + channelID, nil
}

// AnswerEvent shows the answer where ev.Chat, as the page reported it, says: under the pressed post to the person
// alone, in the Thread, or in the person's direct chat with the bot.
func (m *messenger) AnswerEvent(_ context.Context, _ string, ev bridge.ReportedEvent, text string) (
	bridge.AnswerShownShownAs, error) {
	answer := post{Bot: true, Text: text, HTML: renderText(text)}
	var shown bridge.AnswerShownShownAs
	var err error
	switch ev.Type {
	case bridge.EventTypePress:
		if m.on["private_answers"] {
			shown, err = bridge.AnswerShownShownAsEphemeral, m.chat.addNote(ev.MessageID, ev.Chat,
				ev.User.ExternalId, text)
			break
		}
		shown = bridge.AnswerShownShownAsThread
		_, err = m.chat.postInThread(ev.MessageID, ev.Chat, answer)
	case bridge.EventTypeThreadMessage:
		shown = bridge.AnswerShownShownAsThread
		_, err = m.chat.postInThread(ev.MessageID, ev.Chat, answer)
	case bridge.EventTypeAccountLink:
		shown = bridge.AnswerShownShownAsDirect
		login, ok := strings.CutPrefix(ev.Chat, "direct:")
		if !ok || login != ev.User.ExternalId {
			err = errNoPerson
			break
		}
		_, err = m.chat.addDirect(login, directEntry{Bot: true, Text: text, HTML: answer.HTML})
	default:
		return "", bridge.GoneError("This event cannot be answered.")
	}
	if err != nil {
		return "", bridge.GoneError("The message or chat of this event no longer exists.")
	}
	return shown, nil
}

func (m *messenger) FindUser(_ context.Context, _, handle string) (*bridge.Person, error) {
	p, ok := m.chat.person(handle)
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (m *messenger) SendDirectMessage(_ context.Context, _ string, in bridge.DirectMessageInput) (
	bridge.DirectMessageSent, error) {
	id, err := m.chat.addDirect(in.UserExternalId, directEntry{Bot: true, Body: in.Body,
		HTML: renderBody(in.Body, nil, false)})
	if err != nil {
		return bridge.DirectMessageSent{}, bridge.ForbiddenError("No person with this id can receive a message " +
			"from the bot.")
	}
	return bridge.DirectMessageSent{MessageId: id}, nil
}

// DeleteMessage deletes a post of the bot as Muster asks; only a deletion on the page is a message_deleted event.
func (m *messenger) DeleteMessage(_ context.Context, _, messageID string) error {
	if _, err := m.chat.remove(messageID, true); err != nil {
		return noMessage()
	}
	return nil
}

// content is a post of the bot that shows c: its status by colour with status_color, else by emoji.
func (m *messenger) content(c bridge.MessageContent, mentions []bridge.Mention) post {
	p := post{Bot: true, Status: c.Status, Title: c.Title, Link: pageLink{Label: c.Link.Label}, Body: c.Body,
		HTML: renderBody(c.Body, mentions, m.on["native_mentions"])}
	if webLink(c.Link.Url) {
		p.Link.URL = c.Link.Url
	}
	if m.on["status_color"] {
		p.Color = c.Color
	} else {
		p.Emoji = c.Emoji
	}
	p.Buttons, p.data = buttons(c.Buttons)
	return p
}

// buttons splits the buttons into what the page shows and the data a press carries back.
func buttons(in []bridge.Button) ([]pageButton, []string) {
	shown := make([]pageButton, 0, len(in))
	data := make([]string, 0, len(in))
	for _, b := range in {
		shown = append(shown, pageButton{Label: b.Label, Style: b.Style})
		data = append(data, b.Data)
	}
	return shown, data
}
