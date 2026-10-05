package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"halfway/internal/store"
)

func TestOnlyDiscordWebhooksAreAccepted(t *testing.T) {
	c := New()
	good := map[string]string{
		"https://discord.com/api/webhooks/123/abc-DEF":            "https://discord.com/api/webhooks/123/abc-DEF",
		"  https://discordapp.com/api/webhooks/123/abc?wait=1#x ": "https://discordapp.com/api/webhooks/123/abc",
		"https://canary.discord.com/api/webhooks/1/t":             "https://canary.discord.com/api/webhooks/1/t",
	}
	for in, want := range good {
		if got, err := c.Clean(in); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"http://discord.com/api/webhooks/123/abc",           // not HTTPS
		"https://discord.com.evil.example/api/webhooks/1/2", // someone else's host
		"https://evil.example/api/webhooks/1/2",
		"https://discord.com:8443/api/webhooks/1/2", // another port
		"https://user:pw@discord.com/api/webhooks/1/2",
		"https://discord.com/api/channels/1/messages",
		"https://discord.com/api/webhooks/123",
		"https://discord.com/api/webhooks/123/abc/github",
		"not a url",
		"",
	} {
		if _, err := c.Clean(bad); err == nil {
			t.Errorf("Clean(%q) accepted it", bad)
		}
	}
}

// fakeDiscord stands in for Discord's webhooks: 1/ok exists, 2/gone does not.
type fakeDiscord struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/webhooks/1/ok" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == "GET" {
		json.NewEncoder(w).Encode(map[string]string{"name": "Halfway", "channel_id": "9"})
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.sent = append(f.sent, body)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func TestCheckSendAndDeliver(t *testing.T) {
	fake := &fakeDiscord{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	host, _ := url.Parse(srv.URL)
	c := NewWith(srv.Client(), func(u *url.URL) bool { return u.Host == host.Host && strings.HasPrefix(u.Path, "/api/webhooks/") })
	ctx := context.Background()
	ok, gone := srv.URL+"/api/webhooks/1/ok", srv.URL+"/api/webhooks/2/gone"

	if name, err := c.Check(ctx, ok); err != nil || name != "Halfway" {
		t.Errorf("check: %q %v", name, err)
	}
	if _, err := c.Check(ctx, gone); err != ErrRefused {
		t.Errorf("checking a deleted webhook: %v", err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _, _ := s.CreateUser("Anna")
	now := time.Now()
	p, _ := s.CreatePoll(u.ID, store.NewPoll{Title: "@everyone dinner", Slots: []string{now.Add(72 * time.Hour).Format(store.Stamp)}}, now)
	slots, _ := s.Slots(p.ID)
	s.AddChat(p.ID, Platform, ok, "Discord: Halfway")
	s.AddChat(p.ID, Platform, gone, "Discord: old")
	s.AddChat(p.ID, Platform, "https://evil.example/api/webhooks/1/2", "planted")
	s.Pick(p.ID, slots[0].ID)

	c.Deliver(ctx, s)
	c.Deliver(ctx, s) // nothing twice
	if len(fake.sent) != 1 {
		t.Fatalf("sent %d messages", len(fake.sent))
	}
	msg := fake.sent[0]
	if !strings.HasPrefix(msg["content"].(string), "✅ @everyone dinner is on: ") {
		t.Errorf("content %q", msg["content"])
	}
	if am, _ := msg["allowed_mentions"].(map[string]any); am == nil || len(am["parse"].([]any)) != 0 {
		t.Errorf("mentions were not switched off: %v", msg["allowed_mentions"])
	}
	chats, _ := s.Chats(p.ID)
	for _, ch := range chats {
		if (ch.Title != "Discord: Halfway") != ch.Broken {
			t.Errorf("%s: broken=%v", ch.Title, ch.Broken)
		}
	}
}
