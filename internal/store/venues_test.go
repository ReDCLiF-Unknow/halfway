package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"halfway/internal/places"
)

var (
	schwabing = places.Point{Lat: 48.1636, Lon: 11.5868}
	giesing   = places.Point{Lat: 48.1110, Lon: 11.5960}
	pasing    = places.Point{Lat: 48.1494, Lon: 11.4614}
	marienpl  = places.Point{Lat: 48.1374, Lon: 11.5755}
)

// placesPoll is poll with places turned on.
func placesPoll(t *testing.T, s *Store, by User, quorum int) (Poll, []Slot) {
	t.Helper()
	p, err := s.CreatePoll(by.ID, NewPoll{
		Title: "Friday dinner", Category: "dinner", Deadline: at(2, 18), Quorum: quorum, Places: true,
		Slots: []string{at(3, 19), at(4, 19)}, Origin: "https://halfway.example",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Places {
		t.Fatal("places did not stick")
	}
	slots, _ := s.Slots(p.ID)
	return p, slots
}

func TestStartsAreRoundedAndOnlyForPeopleOnThePoll(t *testing.T) {
	s := open(t)
	anna, ben := user(t, s, "Anna"), user(t, s, "Ben")
	p, _ := placesPoll(t, s, anna, 0)
	if err := s.SetStart(p.ID, anna.ID, places.Point{Lat: 48.16371, Lon: 11.58689}, "  Schwabing,   München ", "bike"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStart(p.ID, ben.ID, schwabing, "", "walk"); !errors.Is(err, ErrNotFound) {
		t.Errorf("somebody not on the poll set a start: %v", err)
	}
	if err := s.SetStart(p.ID, anna.ID, places.Point{Lat: 200}, "", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("a point off the Earth: %v", err)
	}
	starts, _ := s.Starts(p.ID)
	if len(starts) != 1 || starts[0].At != (places.Point{Lat: 48.165, Lon: 11.585}) || starts[0].Label != "Schwabing, München" || starts[0].Mode != "bike" {
		t.Fatalf("got %+v", starts)
	}
	s.SetMode(p.ID, anna.ID, "rocket")
	if starts, _ := s.Starts(p.ID); starts[0].Mode != "transit" {
		t.Errorf("mode %q", starts[0].Mode)
	}
	// Leaving takes the start along.
	s.Join(p.ID, ben.ID, false)
	s.SetStart(p.ID, ben.ID, giesing, "", "")
	s.Leave(p.ID, ben.ID)
	if starts, _ := s.Starts(p.ID); len(starts) != 1 {
		t.Errorf("%d starts after Ben left", len(starts))
	}
	s.ClearStart(p.ID, anna.ID)
	if starts, _ := s.Starts(p.ID); len(starts) != 0 {
		t.Error("the start was not cleared")
	}
}

func TestSuggestKeepsWhatPeopleChoseAndCapsTheList(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, _ := placesPoll(t, s, anna, 0)
	first := []places.Venue{
		{Ref: "node/1", Name: "Café One", At: marienpl},
		{Ref: "node/2", Name: "Café Two", At: schwabing},
		{Ref: "node/3", Name: "Café Three", At: giesing},
	}
	if err := s.Suggest(p.ID, first); err != nil {
		t.Fatal(err)
	}
	vs, _ := s.Venues(p.ID)
	if len(vs) != 3 {
		t.Fatalf("got %+v", vs)
	}
	s.ToggleVenueVote(p.ID, vs[1].ID, anna.ID)
	custom, err := s.AddVenue(p.ID, anna.ID, "Anna's flat", "Somewhere 1", pasing)
	if err != nil {
		t.Fatal(err)
	}
	// Suggesting again keeps the voted one and the custom one, and replaces the rest.
	s.Suggest(p.ID, []places.Venue{{Ref: "node/2", Name: "Café Two", At: schwabing}, {Ref: "node/9", Name: "Café Nine", At: marienpl}})
	vs, _ = s.Venues(p.ID)
	names := []string{}
	for _, v := range vs {
		names = append(names, v.Name)
	}
	if strings.Join(names, ",") != "Café Two,Anna's flat,Café Nine" {
		t.Errorf("after suggesting again: %v", names)
	}
	if !vs[1].Custom() || vs[1].ID != custom.ID || vs[0].Custom() {
		t.Error("Custom() is wrong")
	}
	if _, err := s.AddVenue(p.ID, anna.ID, " ", "", pasing); !errors.Is(err, ErrInvalid) {
		t.Errorf("a place with no name: %v", err)
	}
	var many []places.Venue
	for i := 0; i < 20; i++ {
		many = append(many, places.Venue{Ref: "node/" + itoa(int64(100+i)), Name: "Place " + itoa(int64(i)), At: marienpl})
	}
	s.Suggest(p.ID, many)
	if vs, _ := s.Venues(p.ID); len(vs) != MaxVenues {
		t.Errorf("%d places, want at most %d", len(vs), MaxVenues)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestTheDecisionPicksAPlaceAndSaysSo(t *testing.T) {
	s := open(t)
	anna, ben, chris := user(t, s, "Anna"), user(t, s, "Ben"), user(t, s, "Chris")
	p, slots := placesPoll(t, s, anna, 0)
	s.Join(p.ID, ben.ID, false)
	s.Join(p.ID, chris.ID, false)
	s.SetStart(p.ID, anna.ID, schwabing, "", "transit")
	s.SetStart(p.ID, ben.ID, giesing, "", "transit")
	s.SetStart(p.ID, chris.ID, pasing, "", "transit")
	s.Suggest(p.ID, []places.Venue{
		{Ref: "node/1", Name: "Next to Anna", At: schwabing},
		{Ref: "node/2", Name: "Middle", At: marienpl},
		{Ref: "node/3", Name: "Next to Chris", At: pasing},
	})
	vs, _ := s.Venues(p.ID)

	// No votes: the fairest wins.
	if top, _ := BestVenue(vs, mustStarts(t, s, p)); top.Name != "Middle" {
		t.Errorf("with no votes, chose %q", top.Name)
	}
	// Votes beat fairness.
	s.ToggleVenueVote(p.ID, vs[2].ID, chris.ID)
	s.ToggleVenueVote(p.ID, vs[0].ID, anna.ID)
	s.ToggleVenueVote(p.ID, vs[0].ID, ben.ID)
	s.ToggleVenueVote(p.ID, vs[2].ID, chris.ID) // Chris changes their mind
	vs, _ = s.Venues(p.ID)
	if len(vs[0].Voters) != 2 || len(vs[2].Voters) != 0 {
		t.Fatalf("voters: %+v", vs)
	}
	vote(t, s, p, slots[0], anna, Yes)
	ev, err := s.DecideNow(p.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ev.Text, "✅ Friday dinner is on: "+Label(slots[0].Start)+" at Next to Anna · ") {
		t.Errorf("message %q", ev.Text)
	}
	got, _ := s.Poll(p.ID)
	if got.ChosenVenue != vs[0].ID {
		t.Errorf("chose venue %d", got.ChosenVenue)
	}
	if err := s.ToggleVenueVote(p.ID, vs[1].ID, chris.ID); !errors.Is(err, ErrClosed) {
		t.Errorf("voting on a decided poll: %v", err)
	}
	if sum, _ := s.Polls(anna.ID); sum[0].Venue != "Next to Anna" {
		t.Errorf("summary venue %q", sum[0].Venue)
	}

	// An organizer moving the place, and then the time.
	ev, err = s.PickVenue(p.ID, vs[1].ID)
	if err != nil || ev.Kind != KindChanged || !strings.HasPrefix(ev.Text, "🔁 Friday dinner is now at Middle, same time") {
		t.Fatalf("moving the place: %+v %v", ev, err)
	}
	ev, _ = s.Pick(p.ID, slots[1].ID)
	if !strings.HasPrefix(ev.Text, "🔁 Friday dinner moved to "+Label(slots[1].Start)+", same place") {
		t.Errorf("moving the time: %q", ev.Text)
	}
	if err := s.RemoveVenue(p.ID, vs[1].ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("removing the chosen place: %v", err)
	}
	if err := s.RemoveVenue(p.ID, vs[2].ID); err != nil {
		t.Error(err)
	}
}

func TestPickingAPlaceBeforeTheDecisionSticks(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, slots := placesPoll(t, s, anna, 0)
	s.Suggest(p.ID, []places.Venue{{Ref: "node/1", Name: "A", At: marienpl}, {Ref: "node/2", Name: "B", At: giesing}})
	vs, _ := s.Venues(p.ID)
	s.ToggleVenueVote(p.ID, vs[0].ID, anna.ID)
	if ev, err := s.PickVenue(p.ID, vs[1].ID); ev != nil || err != nil {
		t.Fatalf("picking before the decision: %+v %v", ev, err)
	}
	ev, _ := s.Pick(p.ID, slots[0].ID)
	if !strings.Contains(ev.Text, " at B ") {
		t.Errorf("the organizer's place was not kept: %q", ev.Text)
	}
	// A poll without places never mentions one.
	q, qslots := poll(t, s, anna, 0)
	ev, _ = s.Pick(q.ID, qslots[0].ID)
	if strings.Contains(ev.Text, " at ") {
		t.Errorf("message %q", ev.Text)
	}
}

func TestStartsAreForgottenAWeekAfter(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, slots := placesPoll(t, s, anna, 0)
	s.SetStart(p.ID, anna.ID, schwabing, "", "")
	s.Pick(p.ID, slots[0].ID)
	s.PurgeStarts(now.Add(9 * 24 * time.Hour)) // six days after
	if starts, _ := s.Starts(p.ID); len(starts) != 1 {
		t.Fatal("forgotten too soon")
	}
	s.PurgeStarts(now.Add(11 * 24 * time.Hour)) // eight days after
	if starts, _ := s.Starts(p.ID); len(starts) != 0 {
		t.Error("still kept more than a week after")
	}
}

func TestDatabasesFromVersionOneAreUpgraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The polls table as v1.0.0 made it, with a poll in it.
	if _, err := db.Exec(`CREATE TABLE polls (
		id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, category TEXT NOT NULL, deadline TEXT NOT NULL,
		quorum INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'open', chosen_slot INTEGER,
		decision_seq INTEGER NOT NULL DEFAULT 0, invite_code TEXT NOT NULL UNIQUE, invite_open INTEGER NOT NULL DEFAULT 1,
		organizer_code TEXT NOT NULL UNIQUE, chat_code TEXT NOT NULL UNIQUE, origin TEXT NOT NULL DEFAULT '',
		created_by INTEGER, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, deleted_at TIMESTAMP);
		INSERT INTO polls (title, category, deadline, invite_code, organizer_code, chat_code) VALUES ('Old', 'other', '2026-10-09T18:00', 'a', 'b', 'c');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening a v1.0 database: %v", err)
	}
	defer s.Close()
	p, err := s.Poll(1)
	if err != nil || p.Title != "Old" || p.Places {
		t.Errorf("got %+v, %v", p, err)
	}
}

func mustStarts(t *testing.T, s *Store, p Poll) []Start {
	t.Helper()
	st, err := s.Starts(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
