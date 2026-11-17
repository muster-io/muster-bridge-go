// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Command gateway is a bridge in the role gateway whose provider lives in memory: it writes every notification
// Muster sends to its log, as a phone call or an SMS would reach a person, so that Muster, muster bridge check or a
// person with curl can try the Bridge protocol's notifications without a provider.
//
//	printf example-token > /tmp/bt
//	go run ./examples/gateway -token-file /tmp/bt
//
// Outside the protocol it serves /demo/acknowledge, a page that lists the notifications that carry an action, each
// with a button that acknowledges it as the person would, by key press, and POST /demo/acknowledge
// {"notification_id"} for scripts. Unlike the protocol, which needs the token, it has no authentication of its own:
// the example listens on loopback by default.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

const botName = "main"

func main() {
	listen := flag.String("listen", "127.0.0.1:18095", "the address to listen on")
	tokenFile := flag.String("token-file", "", "the file that holds the bot's token (required)")
	pushURL := flag.String("push-url", "", "where to push the events, the callback URL of the Bridge Connection")
	pushSecretFile := flag.String("push-secret-file", "", "the file that holds the push secret, whsec_…")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, g, err := configure(*tokenFile, *pushURL, *pushSecretFile, log)
	if err == nil {
		var b *bridge.Bridge
		if b, err = bridge.New(cfg, nil); err == nil {
			err = serve(*listen, b, g, log)
		}
	}
	if err != nil {
		log.Error("gateway: stopped", "error", err)
		os.Exit(2)
	}
}

// configure builds the bridge's Config, whose only role is gateway, and its in-memory provider from the flags.
func configure(tokenFile, pushURL, pushSecretFile string, log *slog.Logger) (bridge.Config, *provider, error) {
	if tokenFile == "" {
		return bridge.Config{}, nil, errors.New("-token-file is required")
	}
	token, err := readSecret(tokenFile)
	if err != nil {
		return bridge.Config{}, nil, fmt.Errorf("-token-file: %w", err)
	}
	bot := bridge.Bot{Name: botName, Token: token}
	if (pushURL == "") != (pushSecretFile == "") {
		return bridge.Config{}, nil, errors.New("-push-url and -push-secret-file go together")
	}
	if pushURL != "" {
		secret, err := readSecret(pushSecretFile)
		if err != nil {
			return bridge.Config{}, nil, fmt.Errorf("-push-secret-file: %w", err)
		}
		bot.Push = &bridge.PushTarget{URL: pushURL, Secret: secret}
	}
	g := &provider{log: log, idPrefix: strings.ToLower(rand.Text()[:6]), sent: map[string]*notification{}}
	cfg := bridge.Config{
		Name:          "muster-bridge-go gateway example",
		Version:       "0.0.0",
		MessengerName: "Gateway example",
		Roles:         []bridge.Role{bridge.RoleGateway},
		Gateway:       g,
		Bots:          []bridge.Bot{bot},
		Limits:        bridge.Limits{MaxBodyLength: 1000},
		Logger:        log,
	}
	return cfg, g, nil
}

// readSecret returns the token or push secret a file holds, without the white space around it.
func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if secret := strings.TrimSpace(string(data)); secret != "" {
		return secret, nil
	}
	return "", errors.New("it holds no secret")
}

// serve serves the protocol and /demo/acknowledge until SIGINT or SIGTERM, then stops the server and the pushers.
func serve(addr string, b *bridge.Bridge, g *provider, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/v1/", b.Handler())
	mux.HandleFunc("GET /demo/acknowledge", g.page)
	mux.HandleFunc("POST /demo/acknowledge", func(w http.ResponseWriter, r *http.Request) { g.acknowledge(w, r, b) })

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// No WriteTimeout: a long poll of /v1/events holds its answer up to 60 seconds, and a WriteTimeout shorter than
	// that, plus Muster's margin, would cut it off. A real bridge either sets none or one above 75 seconds.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("gateway: listening", "address", ln.Addr().String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return b.Close(shutdown)
}

// notification is a notification the provider reached a person with.
type notification struct {
	ID      string
	Contact string
	Text    string
	// Label is how the person acknowledges, "" for a notification without an action.
	Label string
	Acked bool
	// Answer is what Muster had said to the person after the acknowledgement.
	Answer string
}

// provider is the in-memory provider: it implements bridge.Gateway. Every notification it keeps is also written to the
// log, which stands in for the person's phone.
type provider struct {
	log *slog.Logger
	// idPrefix starts the ids of this process's notifications, so that a restarted example never hands out an id
	// that Muster still holds.
	idPrefix string

	mu    sync.Mutex
	order []string
	sent  map[string]*notification
}

func (g *provider) SendNotification(_ context.Context, _ string, in bridge.NotificationInput) (bridge.NotificationSent,
	error,
) {
	if strings.TrimSpace(in.Contact) == "" {
		return bridge.NotificationSent{}, bridge.InvalidError("The contact is empty.")
	}
	g.mu.Lock()
	id := g.idPrefix + "-n" + strconv.Itoa(len(g.order)+1)
	n := &notification{ID: id, Contact: in.Contact, Text: in.Text}
	if in.Action != nil {
		n.Label = in.Action.Label
	}
	g.sent[id] = n
	g.order = append(g.order, id)
	g.mu.Unlock()
	g.log.Info("gateway: notification", "notification_id", id, "contact", in.Contact, "language", string(in.Language),
		"text", in.Text, "action", n.Label)
	return bridge.NotificationSent{NotificationId: id}, nil
}

func (g *provider) Answer(_ context.Context, _ string, ev bridge.ReportedEvent, text string) (bridge.AnswerShownShownAs,
	error,
) {
	g.mu.Lock()
	if n := g.sent[ev.NotificationID]; n != nil {
		n.Answer = text
	}
	g.mu.Unlock()
	g.log.Info("gateway: answer", "notification_id", ev.NotificationID, "text", text)
	return bridge.AnswerShownShownAsNotification, nil
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<meta charset="utf-8">
<title>Gateway example</title>
<h1>Notifications</h1>
{{range .}}<form method="post" action="/demo/acknowledge">
<p><b>{{.Contact}}</b>: {{.Text}}</p>
{{if .Label}}{{if .Acked}}<p>acknowledged{{if .Answer}}, Muster said: {{.Answer}}{{end}}</p>{{else}}<input type="hidden" name="notification_id" value="{{.ID}}"><button>{{.Label}}</button>{{end}}{{end}}
</form>
{{else}}<p>No notification yet.</p>
{{end}}`))

// page lists the notifications, the newest first, with a button for each that waits for an acknowledgement.
func (g *provider) page(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	list := make([]notification, 0, len(g.order))
	for i := len(g.order) - 1; i >= 0; i-- {
		list = append(list, *g.sent[g.order[i]])
	}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, list); err != nil {
		g.log.Error("gateway: the page could not be written", "error", err.Error())
	}
}

// acknowledge reports the acknowledgement of a notification, from the page's form or from a script's JSON.
func (g *provider) acknowledge(w http.ResponseWriter, r *http.Request, b *bridge.Bridge) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var id string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			NotificationID string `json:"notification_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "the body is not {\"notification_id\": …}", http.StatusBadRequest)
			return
		}
		id = req.NotificationID
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "the form could not be read", http.StatusBadRequest)
			return
		}
		id = r.PostForm.Get("notification_id")
	}
	g.mu.Lock()
	n := g.sent[id]
	g.mu.Unlock()
	if n == nil {
		http.Error(w, "no notification has the id "+strconv.Quote(id), http.StatusBadRequest)
		return
	}
	switch err := b.Acknowledge(r.Context(), botName, id); {
	case errors.Is(err, bridge.ErrNoAction):
		http.Error(w, "the notification has no action to acknowledge", http.StatusConflict)
	case errors.Is(err, bridge.ErrUnknownNotification):
		http.Error(w, "the bridge no longer keeps that notification", http.StatusGone)
	case err != nil:
		g.log.Error("gateway: the acknowledgement could not be reported", "error", err.Error())
		http.Error(w, "the acknowledgement could not be reported", http.StatusInternalServerError)
	default:
		g.mu.Lock()
		n.Acked = true
		g.mu.Unlock()
		g.log.Info("gateway: notification acknowledged", "notification_id", id, "contact", n.Contact)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/demo/acknowledge", http.StatusSeeOther)
	}
}
