// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Command chat is the example bridge: a small chat with its own web page — channels, Threads, buttons, direct chats
// between a person and the bot, a picker of who the viewer is, and an event log — served on one listener beside the
// Bridge protocol, so that Muster can be tried, and tested, with a messenger that runs anywhere.
//
//	printf example-token > /tmp/bt
//	go run ./examples/chat -token-file /tmp/bt
//
// Then open http://127.0.0.1:18095/. It offers every feature of the protocol, or with BRIDGE_FEATURES only those it
// lists, and keeps everything in memory. The page and its JSON API under /chat/api/ have no authentication.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

// version is set with -ldflags "-X main.version=…"; empty, it is the module's version from the build information.
var version string

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// settings are the flags and the environment, checked.
type settings struct {
	listen        string
	token         string
	push          *bridge.PushTarget
	channels      []string
	people        []bridge.Person
	editMaxAge    int
	maxBodyLength int
	features      features
	version       string
}

// flagError is a mistake on the command line, which the help explains.
type flagError struct{ err error }

func (e flagError) Error() string { return e.err.Error() }

// run runs the example with the arguments and the environment and returns the exit status: 0 after -help or a stop
// by a signal, 1 when serving failed, 2 for wrong settings.
func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	s, help, err := loadSettings(args, lookupEnv, stdout)
	if help {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "example-bridge: %v\n", err)
		if _, ok := errors.AsType[flagError](err); ok {
			_, _ = fmt.Fprintln(stderr, "Run example-bridge -help for the flags.")
		}
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	c := newChat(s.channels, s.people, time.Now, strings.ToLower(rand.Text()[:6]))
	m := &messenger{chat: c, on: s.features, editMaxAge: time.Duration(s.editMaxAge) * time.Second}
	b, err := bridge.New(bridgeConfig(s, log), m)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "example-bridge: the configuration: %v\n", err)
		return 2
	}
	if err := serve(ctx, s, b, c, log); err != nil {
		log.Error("example-bridge: stopped", "error", err.Error())
		return 1
	}
	return 0
}

// loadSettings reads the flags and the environment; with -help it writes the usage to stdout and reports help.
func loadSettings(args []string, lookupEnv func(string) (string, bool), stdout io.Writer) (settings, bool, error) {
	fs := flag.NewFlagSet("example-bridge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen", "127.0.0.1:18095",
		"the address of the page and the Bridge protocol (the image passes -listen :8095)")
	tokenFile := fs.String("token-file", "", "the file that holds the bot's token, instead of BRIDGE_TOKEN")
	secretFile := fs.String("push-secret-file", "",
		"the file that holds the push secret (whsec_…), instead of BRIDGE_PUSH_SECRET")
	channels := fs.String("channels", "alerts,ops",
		"the channels the bot is in, comma-separated; any other channel is forbidden")
	users := fs.String("users", "alice:Alice Smith,bob:Bob Brown",
		"the people, as login:display name, comma-separated")
	editMaxAge := fs.Int("edit-max-age", 0, "seconds after posting during which a message can be edited "+
		"(limits.edit_max_age_seconds); 0 for no limit, else at least 300")
	maxBodyLength := fs.Int("max-body-length", 4000,
		"the longest body in code points (limits.max_body_length), 500 to 100000")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout, fs)
			return settings{}, true, nil
		}
		return settings{}, false, flagError{err}
	}
	if fs.NArg() > 0 {
		return settings{}, false, flagError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}

	s := settings{listen: *listen, editMaxAge: *editMaxAge, maxBodyLength: *maxBodyLength, version: bridgeVersion()}
	env := func(name string) string {
		v, _ := lookupEnv(name)
		return strings.TrimSpace(v)
	}
	var err error
	if s.token, err = secret("BRIDGE_TOKEN", env("BRIDGE_TOKEN"), "-token-file", *tokenFile, "token"); err != nil {
		return settings{}, false, err
	}
	if s.token == "" {
		return settings{}, false, errors.New("the token is missing: set BRIDGE_TOKEN or -token-file")
	}
	pushURL := env("BRIDGE_PUSH_URL")
	pushSecret, err := secret("BRIDGE_PUSH_SECRET", env("BRIDGE_PUSH_SECRET"), "-push-secret-file", *secretFile,
		"push secret")
	switch {
	case err != nil:
		return settings{}, false, err
	case pushURL == "" && pushSecret != "":
		return settings{}, false, errors.New("BRIDGE_PUSH_SECRET and -push-secret-file need BRIDGE_PUSH_URL")
	case pushURL != "" && pushSecret == "":
		return settings{}, false, errors.New("BRIDGE_PUSH_URL needs BRIDGE_PUSH_SECRET or -push-secret-file")
	case pushURL != "":
		s.push = &bridge.PushTarget{URL: pushURL, Secret: pushSecret}
	}
	if s.channels, err = parseChannels(*channels); err != nil {
		return settings{}, false, err
	}
	if s.people, err = parseUsers(*users); err != nil {
		return settings{}, false, err
	}
	list, set := lookupEnv("BRIDGE_FEATURES")
	if s.features, err = parseFeatures(list, set); err != nil {
		return settings{}, false, err
	}
	return s, false, nil
}

// secret is the value of the variable or of the file the flag names, trimmed; "" when neither is set.
func secret(variable, value, flagName, path, what string) (string, error) {
	switch {
	case value != "" && path != "":
		return "", fmt.Errorf("set %s or %s, not both", variable, flagName)
	case path == "":
		return value, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flagName, err)
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("%s: it holds no %s", flagName, what)
	}
	return v, nil
}

func parseChannels(list string) ([]string, error) {
	var channels []string
	for id := range strings.SplitSeq(list, ",") {
		switch id = strings.TrimSpace(id); {
		case id == "":
		case slices.Contains(channels, id):
			return nil, fmt.Errorf("-channels: the channel %q appears twice", id)
		default:
			channels = append(channels, id)
		}
	}
	if len(channels) == 0 {
		return nil, errors.New("-channels: name at least one channel")
	}
	return channels, nil
}

func parseUsers(list string) ([]bridge.Person, error) {
	var people []bridge.Person
	for entry := range strings.SplitSeq(list, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		login, name, ok := strings.Cut(entry, ":")
		login, name = strings.TrimSpace(login), strings.TrimSpace(name)
		if !ok || login == "" || name == "" {
			return nil, fmt.Errorf("-users: %q is not login:display name", entry)
		}
		if slices.ContainsFunc(people, func(p bridge.Person) bool { return p.ExternalId == login }) {
			return nil, fmt.Errorf("-users: the login %q appears twice", login)
		}
		people = append(people, bridge.Person{ExternalId: login, DisplayName: name, Handle: new(login)})
	}
	if len(people) == 0 {
		return nil, errors.New("-users: name at least one person")
	}
	return people, nil
}

// usage writes the help: what the example is, its flags and its environment.
func usage(w io.Writer, fs *flag.FlagSet) {
	_, _ = fmt.Fprintf(w, `Usage: example-bridge [flags]
%s %s: an in-memory chat with its web page at / and the Bridge protocol under /v1/.
The page and its JSON API under /chat/api/ have no authentication: put them behind some before exposing them.

Flags:
`, bridgeName, bridgeVersion())
	fs.SetOutput(w)
	fs.PrintDefaults()
	_, _ = fmt.Fprintf(w, `
Environment:
  BRIDGE_TOKEN        the bot's token, which Muster sends; or -token-file
  BRIDGE_PUSH_URL     the callback address of the Bridge Connection, to push the events to; unset, Muster polls
  BRIDGE_PUSH_SECRET  the push secret of the Bridge Connection, whsec_…; or -push-secret-file
  BRIDGE_FEATURES     the features to offer, comma-separated, all when unset: %s
`, strings.Join(featureNames, ", "))
}

// bridgeVersion is the version set at build time, else the module's version from the build information, else "devel".
func bridgeVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
}

func aboutOf(s settings) pageAbout {
	about := pageAbout{version: s.version, features: s.features, maxBodyLength: s.maxBodyLength, push: s.push != nil}
	if s.editMaxAge != 0 {
		about.editMaxAge = new(s.editMaxAge)
	}
	return about
}

// stopTimeout is how long each step of the stop may take: the server's shutdown, then the bridge's.
const stopTimeout = 5 * time.Second

// serve serves the page and the protocol until ctx ends or SIGINT or SIGTERM comes, then shuts the server down and
// stops the bridge's pusher.
func serve(ctx context.Context, s settings, b *bridge.Bridge, c *chat, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.listen)
	if err != nil {
		return err
	}
	srv := newServer(ctx, newMux(b, c, aboutOf(s), log))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("example-bridge: listening", "address", ln.Addr().String(), "version", s.version,
		"features", strings.Join(s.features.names(), ","), "push", s.push != nil)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	if err := within(ctx, stopTimeout, srv.Shutdown); err != nil {
		_ = srv.Close()
	}
	return within(ctx, stopTimeout, b.Close)
}

// newServer serves h with every request under ctx, so that the end of ctx also ends a long poll of /v1/events
// at once rather than holding the shutdown. It has no WriteTimeout: a long poll holds its answer up to 60 seconds, and
// a WriteTimeout shorter than that, plus Muster's margin, would cut it off.
func newServer(ctx context.Context, h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }}
}

// within runs one step of the stop under a deadline of its own, which the end of ctx does not cut short.
func within(ctx context.Context, d time.Duration, step func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
	defer cancel()
	return step(ctx)
}
