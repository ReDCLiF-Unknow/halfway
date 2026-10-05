package store

import (
	"errors"
	"testing"

	"halfway/internal/places"
)

func TestGroupPollsTakeInEveryone(t *testing.T) {
	s := open(t)
	anna, ben, chris, dana := user(t, s, "Anna"), user(t, s, "Ben"), user(t, s, "Chris"), user(t, s, "Dana")
	g, err := s.CreateGroup(anna.ID, "  Dinner   club ")
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "Dinner club" || !g.Organizer || g.Members != 1 {
		t.Fatalf("got %+v", g)
	}
	if err := s.JoinGroup(g.ID, ben.ID); err != nil {
		t.Fatal(err)
	}
	s.AddGroupChat(g.ID, "telegram", "-100", "Club chat")

	p, err := s.CreatePoll(anna.ID, NewPoll{Title: "October dinner", GroupID: g.ID, Slots: []string{at(3, 19)}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.GroupID != g.ID {
		t.Errorf("group %d", p.GroupID)
	}
	people, _ := s.Participants(p.ID)
	if len(people) != 2 {
		t.Errorf("%d people on the group's poll, want both members", len(people))
	}
	if chats, _ := s.Chats(p.ID); len(chats) != 1 || chats[0].Title != "Club chat" {
		t.Errorf("the group's chat was not carried over: %+v", chats)
	}
	// Somebody joining the group later is on its open polls.
	s.JoinGroup(g.ID, chris.ID)
	if _, err := s.Role(p.ID, chris.ID); err != nil {
		t.Errorf("Chris, joining the group, is not on its open poll: %v", err)
	}
	// Only members make polls for a group, or see it.
	if _, err := s.CreatePoll(dana.ID, NewPoll{Title: "Mine", GroupID: g.ID, Slots: []string{at(3, 19)}}, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("an outsider made a poll for the group: %v", err)
	}
	if _, err := s.Group(g.ID, dana.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("an outsider saw the group: %v", err)
	}
	if sum, _ := s.Polls(ben.ID); len(sum) != 1 || sum[0].GroupName != "Dinner club" {
		t.Errorf("summary %+v", sum)
	}
	// A Telegram group connecting itself with the group's code joins its
	// open polls too.
	name, err := s.LinkChat(g.ChatCode, "telegram", "-200", "Second chat")
	if err != nil || name != "Dinner club" {
		t.Fatalf("linking by the group's code: %q %v", name, err)
	}
	if chats, _ := s.Chats(p.ID); len(chats) != 2 {
		t.Errorf("%d chats on the open poll", len(chats))
	}
	if err := s.LeaveGroup(g.ID, anna.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("the only organizer left: %v", err)
	}
	if err := s.LeaveGroup(g.ID, ben.ID); err != nil {
		t.Error(err)
	}
	if err := s.DeleteGroup(g.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Poll(p.ID); got.GroupID != 0 {
		t.Error("a deleted group's poll still belongs to it")
	}
}

func TestRotationMemoryEvensOutTheTravelling(t *testing.T) {
	s := open(t)
	anna, ben, priya := user(t, s, "Anna"), user(t, s, "Ben"), user(t, s, "Priya")
	g, _ := s.CreateGroup(anna.ID, "Dinner club")
	s.JoinGroup(g.ID, ben.ID)
	s.JoinGroup(g.ID, priya.ID)
	meetup := func(title string) (Poll, []Slot) {
		p, err := s.CreatePoll(anna.ID, NewPoll{Title: title, GroupID: g.ID, Places: true, Slots: []string{at(3, 19)}}, now)
		if err != nil {
			t.Fatal(err)
		}
		s.SetStart(p.ID, anna.ID, schwabing, "", "transit")
		s.SetStart(p.ID, ben.ID, giesing, "", "transit")
		s.SetStart(p.ID, priya.ID, pasing, "", "transit")
		slots, _ := s.Slots(p.ID)
		return p, slots
	}

	// The first meetup is in Schwabing, which is a long way for Priya.
	first, slots := meetup("September")
	s.Suggest(first.ID, []places.Venue{{Ref: "node/1", Name: "Schwabinger Wirt", At: schwabing}})
	s.Pick(first.ID, slots[0].ID)
	shares, _ := s.Shares(g.ID)
	if shares[0].Name != "Priya" || shares[0].Extra <= 0 || shares[len(shares)-1].Extra >= 0 || shares[0].Meetups != 1 {
		t.Fatalf("shares after the first meetup: %+v", shares)
	}

	// Next time Priya's trips count for more, and the store says so.
	second, _ := meetup("October")
	starts, _ := s.Starts(second.ID)
	owed := map[string]float64{}
	for _, st := range starts {
		owed[st.Name] = st.Owed
	}
	if !(owed["Priya"] > owed["Ben"] && owed["Ben"] > 0 && owed["Anna"] < 0) {
		t.Errorf("owed: %v; Priya travelled farthest, Anna hardly at all", owed)
	}
	// Two places almost equally fair to everyone: the one easier for Priya wins.
	nearPriya := places.Point{Lat: 48.1440, Lon: 11.5450}
	nearAnna := places.Point{Lat: 48.1500, Lon: 11.5800}
	plain := []places.Start{
		{ID: anna.ID, At: schwabing, Mode: "transit"}, {ID: ben.ID, At: giesing, Mode: "transit"}, {ID: priya.ID, At: pasing, Mode: "transit"},
	}
	fNearPriya, fNearAnna := places.Measure(nearPriya, plain), places.Measure(nearAnna, plain)
	if d := fNearPriya.Longest - fNearAnna.Longest; d < -10 || d > 10 {
		t.Fatalf("the two places should be close calls without memory: %d vs %d", fNearPriya.Longest, fNearAnna.Longest)
	}
	got := places.Suggest([]places.Venue{{Name: "Near Anna", At: nearAnna}, {Name: "Near Priya", At: nearPriya}}, PlacesStarts(starts), 1)
	if got[0].Name != "Near Priya" {
		t.Errorf("with the memory, chose %q", got[0].Name)
	}
	// A meetup alone, or one called off, teaches the memory nothing.
	if first.ID == second.ID {
		t.Fatal("same poll")
	}
	s.DecideNow(second.ID, now) // nobody answered: called off
	if shares2, _ := s.Shares(g.ID); shares2[0].Meetups != 1 {
		t.Errorf("a called-off meetup counted: %+v", shares2)
	}
}
