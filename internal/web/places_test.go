package web

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

	"halfway/internal/places"
	"halfway/internal/store"
)

// fakeFinder stands in for OpenStreetMap: a few places around Munich.
type fakeFinder struct {
	mu      sync.Mutex
	nearby  int
	nothing bool // find no venues at all
}

var (
	schwabing = places.Point{Lat: 48.1636, Lon: 11.5868}
	giesing   = places.Point{Lat: 48.1110, Lon: 11.5960}
	pasing    = places.Point{Lat: 48.1494, Lon: 11.4614}
)

func (f *fakeFinder) Search(ctx context.Context, q string, near []places.Point) ([]places.Place, error) {
	switch strings.ToLower(q) {
	case "schwabing":
		return []places.Place{{Label: "Schwabing, München", At: schwabing}}, nil
	case "giesing":
		return []places.Place{{Label: "Giesing, München", At: giesing}}, nil
	case "luitpold":
		return []places.Place{{Label: "Café Luitpold, Brienner Straße 11", At: places.Point{Lat: 48.1435, Lon: 11.5728}}}, nil
	}
	return []places.Place{}, nil
}

func (f *fakeFinder) Nearby(ctx context.Context, category string, center places.Point, km float64) ([]places.Venue, error) {
	f.mu.Lock()
	f.nearby++
	f.mu.Unlock()
	if f.nothing {
		return []places.Venue{}, nil
	}
	return []places.Venue{
		{Ref: "node/1", Name: "Brasserie Mitte", Address: "Marienplatz 1", At: places.Point{Lat: 48.1374, Lon: 11.5755}},
		{Ref: "node/2", Name: "Schwabinger Wirt", At: schwabing},
		{Ref: "node/3", Name: "Pasinger Stube", At: pasing},
		{Ref: "node/4", Name: "Giesinger Eck", At: giesing},
	}, nil
}

func newPlacesEnv(t *testing.T) (*env, *fakeFinder) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFinder{}
	app := New(s, nil, f)
	app.now = func() time.Time { return monday }
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &env{t: t, srv: srv, st: s, app: app}, f
}

func withPlaces(v url.Values) url.Values {
	v.Set("places", "on")
	return v
}

func (e *env) start(token string, p store.Poll, at places.Point, label, mode string) *http.Response {
	e.t.Helper()
	return e.form(token, "/polls/"+itoa(p.ID)+"/start", url.Values{
		"lat": {jsonNumber(at.Lat)}, "lon": {jsonNumber(at.Lon)}, "label": {label}, "mode": {mode}})
}

func jsonNumber(f float64) string { b, _ := json.Marshal(f); return string(b) }

func TestPlacesAreOffUnlessTheServerTurnsThemOn(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	if _, body := e.page(anna, "/new"); strings.Contains(body, `name="places"`) {
		t.Error("the form offers places on a server without them")
	}
	p := e.create(anna, withPlaces(dinner("")))
	if p.Places {
		t.Error("a poll got places on a server without them")
	}
	if code, _ := e.page(anna, "/polls/"+itoa(p.ID)+"/places?q=x"); code != http.StatusNotFound {
		t.Errorf("searching on a server without places: %d", code)
	}
	if _, body := e.page(anna, "/polls/"+itoa(p.ID)); strings.Contains(body, `data-live="places"`) {
		t.Error("the Where card is on a poll without places")
	}
}

func TestFindingAFairPlace(t *testing.T) {
	e, finder := newPlacesEnv(t)
	anna, ben, chris := e.register("Anna"), e.register("Ben"), e.register("Chris")
	if _, body := e.page(anna, "/new"); !strings.Contains(body, `name="places" value="on" checked`) {
		t.Error("the form should offer places, on to begin with")
	}
	p := e.create(anna, withPlaces(dinner("")))
	if !p.Places {
		t.Fatal("places did not stick")
	}
	e.join(ben, p)
	e.join(chris, p)
	id := itoa(p.ID)

	// Nothing to go on yet.
	want(t, "suggesting with no starting points", e.form(anna, "/polls/"+id+"/venues/suggest", nil).StatusCode, http.StatusConflict)
	_, body := e.page(anna, "/polls/"+id)
	if !strings.Contains(body, "Where are you coming from?") || !strings.Contains(body, "Places © OpenStreetMap contributors") {
		t.Error("the Where card does not ask where people are coming from")
	}

	// Searching goes through this server, as JSON.
	resp, raw := e.fetch("GET", "/polls/"+id+"/places?q=Schwabing", anna, nil)
	var found []places.Place
	json.Unmarshal(raw, &found)
	if resp.StatusCode != 200 || len(found) != 1 || found[0].Label != "Schwabing, München" {
		t.Fatalf("search: %d %s", resp.StatusCode, raw)
	}
	stranger := e.register("Stranger")
	if code, _ := e.page(stranger, "/polls/"+id+"/places?q=Schwabing"); code != http.StatusNotFound {
		t.Errorf("somebody not on the poll searched: %d", code)
	}

	want(t, "a start off the map", e.start(anna, p, places.Point{Lat: 300}, "", "").StatusCode, http.StatusBadRequest)
	want(t, "Anna's start", e.start(anna, p, schwabing, "Schwabing, München", "bike").StatusCode, http.StatusSeeOther)
	e.start(ben, p, giesing, "Giesing, München", "transit")
	e.start(chris, p, pasing, "Your location", "car")

	// Your own starting point is yours to see; the others see times.
	_, body = e.page(anna, "/polls/"+id)
	if !strings.Contains(body, "Schwabing, München") || strings.Contains(body, "Giesing, München") {
		t.Error("Anna should see her own starting point and nobody else's")
	}
	if !strings.Contains(body, "3 of 3 have said where they") {
		t.Error("the count of starting points is missing")
	}

	want(t, "suggesting", e.form(ben, "/polls/"+id+"/venues/suggest", url.Values{}).StatusCode, http.StatusSeeOther)
	venues, _ := e.st.Venues(p.ID)
	if len(venues) != suggestions || venues[0].Name != "Brasserie Mitte" {
		t.Fatalf("suggested %+v", venues)
	}
	_, body = e.page(chris, "/polls/"+id)
	for _, s := range []string{"Brasserie Mitte", "Fairest", "Anna ", " min", "longest", "I'd go", "Suggest again", "openstreetmap.org/?mlat="} {
		if !strings.Contains(body, s) {
			t.Errorf("the places on the page are missing %q", s)
		}
	}
	// Each suggestion asks OpenStreetMap, so a poll gets a few a minute.
	e.form(ben, "/polls/"+id+"/venues/suggest", nil)
	e.form(ben, "/polls/"+id+"/venues/suggest", nil)
	want(t, "suggesting too often", e.form(ben, "/polls/"+id+"/venues/suggest", nil).StatusCode, http.StatusTooManyRequests)
	if finder.nearby != 3 {
		t.Errorf("%d lookups, want 3", finder.nearby)
	}

	// Votes, then the decision.
	venues, _ = e.st.Venues(p.ID)
	var wirt store.Venue
	for _, v := range venues {
		if v.Name == "Schwabinger Wirt" {
			wirt = v
		}
	}
	if wirt.ID == 0 {
		t.Fatalf("no Schwabinger Wirt in %+v", venues)
	}
	vote := func(token string, v store.Venue) {
		want(t, "voting for a place", e.form(token, "/polls/"+id+"/venues/"+itoa(v.ID)+"/vote", url.Values{}).StatusCode, http.StatusSeeOther)
	}
	vote(anna, wirt)
	vote(ben, wirt)
	e.vote(anna, p, e.slots(p)[1], store.Yes)
	e.form(anna, "/polls/"+id+"/decide", nil)
	_, body = e.page(ben, "/polls/"+id)
	if !strings.Contains(body, "It's on") || !strings.Contains(body, `<i class="ti ti-map-pin text-green me-1"></i>Schwabinger Wirt`) {
		t.Error("the result does not say where")
	}
	if !strings.Contains(body, "is on: Fri 9 Oct, 19:00\n📍 Schwabinger Wirt\nMap: https://www.openstreetmap.org/") {
		t.Error("the message to tell everyone does not say where")
	}
	if _, body = e.page("", "/i/"+p.InviteCode); !strings.Contains(body, "✅ Fri 9 Oct, 19:00 · Schwabinger Wirt") {
		t.Error("the link preview does not say where")
	}
	_, ics := e.fetch("GET", "/polls/"+id+"/event.ics", anna, nil)
	if !strings.Contains(string(ics), "LOCATION:Schwabinger Wirt\r\n") || !strings.Contains(string(ics), "GEO:48.16360;11.58680") {
		t.Errorf("the calendar file does not say where:\n%s", ics)
	}
	evs, _ := e.st.Events(p.ID)
	if !strings.Contains(evs[0].Text, " at Schwabinger Wirt") {
		t.Errorf("chat message %q", evs[0].Text)
	}
	if _, body = e.page(anna, "/"); !strings.Contains(body, "Fri 9 Oct, 19:00 · Schwabinger Wirt") {
		t.Error("the dashboard card does not say where")
	}
}

func TestOrganizersAddAndOverruleAPlace(t *testing.T) {
	e, finder := newPlacesEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, withPlaces(dinner("")))
	e.join(ben, p)
	id := itoa(p.ID)
	luitpold := url.Values{"lat": {"48.1435"}, "lon": {"11.5728"}, "label": {"Café Luitpold, Brienner Straße 11"}}
	want(t, "a participant adding a place", e.form(ben, "/polls/"+id+"/venues", luitpold).StatusCode, http.StatusForbidden)
	want(t, "the organizer adding a place", e.form(anna, "/polls/"+id+"/venues", luitpold).StatusCode, http.StatusSeeOther)
	venues, _ := e.st.Venues(p.ID)
	if len(venues) != 1 || venues[0].Name != "Café Luitpold" || venues[0].Address != "Brienner Straße 11" || !venues[0].Custom() {
		t.Fatalf("got %+v", venues)
	}
	_, body := e.page(anna, "/polls/"+id)
	if !strings.Contains(body, "Added by hand") || !strings.Contains(body, "Settle on this place") {
		t.Error("the organizer's place is not shown as theirs to settle on")
	}
	want(t, "a participant picking a place", e.form(ben, "/polls/"+id+"/venues/"+itoa(venues[0].ID)+"/pick", nil).StatusCode, http.StatusForbidden)

	// Nothing nearby: say so, rather than suggesting nothing silently.
	finder.nothing = true
	e.start(ben, p, giesing, "", "")
	resp := e.form(ben, "/polls/"+id+"/venues/suggest", nil)
	want(t, "suggesting with nothing around", resp.StatusCode, http.StatusUnprocessableEntity)
	if finder.nearby != 2 {
		t.Errorf("%d lookups; it should look twice as far once before giving up", finder.nearby)
	}

	// Changing how you travel keeps where from.
	want(t, "changing mode", e.form(ben, "/polls/"+id+"/start/mode", url.Values{"mode": {"walk"}}).StatusCode, http.StatusSeeOther)
	starts, _ := e.st.Starts(p.ID)
	if len(starts) != 1 || starts[0].Mode != "walk" {
		t.Errorf("starts %+v", starts)
	}
	e.form(ben, "/polls/"+id+"/start/clear", nil)
	if starts, _ := e.st.Starts(p.ID); len(starts) != 0 {
		t.Error("forgetting the starting point did not")
	}
}
