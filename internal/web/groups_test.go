package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"halfway/internal/places"
	"halfway/internal/store"
)

// makeGroup makes a group through the sidebar form and returns its id.
func (e *env) makeGroup(token, name string) int64 {
	e.t.Helper()
	resp := e.form(token, "/groups", url.Values{"name": {name}})
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "#share") {
		e.t.Fatalf("making a group: %d %q", resp.StatusCode, loc)
	}
	groups, _ := e.st.Groups(1)
	return groups[len(groups)-1].ID
}

func TestGroups(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	gid := e.makeGroup(anna, "Dinner club")
	g, _ := e.st.Group(gid, 1)
	path := "/groups/" + itoa(gid)

	_, body := e.page(anna, path)
	for _, s := range []string{"Dinner club", "/g/" + g.InviteCode, "No polls yet", "Who has travelled most", "Leave group"} {
		if !strings.Contains(body, s) {
			t.Errorf("the group page is missing %q", s)
		}
	}
	if code, _ := e.page(ben, path); code != http.StatusNotFound {
		t.Errorf("an outsider opened the group: %d", code)
	}

	// The group's invite link: a name, and you are in.
	code, body := e.page("", "/g/"+g.InviteCode)
	if code != 200 || !strings.Contains(body, "Join “Dinner club”") {
		t.Fatalf("group invite: %d", code)
	}
	resp := e.form("", "/g/"+g.InviteCode, url.Values{"name": {"Chris"}})
	chris := cookieFrom(resp)
	if resp.Header.Get("Location") != path || chris == "" {
		t.Fatalf("joining the group: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _ := e.page(ben, "/g/"+g.InviteCode); code != http.StatusSeeOther {
		t.Errorf("Ben joining by link: %d", code)
	}

	// "New poll" on the group page makes a poll for everyone in it.
	if _, body := e.page(anna, "/new?group="+itoa(gid)); !strings.Contains(body, `<option value="`+itoa(gid)+`" selected>Dinner club (3 people)</option>`) {
		t.Error("the new poll form does not start with the group chosen")
	}
	v := dinner("")
	v.Set("group", itoa(gid))
	p := e.create(anna, v)
	if p.GroupID != gid {
		t.Fatalf("poll group %d", p.GroupID)
	}
	if people, _ := e.st.Participants(p.ID); len(people) != 3 {
		t.Errorf("%d people on the group's poll", len(people))
	}
	_, body = e.page(chris, "/polls/"+itoa(p.ID))
	if !strings.Contains(body, `<a href="/groups/`+itoa(gid)+`" class="text-reset">Dinner club</a>`) {
		t.Error("the poll does not say which group it is for")
	}
	if _, body = e.page(chris, path); !strings.Contains(body, "Friday dinner") {
		t.Error("the group page does not list its poll")
	}
	// Somebody outside the group cannot make polls for it.
	dana := e.register("Dana")
	v.Set("group", itoa(gid))
	want(t, "an outsider making a group poll", e.form(dana, "/polls", v).StatusCode, http.StatusBadRequest)

	// Only organizers rename, and the only one cannot leave.
	want(t, "a member renaming", e.form(ben, path+"/rename", url.Values{"name": {"Mine"}}).StatusCode, http.StatusForbidden)
	want(t, "the only organizer leaving", e.form(anna, path+"/leave", nil).StatusCode, http.StatusConflict)
	want(t, "a member leaving", e.form(ben, path+"/leave", nil).StatusCode, http.StatusSeeOther)
	if code, _ := e.page(ben, path); code != http.StatusNotFound {
		t.Error("Ben still sees the group he left")
	}
	want(t, "deleting", e.form(anna, path+"/delete", nil).StatusCode, http.StatusSeeOther)
	if code, _ := e.page(anna, "/polls/"+itoa(p.ID)); code != 200 {
		t.Error("deleting the group took its poll with it")
	}
}

func TestTheGroupRemembersWhoTravelledFarthest(t *testing.T) {
	e, _ := newPlacesEnv(t)
	anna, ben, priya := e.register("Anna"), e.register("Ben"), e.register("Priya")
	gid := e.makeGroup(anna, "Dinner club")
	g, _ := e.st.Group(gid, 1)
	e.page(ben, "/g/"+g.InviteCode)
	e.page(priya, "/g/"+g.InviteCode)
	meetup := func(title string) store.Poll {
		v := withPlaces(dinner(""))
		v.Set("title", title)
		v.Set("group", itoa(gid))
		p := e.create(anna, v)
		e.start(anna, p, schwabing, "", "transit")
		e.start(ben, p, giesing, "", "transit")
		e.start(priya, p, pasing, "", "transit")
		return p
	}
	first := meetup("September")
	e.st.Suggest(first.ID, []places.Venue{{Ref: "node/1", Name: "Schwabinger Wirt", At: schwabing}})
	e.form(anna, "/polls/"+itoa(first.ID)+"/slots/"+itoa(e.slots(first)[0].ID)+"/pick", nil)

	_, body := e.page(anna, "/groups/"+itoa(gid))
	if !strings.Contains(body, "text-orange\">+") || !strings.Contains(body, "Priya") {
		t.Error("the group page does not show Priya travelled more than her share")
	}
	second := meetup("October")
	_, body = e.page(priya, "/polls/"+itoa(second.ID))
	if !strings.Contains(body, "Taking turns.") || !strings.Contains(body, "Priya has travelled about") {
		t.Error("the next poll does not say it is evening things out for Priya")
	}
}
