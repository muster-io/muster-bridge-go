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
	pushTimeout      = 10 * time.Second
	pushRetryFor     = time.Hour
	pushBackoffFirst = time.Second
	pushBackoffMax   = 5 * time.Minute
	pushDrainBytes   = 64 << 10
	userAgent        = "muster-bridge-go"
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
		Timeout:   pushTimeout + 5*time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// startPusher starts the bot's pusher unless it runs, and reports whether it started it; b.mu is held.
func (b *Bridge) startPusher(bs *botState) bool {
	if bs.pushing || b.closed {
		return false
	}
	bs.pushing = true
	b.pushers.Add(1)
	go func() {
		defer b.pushers.Done()
		b.push(bs)
	}()
	return true
}

func (bs *botState) pushTarget() *pushTarget {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.target
}

// push posts the bot's events to its target, one at a time and in order, from the oldest kept, until Close.
func (b *Bridge) push(bs *botState) {
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
		changed := bs.changes()
		events, next, err := b.store.EventsAfter(ctx, bs.name, cursor, 1, b.clock.Now())
		switch {
		case errors.Is(err, ErrUnknownCursor) && cursor != "":
			b.log.LogAttrs(ctx, slog.LevelWarn, "bridge: the store forgot the push cursor, pushing every kept event",
				slog.String("bot", bs.name))
			cursor = ""
		case err != nil:
			b.log.LogAttrs(ctx, slog.LevelError, "bridge: the store failed", slog.String("operation", "push"),
				slog.String("bot", bs.name), slog.String("error", b.redact(err.Error())))
			b.wait(ctx, bs, pushBackoffFirst)
		case len(events) == 0:
			cursor = next
			select {
			case <-changed:
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

// deliver pushes ev until Muster acknowledges or drops it, or for at least pushRetryFor, with exponential back-off
// under the same webhook-id. It reports false when it stopped without an outcome: no target any more, or Close.
func (b *Bridge) deliver(ctx context.Context, bs *botState, ev StoredEvent) bool {
	first := b.clock.Now()
	delay := pushBackoffFirst
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
		if b.clock.Now().Sub(first) >= pushRetryFor {
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
			first, delay, attempt = b.clock.Now(), pushBackoffFirst, 1
			continue
		}
		delay = min(2*delay, pushBackoffMax)
		attempt++
	}
}

// attempt posts ev once, signed as Standard Webhooks specifies, and returns the answer's status. The clock bounds it
// with pushTimeout.
func (b *Bridge) attempt(ctx context.Context, target *pushTarget, ev StoredEvent) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timeout, stop := b.clock.Timer(pushTimeout)
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
	timestamp := strconv.FormatInt(b.clock.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(ev.Body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("webhook-id", ev.ID)
	req.Header.Set("webhook-timestamp", timestamp)
	req.Header.Set("webhook-signature", "v1,"+sign(target.key, ev.ID, timestamp, ev.Body))
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, pushDrainBytes))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// sign is the base64 HMAC-SHA256 of "<id>.<timestamp>.<body>" with the key.
func sign(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
