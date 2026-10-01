package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"halfway/internal/store"
)

// fakeAPI stands in for Telegram's Bot API.
type fakeAPI struct {
	mu      sync.Mutex
	updates []map[string]any // handed out by the next getUpdates
	sent    []map[string]any
	// refuse makes sendMessage to a chat fail with this error.
	refuse map[int64]map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var params map[string]any
	json.NewDecoder(r.Body).Decode(&params)
	if !strings.HasPrefix(r.URL.Path, "/botSECRET/") {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	reply := func(result any) { json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}) }
	switch strings.TrimPrefix(r.URL.Path, "/botSECRET/") {
	case "getMe":
		reply(map[string]any{"id": 1, "username": "HalfwayTestBot"})
	case "getUpdates":
		u := f.updates
		f.updates = nil
		if len(u) == 0 {
			// The real one waits for news; don't let Listen spin.
			f.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			f.mu.Lock()
			u = []map[string]any{}
		}
		reply(u)
	case "sendMessage":
		chat := int64(params["chat_id"].(float64))
		if e, ok := f.refuse[chat]; ok {
			w.WriteHeader(int(e["error_code"].(int)))
			json.NewEncoder(w).Encode(e)
			return
		}
		f.sent = append(f.sent, params)
		reply(map[string]any{"message_id": len(f.sent)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAPI) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		out = append(out, m["text"].(string))
	}
	return out
}

func setup(t *testing.T) (*Bot, *fakeAPI, *store.Store, store.Poll, []store.Slot) {
	t.Helper()
	api := &fakeAPI{refuse: map[int64]map[string]any{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	b := New("SECRET", srv.URL)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	u, _, _ := s.CreateUser("Anna")
	now := time.Now()
	p, err := s.CreatePoll(u.ID, store.NewPoll{Title: "Friday dinner", Slots: []string{
		now.Add(48 * time.Hour).Format(store.Stamp), now.Add(72 * time.Hour).Format(store.Stamp),
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	slots, _ := s.Slots(p.ID)
	return b, api, s, p, slots
}

func groupStart(chatID int64, text string) map[string]any {
	return map[string]any{"update_id": 7, "message": map[string]any{
		"chat": map[string]any{"id": chatID, "type": "group", "title": "Dinner club"}, "text": text,
	}}
}

func TestStartLearnsTheUsernameAndRejectsABadToken(t *testing.T) {
	b, _, _, p, _ := setup(t)
	if b.Username != "HalfwayTestBot" {
		t.Errorf("username %q", b.Username)
	}
	if got := b.AddLink(p.ChatCode); got != "https://t.me/HalfwayTestBot?startgroup="+p.ChatCode {
		t.Errorf("link %q", got)
	}
	bad := New("WRONG", b.api)
	err := bad.Start(context.Background())
	if err == nil {
		t.Fatal("a wrong token was accepted")
	}
	if strings.Contains(err.Error(), "WRONG") {
		t.Errorf("the error gives the token away: %v", err)
	}
}

func TestAGroupConnectsItselfWithTheCode(t *testing.T) {
	b, api, s, p, _ := setup(t)
	ctx := context.Background()
	b.handle(ctx, s, update{})
	for _, u := range []map[string]any{
		groupStart(-1, "/start@SomeOtherBot "+p.ChatCode), // not for us
		groupStart(-2, "/start@halfwaytestbot "+p.ChatCode),
		groupStart(-3, "/start not-a-code"),
		groupStart(-4, "hello everyone"),
	} {
		var up update
		raw, _ := json.Marshal(u)
		json.Unmarshal(raw, &up)
		b.handle(ctx, s, up)
	}
	chats, _ := s.Chats(p.ID)
	if len(chats) != 1 || chats[0].Target != "-2" || chats[0].Title != "Dinner club" {
		t.Fatalf("got %+v", chats)
	}
	texts := api.texts()
	if len(texts) != 2 || !strings.Contains(texts[0], "connected to “Friday dinner”") || !strings.Contains(texts[1], "isn't for any poll") {
		t.Errorf("replies: %q", texts)
	}
}

func TestDeliverPostsEachDecisionOnceAndNoticesBrokenChats(t *testing.T) {
	b, api, s, p, slots := setup(t)
	ctx := context.Background()
	s.LinkChat(p.ChatCode, Platform, "-10", "A")
	s.LinkChat(p.ChatCode, Platform, "-20", "B")
	s.LinkChat(p.ChatCode, Platform, "-30", "C")
	api.refuse[-20] = map[string]any{"ok": false, "error_code": 403, "description": "Forbidden: bot was kicked from the group chat"}
	api.refuse[-30] = map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: group chat was upgraded to a supergroup chat",
		"parameters": map[string]any{"migrate_to_chat_id": -1000030}}

	s.Pick(p.ID, slots[0].ID)
	b.Deliver(ctx, s)
	b.Deliver(ctx, s) // the supergroup gets it now, and nobody gets it twice
	texts := api.texts()
	if len(texts) != 2 || texts[0] != texts[1] || !strings.HasPrefix(texts[0], "✅ Friday dinner is on: ") {
		t.Fatalf("sent %q", texts)
	}
	chats, _ := s.Chats(p.ID)
	byTitle := map[string]store.Chat{}
	for _, c := range chats {
		byTitle[c.Title] = c
	}
	if !byTitle["B"].Broken || byTitle["A"].Broken {
		t.Errorf("chats: %+v", chats)
	}
	if byTitle["C"].Target != "-1000030" {
		t.Errorf("the supergroup was not followed: %+v", byTitle["C"])
	}
}

func TestLeavingAGroupBreaksItsConnection(t *testing.T) {
	b, _, s, p, _ := setup(t)
	s.LinkChat(p.ChatCode, Platform, "-5", "A")
	var up update
	raw, _ := json.Marshal(map[string]any{"update_id": 1, "my_chat_member": map[string]any{
		"chat": map[string]any{"id": -5, "type": "group"}, "new_chat_member": map[string]any{"status": "kicked"},
	}})
	json.Unmarshal(raw, &up)
	b.handle(context.Background(), s, up)
	if chats, _ := s.Chats(p.ID); !chats[0].Broken {
		t.Error("still connected after the bot was thrown out")
	}
}

func TestListenReadsUpdatesUntilStopped(t *testing.T) {
	b, api, s, p, _ := setup(t)
	api.updates = []map[string]any{groupStart(-9, "/start "+p.ChatCode)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Listen(ctx, s); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if chats, _ := s.Chats(p.ID); len(chats) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the group never got connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not stop")
	}
}
