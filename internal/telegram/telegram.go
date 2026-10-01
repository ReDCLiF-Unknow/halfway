// Package telegram posts Halfway's decisions into Telegram groups.
//
// A poll's organizer adds the bot to a group with a link that carries the
// poll's connect code (t.me/<bot>?startgroup=<code>). Telegram then sends the
// bot "/start <code>" from that group, which links the group to the poll.
// From then on the bot posts one message into it per decision. It never reads
// anything else, so the group's privacy mode can stay on.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"halfway/internal/store"
)

// Platform is how chats connected through this package are marked in the
// database.
const Platform = "telegram"

// DefaultAPI is Telegram's Bot API.
const DefaultAPI = "https://api.telegram.org"

type Bot struct {
	token string
	api   string
	http  *http.Client
	// Username is the bot's name without the @, known once Start has run.
	Username string
}

// New makes a bot that talks to api (DefaultAPI, or a stand-in in tests).
func New(token, api string) *Bot {
	if api == "" {
		api = DefaultAPI
	}
	// Longer than the 50 seconds getUpdates is asked to wait for news.
	return &Bot{token: token, api: strings.TrimRight(api, "/"), http: &http.Client{Timeout: 70 * time.Second}}
}

// apiError is what Telegram says when it turns a request down.
type apiError struct {
	Code        int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		MigrateTo  int64 `json:"migrate_to_chat_id"`
		RetryAfter int   `json:"retry_after"`
	} `json:"parameters"`
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Description) }

// call posts a method's parameters and decodes its result into out.
func (b *Bot) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", b.api+"/bot"+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		// The error names the URL, which holds the token: keep it out of logs.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("telegram: %s: could not reach the Bot API", method)
	}
	defer resp.Body.Close()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		apiError
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram: %s: HTTP %d", method, resp.StatusCode)
	}
	if !env.OK {
		e := env.apiError
		if e.Code == 0 {
			e.Code = resp.StatusCode
		}
		return &e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// Start checks the token and learns the bot's username, which the links that
// add it to a group are made of.
func (b *Bot) Start(ctx context.Context) error {
	var me struct {
		Username string `json:"username"`
	}
	if err := b.call(ctx, "getMe", struct{}{}, &me); err != nil {
		return err
	}
	if me.Username == "" {
		return errors.New("telegram: the bot has no username")
	}
	b.Username = me.Username
	return nil
}

// AddLink is the link that adds the bot to a group and connects the group to
// the poll whose connect code this is.
func (b *Bot) AddLink(code string) string {
	return "https://t.me/" + b.Username + "?startgroup=" + code
}

// Send posts text into a chat.
func (b *Bot) Send(ctx context.Context, chat, text string) error {
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return err
	}
	return b.call(ctx, "sendMessage", map[string]any{"chat_id": id, "text": text}, nil)
}

type chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type update struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		Chat      chat   `json:"chat"`
		Text      string `json:"text"`
		MigrateTo int64  `json:"migrate_to_chat_id"`
	} `json:"message"`
	Member *struct {
		Chat chat `json:"chat"`
		New  struct {
			Status string `json:"status"`
		} `json:"new_chat_member"`
	} `json:"my_chat_member"`
}

// startCode is the connect code in "/start CODE" or "/start@ThisBot CODE", if
// the message is one of those, and ok says whether it was a /start for this
// bot at all.
func (b *Bot) startCode(text string) (code string, ok bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", false
	}
	cmd, to, addressed := strings.Cut(fields[0], "@")
	if cmd != "/start" || (addressed && !strings.EqualFold(to, b.Username)) {
		return "", false
	}
	if len(fields) > 1 {
		code = fields[1]
	}
	return code, true
}

// Listen reads what Telegram sends the bot until ctx ends: groups asking to
// be connected, and news of the bot being removed from one or a group moving
// to a new id.
func (b *Bot) Listen(ctx context.Context, s *store.Store) {
	var offset int64
	for ctx.Err() == nil {
		var updates []update
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "my_chat_member"},
		}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				log.Print(err)
				sleep(ctx, 5*time.Second)
			}
			continue
		}
		for _, u := range updates {
			offset = u.ID + 1
			b.handle(ctx, s, u)
		}
	}
}

func (b *Bot) handle(ctx context.Context, s *store.Store, u update) {
	if m := u.Member; m != nil {
		if m.New.Status == "left" || m.New.Status == "kicked" {
			s.ChatBroken(Platform, strconv.FormatInt(m.Chat.ID, 10))
		}
		return
	}
	m := u.Message
	if m == nil {
		return
	}
	target := strconv.FormatInt(m.Chat.ID, 10)
	if m.MigrateTo != 0 {
		s.ChatMoved(Platform, target, strconv.FormatInt(m.MigrateTo, 10))
		return
	}
	code, ok := b.startCode(m.Text)
	if !ok {
		return
	}
	reply := func(text string) {
		if err := b.Send(ctx, target, text); err != nil {
			log.Print(err)
		}
	}
	if m.Chat.Type == "private" {
		reply("Hi! I post Halfway decisions into group chats. On Halfway, open a poll, press Invite, and choose Telegram to add me to your group.")
		return
	}
	if code == "" {
		reply("To connect this group to a poll, add me from the poll's Invite dialog on Halfway.")
		return
	}
	p, err := s.LinkChat(code, Platform, target, m.Chat.Title)
	if errors.Is(err, store.ErrNotFound) {
		reply("That link isn't for any poll any more. Add me again from the poll's Invite dialog on Halfway.")
		return
	} else if err != nil {
		log.Print(err)
		return
	}
	reply("👋 This group is connected to “" + p.Title + "”. I'll post here once it's decided.")
}

// Deliver posts every decision still owed to a Telegram chat. A chat that
// has thrown the bot out is marked broken, so its organizers are asked to
// reconnect it; one that is busy is tried again next time.
func (b *Bot) Deliver(ctx context.Context, s *store.Store) {
	pending, err := s.Pending()
	if err != nil {
		log.Print(err)
		return
	}
	for _, d := range pending {
		if d.Platform != Platform {
			continue
		}
		err := b.Send(ctx, d.Target, d.Text)
		var e *apiError
		switch {
		case err == nil:
			s.Delivered(d.EventID, d.ChatID)
		case errors.As(err, &e) && e.Parameters.MigrateTo != 0:
			// The group became a supergroup; follow it, and send there next time.
			s.ChatMoved(Platform, d.Target, strconv.FormatInt(e.Parameters.MigrateTo, 10))
		case errors.As(err, &e) && (e.Code == http.StatusForbidden ||
			(e.Code == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Description), "chat not found"))):
			s.ChatBroken(Platform, d.Target)
		case errors.As(err, &e) && e.Code == http.StatusTooManyRequests:
			return // try the rest later, too
		default:
			log.Print(err)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
