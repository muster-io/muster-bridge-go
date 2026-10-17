// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const (
	// attemptTimeout bounds one post of an event to the callback.
	attemptTimeout = 10 * time.Second
	// retryWindow is how long an event is retried before the pusher moves on and leaves it to polling.
	retryWindow = time.Hour
	// firstBackoff is the wait after the first failed attempt; each further wait doubles, up to maxBackoff.
	firstBackoff   = time.Second
	maxBackoff     = 5 * time.Minute
	pushDrainBytes = 64 << 10
	userAgent      = "muster-bridge-go"
)

// newPushClient is the HTTP client of the pushes: the proxy of the environment, as any Go client, and no redirects,
// since Muster's callback never redirects. Its timeout is a safety net behind the clock's per-attempt timeout.
func newPushClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	return &http.Client{
		Transport: transport.Clone(),
		Timeout:   attemptTimeout + 5*time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// launchPusher runs the bot's pusher in its own goroutine, unless one already runs or the bridge is closed, and
// reports whether it launched one; b.mu is held. A pusher lives until Close.
func (b *Bridge) launchPusher(bs *botState) bool {
	if bs.pusherRunning || b.closed {
		return false
	}
	bs.pusherRunning = true
	b.pusherGroup.Go(func() { b.runPusher(bs) })
	return true
}

func (bs *botState) pushTarget() *pushTarget {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.target
}

// runPusher posts the bot's events to its target, one at a time and in order, from the oldest kept, until Close.
func (b *Bridge) runPusher(bs *botState) {
	ctx := b.ctx
	cursor := ""
	for ctx.Err() == nil {
		target := bs.pushTarget()
		if target == nil {
			select {
			case <-bs.retarget:
			case <-ctx.Done():
			}
			continue
		}
		arrived := bs.eventSignal()
		events, next, err := b.store.EventsAfter(ctx, bs.name, cursor, 1, b.clock.Now())
		switch {
		case errors.Is(err, ErrUnknownCursor) && cursor != "":
			b.log.LogAttrs(ctx, slog.LevelWarn, "bridge: the store forgot the push cursor, pushing every kept event",
				slog.String("bot", bs.name))
			cursor = ""
		case err != nil:
			b.log.LogAttrs(ctx, slog.LevelError, "bridge: the store failed", slog.String("operation", "push"),
				slog.String("bot", bs.name), slog.String("error", b.redact(err.Error())))
			b.wait(ctx, bs, firstBackoff)
		case len(events) == 0:
			cursor = next
			select {
			case <-arrived:
			case <-bs.retarget:
			case <-ctx.Done():
			}
		default:
			if b.deliver(ctx, bs, events[0]) {
				cursor = next
			}
		}
	}
}

// wait waits d, a retarget or the end of ctx, and reports whether d passed.
func (b *Bridge) wait(ctx context.Context, bs *botState, d time.Duration) bool {
	timer, stop := b.clock.Timer(d)
	defer stop()
	select {
	case <-timer:
		return true
	case <-bs.retarget:
	case <-ctx.Done():
	}
	return false
}

// deliver pushes ev until Muster acknowledges or drops it, or for at least retryWindow, with exponential back-off
// under the same webhook-id. It reports false when it stopped without an outcome: no target any more, or Close.
func (b *Bridge) deliver(ctx context.Context, bs *botState, ev StoredEvent) bool {
	first := b.clock.Now()
	delay := firstBackoff
	attempt := 1
	for {
		target := bs.pushTarget()
		if target == nil {
			return false
		}
		status, err := b.attempt(ctx, target, ev)
		if ctx.Err() != nil {
			return false
		}
		switch {
		case err == nil && status >= 200 && status < 300:
			return true
		case err == nil && status == http.StatusBadRequest:
			b.log.LogAttrs(ctx, slog.LevelWarn, "bridge: Muster refused a pushed event for good",
				slog.String("bot", bs.name), slog.String("event_id", ev.ID))
			return true
		}
		if b.clock.Now().Sub(first) >= retryWindow {
			b.log.LogAttrs(ctx, slog.LevelWarn, "bridge: gave up pushing an event, it stays for polling",
				slog.String("bot", bs.name), slog.String("event_id", ev.ID), slog.Int("attempts", attempt))
			return true
		}
		level := slog.LevelDebug
		if attempt == 1 {
			level = slog.LevelWarn
		}
		outcome := slog.Int("status", status)
		if err != nil {
			outcome = slog.String("error", b.redact(err.Error()))
		}
		b.log.LogAttrs(ctx, level, "bridge: a push failed, retrying", slog.String("bot", bs.name),
			slog.String("event_id", ev.ID), slog.Int("attempt", attempt), outcome)
		if !b.wait(ctx, bs, delay) {
			if ctx.Err() != nil {
				return false
			}
			first, delay, attempt = b.clock.Now(), firstBackoff, 1
			continue
		}
		delay = min(2*delay, maxBackoff)
		attempt++
	}
}

// attempt posts ev once, signed as Standard Webhooks specifies, and returns the answer's status. A timer of the
// library's clock cancels it after attemptTimeout.
func (b *Bridge) attempt(ctx context.Context, target *pushTarget, ev StoredEvent) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timeout, stop := b.clock.Timer(attemptTimeout)
	defer stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-timeout:
			cancel()
		case <-done:
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(ev.Body))
	if err != nil {
		return 0, err
	}
	sentAt := strconv.FormatInt(b.clock.Now().Unix(), 10)
	for name, value := range map[string]string{
		"Content-Type":      "application/json",
		"User-Agent":        userAgent,
		"webhook-id":        ev.ID,
		"webhook-timestamp": sentAt,
		"webhook-signature": webhookSignature(target.key, ev.ID, sentAt, ev.Body),
	} {
		req.Header.Set(name, value)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Only the status counts; reading a little of the body lets the client reuse the connection.
	_, _ = io.CopyN(io.Discard, resp.Body, pushDrainBytes)
	return resp.StatusCode, nil
}

// webhookSignature is the webhook-signature header of a Standard Webhooks message: the version "v1", a comma, and
// the standard base64 of the HMAC-SHA256 that secretKey makes of the signed content, which is the message id, the
// timestamp and the payload, separated by full stops.
func webhookSignature(secretKey []byte, msgID, sentAt string, payload []byte) string {
	signed := make([]byte, 0, len(msgID)+len(sentAt)+len(payload)+2)
	signed = append(signed, msgID...)
	signed = append(signed, '.')
	signed = append(signed, sentAt...)
	signed = append(signed, '.')
	signed = append(signed, payload...)
	h := hmac.New(sha256.New, secretKey)
	_, _ = h.Write(signed)
	return "v1," + base64.StdEncoding.EncodeToString(h.Sum(nil))
}
