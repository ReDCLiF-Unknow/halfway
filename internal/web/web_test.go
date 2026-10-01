package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"halfway/internal/store"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	app *Server // for setting its clock
}

// The tests happen on Monday 5 October 2026, in the afternoon.
var monday = time.Date(2026, 10, 5, 14, 0, 0, 0, time.Local)

func at(days, hour int) string {
	return time.Date(2026, 10, 5+days, hour, 0, 0, 0, time.Local).Format(store.Stamp)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := New(s, nil)
	app.now = func() time.Time { return monday }
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &env{t: t, srv: srv, st: s, app: app}
}

// noRedirect lets tests inspect redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func want(t *testing.T, what string, got, wanted int) {
	t.Helper()
	if got != wanted {
		t.Errorf("%s: status %d, want %d", what, got, wanted)
	}
}

// form posts a form the way a browser on this site would, signed in with
// the cookie when token is set, and returns the response unread.
func (e *env) form(token, path string, v url.Values) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// page fetches an HTML page as a browser signed in with token would.
func (e *env) page(token, path string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func cookieFrom(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	return ""
}

// register picks a name on the welcome page and returns the key it gets.
func (e *env) register(name string) string {
	e.t.Helper()
	resp := e.form("", "/welcome", url.Values{"name": {name}})
	token := cookieFrom(resp)
	if resp.StatusCode != http.StatusSeeOther || token == "" {
		e.t.Fatalf("register %s: status %d", name, resp.StatusCode)
	}
	return token
}

// create makes a poll through the form and returns it.
func (e *env) create(token string, v url.Values) store.Poll {
	e.t.Helper()
	resp := e.form(token, "/polls", v)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "#share") {
		e.t.Fatalf("create: status %d, location %q", resp.StatusCode, loc)
	}
	id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(loc, "/polls/"), "#share"), 10, 64)
	p, err := e.st.Poll(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// dinner is a poll with three evenings, deciding on Wednesday evening.
func dinner(quorum string) url.Values {
	return url.Values{"title": {"Friday dinner"}, "category": {"dinner"}, "deadline": {at(2, 18)}, "quorum": {quorum},
		"slot": {at(3, 19), at(4, 19), "", at(5, 19)}}
}

func (e *env) slots(p store.Poll) []store.Slot {
	e.t.Helper()
	s, err := e.st.Slots(p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) vote(token string, p store.Poll, sl store.Slot, answer int) *http.Response {
	e.t.Helper()
	return e.form(token, "/polls/"+itoa(p.ID)+"/slots/"+itoa(sl.ID)+"/vote", url.Values{"answer": {strconv.Itoa(answer)}})
}

// join follows an invite link as someone already signed in.
func (e *env) join(token string, p store.Poll) {
	e.t.Helper()
	if code, _ := e.page(token, "/i/"+p.InviteCode); code != http.StatusSeeOther {
		e.t.Fatalf("joining: status %d", code)
	}
}

func TestSigningUpAsksOnlyForAName(t *testing.T) {
	e := newEnv(t)
	code, body := e.page("", "/")
	want(t, "home without a name", code, http.StatusSeeOther)
	code, body = e.page("", "/welcome?next=/answer")
	if code != 200 || !strings.Contains(body, "What should we call you?") || !strings.Contains(body, "Already use Halfway on another device?") {
		t.Fatalf("welcome page: %d", code)
	}
	resp := e.form("", "/welcome", url.Values{"name": {"  "}})
	want(t, "a blank name", resp.StatusCode, http.StatusBadRequest)
	resp = e.form("", "/welcome", url.Values{"name": {"Anna"}, "next": {"//evil.example"}})
	if resp.Header.Get("Location") != "/" {
		t.Errorf("sent elsewhere after signing up: %q", resp.Header.Get("Location"))
	}
	token := cookieFrom(resp)
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && (!c.HttpOnly || c.SameSite != http.SameSiteLaxMode) {
			t.Errorf("the cookie is not HttpOnly and SameSite=Lax: %+v", c)
		}
	}
	_, body = e.page(token, "/")
	if !strings.Contains(body, "Plan your first meetup") || !strings.Contains(body, "Save your sign-in key") {
		t.Error("a new person's dashboard should be empty and ask them to save their key")
	}
	// The key signs in on another device.
	resp = e.form("", "/welcome/token", url.Values{"token": {token}, "next": {"/answer"}})
	if cookieFrom(resp) != token || resp.Header.Get("Location") != "/answer" {
		t.Errorf("signing in with the key: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = e.form("", "/welcome/token", url.Values{"token": {"nope"}})
	want(t, "a wrong key", resp.StatusCode, http.StatusBadRequest)
	// Saying it is saved stops the reminder.
	e.form(token, "/me/token/saved", url.Values{"next": {"/"}})
	if _, body = e.page(token, "/"); strings.Contains(body, "Save your sign-in key") {
		t.Error("still reminded after saving the key")
	}
}

func TestMakingAPoll(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	code, body := e.page(anna, "/new")
	if code != 200 || !strings.Contains(body, `name="slot"`) || !strings.Contains(body, "Only if at least") {
		t.Fatalf("new poll page: %d", code)
	}
	bad := dinner("")
	bad.Set("deadline", at(4, 20))
	resp := e.form(anna, "/polls", bad)
	want(t, "a deadline after the first time", resp.StatusCode, http.StatusBadRequest)
	bad = dinner("1")
	resp = e.form(anna, "/polls", bad)
	want(t, "a minimum of one", resp.StatusCode, http.StatusBadRequest)

	p := e.create(anna, dinner("3"))
	if p.Title != "Friday dinner" || p.Quorum != 3 || p.Deadline != at(2, 18) || !strings.HasPrefix(p.Origin, "http://127.0.0.1") {
		t.Errorf("got %+v", p)
	}
	if n := len(e.slots(p)); n != 3 {
		t.Errorf("%d times, want 3 (the empty row is skipped)", n)
	}
	_, body = e.page(anna, "/polls/"+itoa(p.ID))
	for _, s := range []string{"Friday dinner", "/i/" + p.InviteCode, "/o/" + p.OrganizerCode, "On if 3 can come", "Deadline and minimum", "Decide now"} {
		if !strings.Contains(body, s) {
			t.Errorf("the poll page for its organizer is missing %q", s)
		}
	}
	if !strings.Contains(body, "Telegram isn't set up on this server") {
		t.Error("with no bot, the invite dialog should say Telegram is off")
	}
	_, body = e.page(anna, "/")
	if !strings.Contains(body, "Waiting for your answer") || !strings.Contains(body, "0 of 1 answered") {
		t.Error("the new poll is not on the dashboard as waiting for an answer")
	}
}

func TestInviteLinkJoinsWithANameAndNothingElse(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	p := e.create(anna, dinner(""))

	// A newcomer (or a chat app fetching a preview) sees what the poll is,
	// how it stands, and a box for a name.
	code, body := e.page("", "/i/"+p.InviteCode)
	if code != 200 || !strings.Contains(body, "Join “Friday dinner”") || !strings.Contains(body, `name="name"`) {
		t.Fatalf("invite page: %d", code)
	}
	if !strings.Contains(body, `<meta property="og:title" content="Friday dinner">`) ||
		!strings.Contains(body, `<meta property="og:description" content="0 of 1 answered · decides `+store.Label(at(2, 18))+`">`) {
		t.Error("the link preview does not show how the poll stands")
	}

	resp := e.form("", "/i/"+p.InviteCode, url.Values{"name": {"Ben"}})
	ben := cookieFrom(resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/polls/"+itoa(p.ID) || ben == "" {
		t.Fatalf("joining with a name: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Somebody with a name already joins just by opening the link.
	chris := e.register("Chris")
	e.join(chris, p)
	people, _ := e.st.Participants(p.ID)
	if len(people) != 3 {
		t.Errorf("%d people on the poll, want 3", len(people))
	}
	_, body = e.page(chris, "/")
	if !strings.Contains(body, "Friday dinner") {
		t.Error("the poll is not in Chris's list")
	}

	// A closed link lets its people in, and nobody else, but still previews.
	e.form(anna, "/polls/"+itoa(p.ID)+"/invite/close", url.Values{})
	if code, _ := e.page(ben, "/i/"+p.InviteCode); code != http.StatusSeeOther {
		t.Errorf("Ben, on the poll, was kept out by the closed link: %d", code)
	}
	dana := e.register("Dana")
	code, body = e.page(dana, "/i/"+p.InviteCode)
	if code != 200 || !strings.Contains(body, "This invite link is closed") || !strings.Contains(body, `og:description`) {
		t.Errorf("a closed link for a newcomer: %d", code)
	}
	if _, err := e.st.Role(p.ID, 4); err == nil {
		t.Error("Dana got onto the poll through a closed link")
	}
	resp = e.form("", "/i/"+p.InviteCode, url.Values{"name": {"Eve"}})
	want(t, "joining through a closed link", resp.StatusCode, http.StatusForbidden)

	// A replaced link leads nowhere.
	e.form(anna, "/polls/"+itoa(p.ID)+"/invite/reset", url.Values{})
	code, body = e.page("", "/i/"+p.InviteCode)
	if code != http.StatusNotFound || strings.Contains(body, "og:description") {
		t.Errorf("an old link: %d", code)
	}
}

func TestOnlyPeopleOnAPollSeeIt(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, dinner(""))
	sl := e.slots(p)[0]
	id := itoa(p.ID)
	if code, _ := e.page(ben, "/polls/"+id); code != http.StatusNotFound {
		t.Errorf("a stranger opened the poll: %d", code)
	}
	want(t, "a stranger answering", e.vote(ben, p, sl, store.Yes).StatusCode, http.StatusNotFound)
	want(t, "a stranger deciding", e.form(ben, "/polls/"+id+"/decide", nil).StatusCode, http.StatusNotFound)

	e.join(ben, p)
	for _, path := range []string{"/decide", "/rename", "/settings", "/invite/reset", "/organizer/reset", "/delete", "/slots/" + itoa(sl.ID) + "/pick"} {
		want(t, "a participant posting "+path, e.form(ben, "/polls/"+id+path, url.Values{"title": {"Mine now"}}).StatusCode, http.StatusForbidden)
	}
	_, body := e.page(ben, "/polls/"+id)
	if strings.Contains(body, p.OrganizerCode) || strings.Contains(body, "Decide now") {
		t.Error("a participant was shown what only organizers get")
	}
	if !strings.Contains(body, "Leave poll") {
		t.Error("a participant cannot leave")
	}

	// The organizer link makes an organizer.
	chris := e.register("Chris")
	if code, _ := e.page(chris, "/o/"+p.OrganizerCode); code != http.StatusSeeOther {
		t.Fatalf("organizer link: %d", code)
	}
	if org, _ := e.st.Role(p.ID, 3); !org {
		t.Error("the organizer link did not make Chris an organizer")
	}
	resp := e.form("", "/o/"+p.OrganizerCode, url.Values{"name": {"Dana"}})
	if resp.StatusCode != http.StatusSeeOther || cookieFrom(resp) == "" {
		t.Errorf("a newcomer through the organizer link: %d", resp.StatusCode)
	}
}

func TestAnsweringUntilTheQuorumDecides(t *testing.T) {
	e := newEnv(t)
	anna, ben, chris := e.register("Anna"), e.register("Ben"), e.register("Chris")
	p := e.create(anna, dinner("3"))
	e.join(ben, p)
	e.join(chris, p)
	thu, fri := e.slots(p)[0], e.slots(p)[1]
	id := itoa(p.ID)

	resp := e.vote(anna, p, fri, store.Yes)
	want(t, "answering", resp.StatusCode, http.StatusSeeOther)
	want(t, "a made-up answer", e.vote(anna, p, fri, 7).StatusCode, http.StatusBadRequest)
	e.vote(ben, p, fri, store.IfNeeded)
	e.vote(ben, p, thu, store.No)
	_, body := e.page(anna, "/polls/"+id)
	if !strings.Contains(body, "Leading") || !strings.Contains(body, "Ben if needed") || !strings.Contains(body, "2 / 3") {
		t.Error("the page does not show who can come and how close it is")
	}
	if !strings.Contains(body, "Waiting") && !strings.Contains(body, "On if 3 can come") {
		t.Error("the stats are missing")
	}

	e.vote(chris, p, fri, store.Yes) // the third
	got, _ := e.st.Poll(p.ID)
	if !got.IsConfirmed() || got.ChosenSlot != fri.ID {
		t.Fatalf("the quorum did not decide it: %+v", got)
	}
	// A late answer lands on the decision rather than an error.
	resp = e.vote(chris, p, thu, store.Yes)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("answering a decided poll: %d", resp.StatusCode)
	}
	_, body = e.page(chris, "/polls/"+id)
	for _, s := range []string{"It's on", "Friday 9 October, 19:00", "Add to calendar", "Copy result", "✅ Friday dinner is on: Fri 9 Oct, 19:00"} {
		if !strings.Contains(body, s) {
			t.Errorf("the decided poll's page is missing %q", s)
		}
	}
	if strings.Contains(body, `name="answer"`) {
		t.Error("a decided poll still takes answers")
	}
	// Anna has not looked since: her lists say there is news.
	_, body = e.page(anna, "/")
	if !strings.Contains(body, "(1) Halfway") || !strings.Contains(body, ">New<") {
		t.Error("Anna's dashboard does not point out the decision")
	}
	e.page(anna, "/polls/"+id)
	if _, body = e.page(anna, "/"); strings.Contains(body, "(1) Halfway") {
		t.Error("still news after Anna looked")
	}
}

func TestTheDeadlineDecidesWithoutAnybody(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, dinner(""))
	e.join(ben, p)
	thu, sat := e.slots(p)[0], e.slots(p)[2]
	e.vote(anna, p, sat, store.Yes)
	e.vote(ben, p, sat, store.Yes)
	e.vote(ben, p, thu, store.Yes)

	// Run decides on its own once the clock passes the deadline.
	e.app.now = func() time.Time { return time.Date(2026, 10, 7, 18, 1, 0, 0, time.Local) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.app.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := e.st.Poll(p.ID)
		if got.IsConfirmed() {
			if got.ChosenSlot != sat.ID {
				t.Errorf("chose %d, want Saturday, which has two yeses", got.ChosenSlot)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the deadline passed and nothing was decided")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestAPageDecidesAPollWhoseDeadlineHasPassed(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	p := e.create(anna, dinner("2"))
	e.vote(anna, p, e.slots(p)[0], store.Yes)
	e.app.now = func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.Local) }
	_, body := e.page(anna, "/polls/"+itoa(p.ID))
	if !strings.Contains(body, "Called off") || !strings.Contains(body, "No time had 2 people") {
		t.Error("the page still shows an open poll after its deadline")
	}
	if !strings.Contains(body, "Pick this time") {
		t.Error("an organizer should be able to put a cancelled poll on anyway")
	}
}

func TestOrganizerDecidesAndOverrules(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	p := e.create(anna, dinner(""))
	thu, fri := e.slots(p)[0], e.slots(p)[1]
	id := itoa(p.ID)
	e.vote(anna, p, thu, store.Yes)
	want(t, "deciding now", e.form(anna, "/polls/"+id+"/decide", url.Values{}).StatusCode, http.StatusSeeOther)
	got, _ := e.st.Poll(p.ID)
	if got.ChosenSlot != thu.ID {
		t.Fatalf("decided for %d, want Thursday", got.ChosenSlot)
	}
	e.form(anna, "/polls/"+id+"/slots/"+itoa(fri.ID)+"/pick", url.Values{})
	got, _ = e.st.Poll(p.ID)
	if got.ChosenSlot != fri.ID {
		t.Fatal("picking another time did not move it")
	}
	evs, _ := e.st.Events(p.ID)
	if len(evs) != 2 || evs[1].Kind != store.KindChanged {
		t.Errorf("events: %+v", evs)
	}
}

func TestSettingsCanPutItOnAtOnce(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, dinner("4"))
	e.join(ben, p)
	fri := e.slots(p)[1]
	e.vote(anna, p, fri, store.Yes)
	e.vote(ben, p, fri, store.Yes)
	id := itoa(p.ID)
	want(t, "a deadline gone by", e.form(anna, "/polls/"+id+"/settings", url.Values{"deadline": {at(-1, 12)}, "quorum": {"4"}}).StatusCode, http.StatusBadRequest)
	want(t, "saving", e.form(anna, "/polls/"+id+"/settings", url.Values{"deadline": {at(1, 12)}, "quorum": {"2"}}).StatusCode, http.StatusSeeOther)
	got, _ := e.st.Poll(p.ID)
	if !got.IsConfirmed() || got.Deadline != at(1, 12) {
		t.Errorf("got %+v", got)
	}
}

func TestCalendarFile(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	v := dinner("")
	v.Set("title", "Dinner; Anna's, Ben's")
	p := e.create(anna, v)
	id := itoa(p.ID)
	if code, _ := e.page(anna, "/polls/"+id+"/event.ics"); code != http.StatusNotFound {
		t.Errorf("a calendar file for an undecided poll: %d", code)
	}
	e.form(anna, "/polls/"+id+"/slots/"+itoa(e.slots(p)[1].ID)+"/pick", url.Values{})
	resp, body := e.fetch("GET", "/polls/"+id+"/event.ics", anna, nil)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/calendar") ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="Dinner-Annas-Bens.ics"` {
		t.Errorf("headers: %v", resp.Header)
	}
	ics := string(body)
	for _, s := range []string{"BEGIN:VEVENT\r\n", "DTSTART:20261009T190000\r\n", "DTEND:20261009T210000\r\n", `SUMMARY:Dinner\; Anna's\, Ben's`, "/i/" + p.InviteCode} {
		if !strings.Contains(ics, s) {
			t.Errorf("the calendar file is missing %q:\n%s", s, ics)
		}
	}
}

func TestLeavingAndRemoving(t *testing.T) {
	e := newEnv(t)
	anna, ben, chris := e.register("Anna"), e.register("Ben"), e.register("Chris")
	p := e.create(anna, dinner(""))
	e.join(ben, p)
	e.join(chris, p)
	id := itoa(p.ID)
	want(t, "the only organizer leaving", e.form(anna, "/polls/"+id+"/leave", nil).StatusCode, http.StatusConflict)
	_, body := e.page(anna, "/polls/"+id)
	if strings.Contains(body, "Leave poll") {
		t.Error("the only organizer is offered to leave")
	}
	want(t, "leaving", e.form(ben, "/polls/"+id+"/leave", nil).StatusCode, http.StatusSeeOther)
	if code, _ := e.page(ben, "/polls/"+id); code != http.StatusNotFound {
		t.Errorf("Ben still sees the poll after leaving: %d", code)
	}
	e.form(anna, "/polls/"+id+"/people/3/remove", url.Values{})
	if people, _ := e.st.Participants(p.ID); len(people) != 1 {
		t.Errorf("%d people left, want 1", len(people))
	}
}

func TestDeleteOffersUndo(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, dinner(""))
	e.join(ben, p)
	id := itoa(p.ID)

	// Organizers can delete a poll from the dashboard and from its Invite
	// dialog; everybody else can only take it off their own list.
	_, body := e.page(anna, "/")
	if !strings.Contains(body, `action="/polls/`+id+`/delete"`) {
		t.Error("the organizer's dashboard card has no way to delete the poll")
	}
	_, body = e.page(anna, "/polls/"+id)
	if strings.Count(body, `action="/polls/`+id+`/delete"`) != 2 {
		t.Error("the poll page should offer Delete in the ⋯ menu and the Invite dialog")
	}
	_, body = e.page(ben, "/")
	if strings.Contains(body, "/delete") || !strings.Contains(body, `action="/polls/`+id+`/leave"`) {
		t.Error("a participant's card should offer Leave, not Delete")
	}

	resp := e.form(anna, "/polls/"+id+"/delete", nil)
	if resp.Header.Get("Location") != "/?undo="+id {
		t.Fatalf("delete sent us to %q", resp.Header.Get("Location"))
	}
	if code, _ := e.page(anna, "/polls/"+id); code != http.StatusNotFound {
		t.Error("the deleted poll still opens")
	}
	resp = e.form(anna, "/polls/"+id+"/restore", nil)
	if resp.Header.Get("Location") != "/polls/"+id {
		t.Fatalf("restore: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _ := e.page(anna, "/polls/"+id); code != 200 {
		t.Error("the restored poll does not open")
	}
}

func TestAnswerPageListsWhatIsWaiting(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, dinner(""))
	e.join(ben, p)
	_, body := e.page(ben, "/answer")
	if !strings.Contains(body, "Friday dinner") || !regexp.MustCompile(`bg-orange-lt text-orange">1<`).MatchString(body) {
		t.Error("the poll Ben hasn't answered is not waiting for him")
	}
	e.vote(ben, p, e.slots(p)[0], store.No)
	if _, body = e.page(ben, "/answer"); !strings.Contains(body, "You're all caught up") {
		t.Error("still waiting after Ben answered")
	}
}

func TestChangesReachOtherPeopleLive(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, dinner(""))
	e.join(ben, p)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/events", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: anna})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := bufio.NewScanner(res.Body)
	lines.Scan() // "retry: 3000"

	e.vote(ben, p, e.slots(p)[0], store.Yes)
	for lines.Scan() {
		if lines.Text() == "event: changed" {
			return
		}
	}
	t.Fatal("Ben's answer never reached Anna's page")
}

func TestUntil(t *testing.T) {
	now := monday
	for stamp, want := range map[string]string{
		at(0, 13):  "passed",
		at(0, 15):  "in 1 hour",
		at(0, 20):  "in 6 hours",
		at(2, 18):  "in 2 days",
		at(21, 14): "in 3 weeks",
		now.Add(30 * time.Second).Format(store.Stamp): "in a moment",
	} {
		if got := until(stamp, now); got != want {
			t.Errorf("until(%s) = %q, want %q", stamp, got, want)
		}
	}
}
