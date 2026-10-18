// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package main

import (
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	bridge "github.com/muster-io/muster-bridge-go"
)

// The most the chat keeps; past them it forgets the oldest, which the protocol then finds gone.
const (
	maxRoots   = 1000 // Root posts per channel
	maxThread  = 500  // posts per Thread
	maxNotes   = 100  // notes per post
	maxDirect  = 500  // entries per direct chat
	maxJournal = 500  // entries of the event log
	stateLog   = 100  // entries of the event log the page is given
)

var (
	errNoChannel  = errors.New("the bot is not in this channel")
	errNoPost     = errors.New("no message has this id, or it was deleted")
	errEditWindow = errors.New("the message is past its edit window")
	errNoPerson   = errors.New("no person has this login")
	errNoButton   = errors.New("the message has no such button")
)

// post is a message of the chat as the page shows it: a Root post of the bot, a post in its Thread, or a tombstone.
type post struct {
	ID       string             `json:"id"`
	Channel  string             `json:"channel"`
	Root     string             `json:"root,omitempty"`
	Bot      bool               `json:"bot"`
	Author   string             `json:"author,omitempty"`
	Kind     bridge.MessageKind `json:"kind,omitempty"`
	Status   bridge.Status      `json:"status,omitempty"`
	Color    string             `json:"color"`
	Emoji    string             `json:"emoji"`
	Title    string             `json:"title"`
	Link     pageLink           `json:"link"`
	Body     string             `json:"body,omitempty"`
	Text     string             `json:"text,omitempty"`
	HTML     string             `json:"html"`
	Buttons  []pageButton       `json:"buttons"`
	Quiet    bool               `json:"quiet"`
	PostedAt time.Time          `json:"posted_at"`
	EditedAt *time.Time         `json:"edited_at"`
	Edits    int                `json:"edits"`
	Deleted  bool               `json:"deleted"`
	Thread   []*post            `json:"thread,omitzero"`
	Notes    []note             `json:"notes"`
	// data holds each button's data, which only a press carries back and the page never sees.
	data []string
}

type pageLink struct {
	URL   string `json:"url"`
	Label string `json:"label"`
}

type pageButton struct {
	Label string             `json:"label"`
	Style bridge.ButtonStyle `json:"style"`
}

// note is an answer shown under a post to one person only.
type note struct {
	For  string    `json:"for"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// directEntry is a message of a direct chat between a person and the bot: Body for the bot's direct messages,
// Text for a person's message and for an answer.
type directEntry struct {
	ID     string    `json:"id"`
	Bot    bool      `json:"bot"`
	Author string    `json:"author,omitempty"`
	Body   string    `json:"body,omitempty"`
	Text   string    `json:"text,omitempty"`
	HTML   string    `json:"html"`
	At     time.Time `json:"at"`
}

// logEntry is one row of the page's event log. Kind is "call" for a request Muster made under /v1/ and "report" for
// something the page told the library; a run of identical rows folds into one row whose Count grows.
type logEntry struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	What    string    `json:"what"`
	Outcome string    `json:"outcome"`
	Count   int       `json:"count"`
}

type chatState struct {
	Channels []channelView `json:"channels"`
	People   []personView  `json:"people"`
	Version  uint64        `json:"version"`
	Log      []logEntry    `json:"log"`
}

type channelView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type personView struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

// placed is where a post in a Thread went.
type placed struct {
	id, root, channel string
}

// pressed is what a press of a button reports.
type pressed struct {
	channel, data, label, updateID string
}

// chat is the messenger: channels, people, posts, direct chats and the event log, in memory. Ids and update ids start
// with the prefix of this start, so that a restarted chat never repeats one. It hands out copies only.
type chat struct {
	now      func() time.Time
	prefix   string
	channels []string
	people   []bridge.Person

	mu      sync.Mutex
	seq     int
	updates int
	// version moves when a post, a note or a direct chat changes, and never for the event log, which every state
	// carries anyway; the page fetches the messages again only when it moved.
	version uint64
	posts   map[string]*post
	roots   map[string][]*post
	directs map[string][]directEntry
	journal []logEntry
	logSeq  int
}

func newChat(channels []string, people []bridge.Person, now func() time.Time, prefix string) *chat {
	return &chat{now: now, prefix: prefix, channels: slices.Clone(channels), people: slices.Clone(people),
		posts: map[string]*post{}, roots: map[string][]*post{}, directs: map[string][]directEntry{}}
}

func (c *chat) hasChannel(id string) bool { return slices.Contains(c.channels, id) }

func (c *chat) person(login string) (bridge.Person, bool) {
	for _, p := range c.people {
		if p.ExternalId == login {
			if p.Handle != nil {
				p.Handle = new(*p.Handle)
			}
			return p, true
		}
	}
	return bridge.Person{}, false
}

// postRoot posts p as a Root post in p.Channel.
func (c *chat) postRoot(p post) (string, error) {
	if !c.hasChannel(p.Channel) {
		return "", errNoChannel
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	np := c.fresh(p)
	np.Thread = []*post{}
	c.posts[np.ID] = np
	c.roots[np.Channel] = append(c.roots[np.Channel], np)
	if list := c.roots[np.Channel]; len(list) > maxRoots {
		delete(c.posts, list[0].ID)
		for _, t := range list[0].Thread {
			delete(c.posts, t.ID)
		}
		c.roots[np.Channel] = slices.Delete(list, 0, 1)
	}
	c.version++
	return np.ID, nil
}

// edit replaces the content of a post of the bot, unless it is older than maxAge; 0 is no limit.
func (c *chat) edit(id string, p post, maxAge time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.live(id)
	if old == nil || !old.Bot {
		return errNoPost
	}
	now := c.now()
	if maxAge > 0 && now.Sub(old.PostedAt) > maxAge {
		return errEditWindow
	}
	old.Status, old.Color, old.Emoji, old.Title, old.Link = p.Status, p.Color, p.Emoji, p.Title, p.Link
	old.Body, old.Text, old.HTML = p.Body, p.Text, p.HTML
	old.Buttons, old.data = slices.Clone(p.Buttons), slices.Clone(p.data)
	old.EditedAt = &now
	old.Edits++
	c.version++
	return nil
}

// rootOf is the Root post of the Thread a live post belongs to, or the post itself when it is a Root post.
func (c *chat) rootOf(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	root := c.threadRoot(id)
	if root == nil {
		return "", errNoPost
	}
	return root.ID, nil
}

// postInThread posts p in the Thread of parent, a Root post or a post in its Thread; with channel set, the Thread must
// be in it.
func (c *chat) postInThread(parentID, channel string, p post) (placed, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	root := c.threadRoot(parentID)
	if root == nil || channel != "" && root.Channel != channel {
		return placed{}, errNoPost
	}
	np := c.fresh(p)
	np.Root, np.Channel = root.ID, root.Channel
	c.posts[np.ID] = np
	root.Thread = append(root.Thread, np)
	if len(root.Thread) > maxThread {
		delete(c.posts, root.Thread[0].ID)
		root.Thread = slices.Delete(root.Thread, 0, 1)
	}
	c.version++
	return placed{id: np.ID, root: root.ID, channel: root.Channel}, nil
}

// remove turns a live post into a tombstone and reports whether it was the bot's; with botOnly, a person's post is
// not found.
func (c *chat) remove(id string, botOnly bool) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.live(id)
	if p == nil || botOnly && !p.Bot {
		return false, errNoPost
	}
	*p = post{ID: p.ID, Channel: p.Channel, Root: p.Root, Bot: p.Bot, Author: p.Author, Kind: p.Kind,
		PostedAt: p.PostedAt, Thread: p.Thread, Deleted: true}
	c.version++
	return p.Bot, nil
}

// addNote shows text under a live post of the channel to the person with the login only.
func (c *chat) addNote(postID, channel, login, text string) error {
	if _, ok := c.person(login); !ok {
		return errNoPerson
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.live(postID)
	if p == nil || p.Channel != channel {
		return errNoPost
	}
	p.Notes = append(p.Notes, note{For: login, Text: text, At: c.now()})
	if len(p.Notes) > maxNotes {
		p.Notes = slices.Delete(p.Notes, 0, 1)
	}
	c.version++
	return nil
}

// addDirect adds e to the direct chat between the person and the bot.
func (c *chat) addDirect(login string, e directEntry) (string, error) {
	if _, ok := c.person(login); !ok {
		return "", errNoPerson
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e.ID, e.At = c.nextID(), c.now()
	c.directs[login] = append(c.directs[login], e)
	if list := c.directs[login]; len(list) > maxDirect {
		c.directs[login] = slices.Delete(list, 0, 1)
	}
	c.version++
	return e.ID, nil
}

// pressTarget is what a press of the button at the index of a live post of the bot reports, with a new update id.
func (c *chat) pressTarget(postID string, button int) (pressed, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.live(postID)
	if p == nil {
		return pressed{}, errNoPost
	}
	if button < 0 || button >= len(p.data) {
		return pressed{}, errNoButton
	}
	return pressed{channel: p.Channel, data: p.data[button], label: p.Buttons[button].Label,
		updateID: c.nextUpdateLocked()}, nil
}

// nextUpdate is a new id of an update of the page, as the messenger's own update ids are.
func (c *chat) nextUpdate() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextUpdateLocked()
}

// record adds a line to the event log, or counts it in the newest line when that one is equal; version stays.
func (c *chat) record(kind, what, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if n := len(c.journal); n > 0 {
		if last := &c.journal[n-1]; last.Kind == kind && last.What == what && last.Outcome == outcome {
			last.Count++
			last.At = now
			return
		}
	}
	c.logSeq++
	c.journal = append(c.journal, logEntry{Seq: c.logSeq, At: now, Kind: kind, What: what, Outcome: outcome,
		Count: 1})
	if len(c.journal) > maxJournal {
		c.journal = slices.Delete(c.journal, 0, 1)
	}
}

// channelPosts are the Root posts of the channel with their Threads, oldest first.
func (c *chat) channelPosts(id string) ([]post, bool) {
	if !c.hasChannel(id) {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.roots[id]
	out := make([]post, 0, len(list))
	for _, p := range list {
		out = append(out, p.copy())
	}
	return out, true
}

// directChat is the direct chat between the person and the bot, oldest first.
func (c *chat) directChat(login string) ([]directEntry, bool) {
	if _, ok := c.person(login); !ok {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]directEntry{}, c.directs[login]...), true
}

// state is the channels, the people, the version and the newest lines of the event log, newest first.
func (c *chat) state() chatState {
	s := chatState{Channels: make([]channelView, 0, len(c.channels)), People: make([]personView, 0, len(c.people))}
	for _, id := range c.channels {
		s.Channels = append(s.Channels, channelView{ID: id, Name: "#" + id})
	}
	for _, p := range c.people {
		s.People = append(s.People, personView{Login: p.ExternalId, Name: p.DisplayName})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s.Version = c.version
	s.Log = make([]logEntry, 0, min(len(c.journal), stateLog))
	for i := len(c.journal) - 1; i >= 0 && len(s.Log) < stateLog; i-- {
		s.Log = append(s.Log, c.journal[i])
	}
	return s
}

// fresh is a new post with p's content, its own id and the time now; c.mu is held.
func (c *chat) fresh(p post) *post {
	np := &post{ID: c.nextID(), Channel: p.Channel, Bot: p.Bot, Author: p.Author, Kind: p.Kind, Status: p.Status,
		Color: p.Color, Emoji: p.Emoji, Title: p.Title, Link: p.Link, Body: p.Body, Text: p.Text, HTML: p.HTML,
		Buttons: slices.Clone(p.Buttons), Quiet: p.Quiet, PostedAt: c.now(), data: slices.Clone(p.data)}
	return np
}

// live is the post with the id unless it was deleted or forgotten; c.mu is held.
func (c *chat) live(id string) *post {
	if p := c.posts[id]; p != nil && !p.Deleted {
		return p
	}
	return nil
}

// threadRoot is the live Root post of the live post with the id; c.mu is held.
func (c *chat) threadRoot(id string) *post {
	p := c.live(id)
	if p == nil || p.Root == "" {
		return p
	}
	return c.live(p.Root)
}

func (c *chat) nextID() string {
	c.seq++
	return c.prefix + "-" + strconv.Itoa(c.seq)
}

func (c *chat) nextUpdateLocked() string {
	c.updates++
	return c.prefix + "-u" + strconv.Itoa(c.updates)
}

// copy is a deep copy of the post for the page, its Thread included, without the buttons' data.
func (p *post) copy() post {
	cp := *p
	cp.data = nil
	cp.Buttons = append([]pageButton{}, p.Buttons...)
	cp.Notes = append([]note{}, p.Notes...)
	if p.EditedAt != nil {
		cp.EditedAt = new(*p.EditedAt)
	}
	cp.Thread = nil
	if p.Root == "" {
		cp.Thread = make([]*post, 0, len(p.Thread))
		for _, t := range p.Thread {
			tc := t.copy()
			cp.Thread = append(cp.Thread, &tc)
		}
	}
	return cp
}
