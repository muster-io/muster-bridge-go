// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"time"
	"unicode/utf8"
)

// Role is what a bridge is: a bridge to a messenger or a Notification gateway, or both.
type Role = InfoRole

// The roles of a bridge.
const (
	// RoleMessenger is a bridge to a messenger, whose channels are Destinations; it needs a [Messenger].
	RoleMessenger = InfoRoleMessenger
	// RoleGateway is a Notification gateway, which Muster calls with sendNotification; it needs a [Gateway].
	RoleGateway = InfoRoleGateway
)

const (
	maxNotificationID = 512
	// lookupAttempts is how often keepNotification tries the claim of a key that a lookup holds for an instant.
	lookupAttempts = 10
)

// Gateway is what a bridge in the role gateway implements: the calls to its provider, such as a phone call or an SMS.
// Each method is given the bot whose token Muster sent, by its name in Config.Bots, and returns the protocol's answer,
// or an error: an *Error to answer its class, any other error to answer transient with a generic detail. The library
// validates every request before it calls a method and every answer before it sends it.
type Gateway interface {
	// SendNotification reaches the person at in.Contact and reads in.Text out or sends it, in in.Language. With
	// in.Action the person can acknowledge as in.Action.Label tells them; the bridge then calls [Bridge.Acknowledge]
	// with the NotificationId that this returns, which must be 1 to 512 characters and unique per bot: the library
	// keeps the action of the first notification of an id. A contact the provider refuses for good is an [InvalidError];
	// credentials the provider refuses are a [ForbiddenError].
	SendNotification(ctx context.Context, bot string, in NotificationInput) (NotificationSent, error)
	// Answer shows text to the person who acknowledged, which the bridge reported with [Bridge.Acknowledge], and
	// returns where it was shown, normally AnswerShownShownAsNotification. Muster answers an acknowledgement pushed
	// during a phone call before it answers the push, so that the answer can be said to the person still on the line.
	Answer(ctx context.Context, bot string, ev ReportedEvent, text string) (AnswerShownShownAs, error)
}

var (
	// ErrUnknownNotification is the error of Acknowledge for a notification this bridge did not send to the bot, or
	// no longer keeps: notifications are kept for 24 hours.
	ErrUnknownNotification = errors.New("bridge: no such notification")
	// ErrNoAction is the error of Acknowledge for a notification that was sent without an action, so that there is
	// nothing to acknowledge.
	ErrNoAction = errors.New("bridge: the notification has no action to acknowledge")
)

// hasRole reports whether the bridge has the role.
func (b *Bridge) hasRole(r Role) bool { return slices.Contains(b.roles, r) }

// checkRoles makes the roles of the configuration and names what its Messenger and Gateway lack or have too much.
func checkRoles(cfg Config, m Messenger) (roles []Role, errs []error) {
	roles = slices.Clone(cfg.Roles)
	if cfg.Roles == nil {
		roles = []Role{RoleMessenger}
	}
	if len(roles) == 0 {
		errs = append(errs, errors.New("bridge: roles is empty: a bridge has the role messenger, gateway, or both"))
	}
	seen := map[Role]bool{}
	for i, r := range roles {
		switch {
		case !r.Valid():
			errs = append(errs, fmt.Errorf("bridge: roles[%d] %q is not messenger or gateway", i, r))
		case seen[r]:
			errs = append(errs, fmt.Errorf("bridge: roles[%d] %q is repeated", i, r))
		default:
			seen[r] = true
		}
	}
	switch {
	case seen[RoleMessenger] && m == nil:
		errs = append(errs, errors.New("bridge: the Messenger is nil, which the role messenger needs; a bridge whose "+
			"only role is gateway passes a nil Messenger"))
	case !seen[RoleMessenger] && m != nil && len(errs) == 0:
		errs = append(errs, errors.New("bridge: a Messenger is given, but roles has no messenger; add the role or "+
			"pass a nil Messenger"))
	}
	if len(errs) == 0 && !seen[RoleMessenger] {
		errs = append(errs, checkNoMessengerFeatures(cfg)...)
	}
	switch {
	case seen[RoleGateway] && cfg.Gateway == nil:
		errs = append(errs, errors.New("bridge: Config.Gateway is nil, which the role gateway needs"))
	case !seen[RoleGateway] && cfg.Gateway != nil:
		errs = append(errs, errors.New("bridge: Config.Gateway is set, but roles has no gateway; add the role or "+
			"leave the Gateway out"))
	}
	return roles, errs
}

// checkNoMessengerFeatures refuses what only a messenger declares in a bridge that is not one: its capabilities, a
// user lookup, mentions and a link template (a gateway's info has none of them).
func checkNoMessengerFeatures(cfg Config) []error {
	var errs []error
	if !reflect.DeepEqual(cfg.Capabilities, Capabilities{}) {
		errs = append(errs, errors.New("bridge: capabilities are a messenger's, but roles has no messenger"))
	}
	if cfg.UserLookup != "" && cfg.UserLookup != InfoUserLookupNone {
		errs = append(errs, errors.New("bridge: user_lookup is a messenger's, but roles has no messenger"))
	}
	if cfg.GroupMentions || cfg.EveryoneMentions != nil {
		errs = append(errs, errors.New("bridge: group and everyone mentions are a messenger's, but roles has no messenger"))
	}
	if cfg.LinkURLTemplate != "" {
		errs = append(errs, errors.New("bridge: link_url_template is a messenger's, but roles has no messenger"))
	}
	return errs
}

// notificationRecord is what the Store keeps of a notification: the data of its action, "" for none.
type notificationRecord struct {
	Data string `json:"data"`
}

// notificationKey is the Store key of a notification. It starts with a space, which no Idempotency-Key can, so that
// it never meets one; it holds the hash of the id, so that it fits any store whatever the length of the id.
func notificationKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return " notification " + hex.EncodeToString(sum[:])
}

func (c *call) sendNotification() (any, error) {
	if !c.b.hasRole(RoleGateway) {
		return nil, refuse(InvalidError("This bridge is not in the role gateway (info.roles), so it does not send " +
			"notifications."))
	}
	var in NotificationInput
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	// The request was validated against the specification, which bounds the text to 1,000 code points and the action
	// data to 64 printable ASCII bytes, so the Gateway never sees more. The contact and the text are never logged:
	// they are redacted from an error of the Gateway that quotes them.
	c.note.secrets = append(c.note.secrets, in.Contact, in.Text)
	out, err := c.b.gateway.SendNotification(c.ctx, c.bot, in)
	if err != nil {
		return nil, err
	}
	out.Replayed = false
	if n := utf8.RuneCountInString(out.NotificationId); n >= 1 && n <= maxNotificationID {
		record := notificationRecord{}
		if in.Action != nil {
			record.Data = in.Action.Data
		}
		if err := c.b.keepNotification(c.ctx, c.bot, out.NotificationId, record); err != nil {
			c.log(slog.LevelError, "bridge: cannot keep the action of a notification, it cannot be acknowledged",
				slog.String("error", c.b.redact(err.Error())))
		}
	}
	return out, nil
}

// keepNotification stores the record of a notification for 24 hours under its derived key. It takes the key's claim
// first, because a lookup by Acknowledge holds a claim for an instant and SaveKey leaves a key that another claim
// holds as it is; the first record of an id stays.
func (b *Bridge) keepNotification(ctx context.Context, bot, id string, record notificationRecord) error {
	ctx = context.WithoutCancel(ctx)
	key := notificationKey(id)
	for range lookupAttempts {
		now := b.clock.Now()
		claim, err := b.store.ClaimKey(ctx, bot, key, now, now.Add(keyClaimTTL))
		switch {
		case err != nil:
			return err
		case claim.State == KeyClaimed:
			err := b.store.SaveKey(ctx, bot, key, claim.ID, StoredAnswer{Status: http.StatusCreated,
				Body: mustMarshal(record)}, now.Add(keyRetention))
			if err != nil {
				_ = b.store.ReleaseKey(ctx, bot, key, claim.ID)
			}
			return err
		case claim.State == KeyAnswered:
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return errors.New("the key of the notification stayed claimed")
}

// Acknowledge reports that the person acknowledged the notification of the bot that [Gateway.SendNotification]
// answered with notificationID: it queues notification_acknowledged with the data of the notification's action, for
// pollEvents and the push like any other event. It returns once the event is queued. A notification that this bridge
// did not send to the bot, or no longer keeps (or is still keeping: retry), is [ErrUnknownNotification]; one that was sent without an action is
// [ErrNoAction]. Acknowledging a notification twice queues one event.
func (b *Bridge) Acknowledge(ctx context.Context, bot, notificationID string) error {
	if _, ok := b.bots[bot]; !ok {
		return ErrUnknownBot
	}
	if !b.hasRole(RoleGateway) {
		return fmt.Errorf("%w: acknowledge needs the role gateway", ErrNotOffered)
	}
	if n := utf8.RuneCountInString(notificationID); n < 1 || n > maxNotificationID {
		return fmt.Errorf("%w: notification_id is empty or longer than %d characters", ErrInvalidEvent,
			maxNotificationID)
	}
	now := b.clock.Now()
	key := notificationKey(notificationID)
	claim, err := b.store.ClaimKey(ctx, bot, key, now, now.Add(keyClaimTTL))
	if err != nil {
		return fmt.Errorf("bridge: cannot read the notification: %w", err)
	}
	switch claim.State {
	case KeyClaimed:
		// Nothing is kept for the id: the claim was only the lookup.
		if err := b.store.ReleaseKey(context.WithoutCancel(ctx), bot, key, claim.ID); err != nil {
			return fmt.Errorf("bridge: cannot read the notification: %w", err)
		}
		return ErrUnknownNotification
	case KeyAnswered:
	default:
		return ErrUnknownNotification
	}
	var record notificationRecord
	if err := json.Unmarshal(claim.Answer.Body, &record); err != nil {
		return fmt.Errorf("bridge: the stored notification is not the JSON it was kept as: %w", err)
	}
	if record.Data == "" {
		return ErrNoAction
	}
	ev := NotificationAcknowledgedEvent{Type: string(EventTypeNotificationAcknowledged),
		NotificationId: notificationID, Data: record.Data}
	return b.report(ctx, bot, EventTypeNotificationAcknowledged, notificationID, time.Time{}, "",
		func(id string, at time.Time) any {
			ev.Id, ev.OccurredAt = id, at
			return ev
		})
}
