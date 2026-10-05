package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// now is when every test happens: a Monday afternoon.
var now = time.Date(2026, 10, 5, 14, 0, 0, 0, time.Local)

func at(days, hour int) string {
	return time.Date(2026, 10, 5+days, hour, 0, 0, 0, time.Local).Format(Stamp)
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func user(t *testing.T, s *Store, name string) User {
	t.Helper()
	u, _, err := s.CreateUser(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// poll makes a poll with three times, on Thursday, Friday and Saturday
// evening, deciding on Wednesday evening.
func poll(t *testing.T, s *Store, by User, quorum int) (Poll, []Slot) {
	t.Helper()
	p, err := s.CreatePoll(by.ID, NewPoll{
		Title: "Friday dinner", Category: "dinner", Deadline: at(2, 18), Quorum: quorum,
		Slots: []string{at(4, 19), at(3, 19), at(5, 19)}, Origin: "https://halfway.example",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := s.Slots(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, slots
}

func vote(t *testing.T, s *Store, p Poll, sl Slot, u User, answer int) *Event {
	t.Helper()
	ev, err := s.Vote(p.ID, sl.ID, u.ID, answer, now)
	if err != nil {
		t.Fatalf("%s voting: %v", u.Name, err)
	}
	return ev
}

func TestCreatePollChecksWhatItIsGiven(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	good := NewPoll{Title: "Coffee", Slots: []string{at(2, 10)}}
	cases := map[string]func(*NewPoll){
		"no title":       func(n *NewPoll) { n.Title = "  " },
		"a long title":   func(n *NewPoll) { n.Title = strings.Repeat("x", 81) },
		"no times":       func(n *NewPoll) { n.Slots = []string{"", " "} },
		"a time gone by": func(n *NewPoll) { n.Slots = []string{at(-1, 10)} },
		"nine times": func(n *NewPoll) {
			n.Slots = []string{at(1, 1), at(1, 2), at(1, 3), at(1, 4), at(1, 5), at(1, 6), at(1, 7), at(1, 8), at(1, 9)}
		},
		"not a time":           func(n *NewPoll) { n.Slots = []string{"Friday"} },
		"a deadline gone by":   func(n *NewPoll) { n.Deadline = at(-1, 10) },
		"deciding too late":    func(n *NewPoll) { n.Deadline = at(3, 10) },
		"a quorum of one":      func(n *NewPoll) { n.Quorum = 1 },
		"a negative quorum":    func(n *NewPoll) { n.Quorum = -2 },
		"a nonsense deadline":  func(n *NewPoll) { n.Deadline = "soon" },
		"a quorum of hundreds": func(n *NewPoll) { n.Quorum = 500 },
	}
	for name, spoil := range cases {
		in := good
		spoil(&in)
		if _, err := s.CreatePoll(anna.ID, in, now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}

	p, err := s.CreatePoll(anna.ID, NewPoll{Title: "  Coffee   at  noon ", Category: "tea", Slots: []string{at(3, 12), at(2, 12), at(3, 12)}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Coffee at noon" || p.Category != "other" || p.Status != StatusOpen {
		t.Errorf("got %+v", p)
	}
	// A blank deadline is a day before the first time.
	if p.Deadline != at(1, 12) {
		t.Errorf("deadline %s, want %s", p.Deadline, at(1, 12))
	}
	slots, _ := s.Slots(p.ID)
	if len(slots) != 2 || slots[0].Start != at(2, 12) {
		t.Errorf("times should be sorted and each kept once, got %+v", slots)
	}
	if org, err := s.Role(p.ID, anna.ID); err != nil || !org {
		t.Errorf("the one who made it should organize it: %v %v", org, err)
	}
	if p.InviteCode == p.OrganizerCode || len(p.InviteCode) < 12 {
		t.Errorf("links should be long and different: %q %q", p.InviteCode, p.OrganizerCode)
	}
}

func TestBlankDeadlineIsHalfwayWhenTheFirstTimeIsSoon(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, err := s.CreatePoll(anna.ID, NewPoll{Title: "Lunch", Slots: []string{at(0, 18)}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := at(0, 16); p.Deadline != want {
		t.Errorf("deadline %s, want %s (halfway between 14:00 and 18:00)", p.Deadline, want)
	}
}

func TestQuorumDecidesTheMomentItIsReached(t *testing.T) {
	s := open(t)
	anna, ben, chris := user(t, s, "Anna"), user(t, s, "Ben"), user(t, s, "Chris")
	p, slots := poll(t, s, anna, 3)
	thu, fri := slots[0], slots[1]
	s.Join(p.ID, ben.ID, false)
	s.Join(p.ID, chris.ID, false)

	if ev := vote(t, s, p, thu, anna, Yes); ev != nil {
		t.Fatal("decided after one answer")
	}
	vote(t, s, p, fri, anna, Yes)
	vote(t, s, p, fri, ben, Yes)
	if ev := vote(t, s, p, thu, ben, No); ev != nil {
		t.Fatal("decided before anything had three people")
	}
	// "If needed" is somebody who can come, so it counts.
	ev := vote(t, s, p, fri, chris, IfNeeded)
	if ev == nil || ev.Kind != KindConfirmed {
		t.Fatalf("the third person should have settled it, got %+v", ev)
	}
	if !strings.Contains(ev.Text, "Friday dinner is on: "+Label(fri.Start)) || !strings.Contains(ev.Text, "https://halfway.example/i/"+p.InviteCode) {
		t.Errorf("message: %q", ev.Text)
	}
	got, _ := s.Poll(p.ID)
	if !got.IsConfirmed() || got.ChosenSlot != fri.ID {
		t.Errorf("got %+v", got)
	}
	if _, err := s.Vote(p.ID, thu.ID, chris.ID, Yes, now); !errors.Is(err, ErrClosed) {
		t.Errorf("answering a decided poll: %v, want ErrClosed", err)
	}
}

func TestDeadlinePicksMostYesThenIfNeededThenEarliest(t *testing.T) {
	s := open(t)
	anna, ben, chris := user(t, s, "Anna"), user(t, s, "Ben"), user(t, s, "Chris")
	p, slots := poll(t, s, anna, 0)
	thu, fri, sat := slots[0], slots[1], slots[2]
	vote(t, s, p, thu, anna, Yes)
	vote(t, s, p, thu, ben, IfNeeded)
	vote(t, s, p, fri, anna, Yes)
	vote(t, s, p, fri, ben, IfNeeded)
	vote(t, s, p, fri, chris, IfNeeded)
	vote(t, s, p, sat, chris, Yes)

	// Nothing is decided before the deadline without a quorum.
	if evs, _ := s.DecideDue(now); len(evs) != 0 {
		t.Fatalf("decided early: %+v", evs)
	}
	evs, err := s.DecideDue(time.Date(2026, 10, 7, 18, 0, 0, 0, time.Local))
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %v, %v", evs, err)
	}
	got, _ := s.Poll(p.ID)
	if got.ChosenSlot != fri.ID {
		t.Errorf("chose slot %d, want Friday (%d): one yes each, and it has the most if-neededs", got.ChosenSlot, fri.ID)
	}
	// Only once.
	if evs, _ := s.DecideDue(time.Date(2026, 10, 7, 19, 0, 0, 0, time.Local)); len(evs) != 0 {
		t.Errorf("decided twice: %+v", evs)
	}

	// All else equal, the earlier time.
	top, _ := best([]Slot{{ID: 2, Start: at(4, 1), Yes: 1}, {ID: 1, Start: at(3, 1), Yes: 1}})
	if top.ID != 1 {
		t.Errorf("tie went to %d, want the earlier one", top.ID)
	}
}

func TestDeadlineCancelsWithoutQuorumOrWithoutAnybody(t *testing.T) {
	s := open(t)
	anna, ben := user(t, s, "Anna"), user(t, s, "Ben")
	p, slots := poll(t, s, anna, 3)
	vote(t, s, p, slots[0], anna, Yes)
	vote(t, s, p, slots[0], ben, Yes)
	nobody, _ := poll(t, s, anna, 0)

	evs, err := s.DecideDue(time.Date(2026, 10, 7, 18, 0, 0, 0, time.Local))
	if err != nil || len(evs) != 2 {
		t.Fatalf("got %+v, %v", evs, err)
	}
	for _, ev := range evs {
		if ev.Kind != KindCancelled {
			t.Errorf("%d: %s, want cancelled", ev.PollID, ev.Kind)
		}
	}
	if evs[0].Text != "❌ Friday dinner didn't reach 3 people and is cancelled" {
		t.Errorf("message: %q", evs[0].Text)
	}
	if evs[1].PollID != nobody.ID || !strings.Contains(evs[1].Text, "nobody could make") {
		t.Errorf("message: %q", evs[1].Text)
	}
}

func TestAnsweringAfterTheDeadlineDecidesFirst(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, slots := poll(t, s, anna, 0)
	vote(t, s, p, slots[1], anna, Yes)
	late := time.Date(2026, 10, 7, 18, 30, 0, 0, time.Local)
	ev, err := s.Vote(p.ID, slots[0].ID, anna.ID, Yes, late)
	if !errors.Is(err, ErrClosed) || ev == nil || ev.Kind != KindConfirmed {
		t.Fatalf("got %+v, %v", ev, err)
	}
	got, _ := s.Poll(p.ID)
	if got.ChosenSlot != slots[1].ID {
		t.Error("the late answer was counted")
	}
}

func TestOrganizerCanDecideNowAndOverrule(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, slots := poll(t, s, anna, 4)
	vote(t, s, p, slots[0], anna, Yes)
	ev, err := s.DecideNow(p.ID, now)
	if err != nil || ev.Kind != KindCancelled {
		t.Fatalf("deciding now without the quorum: %+v %v", ev, err)
	}
	if _, err := s.DecideNow(p.ID, now); !errors.Is(err, ErrClosed) {
		t.Errorf("deciding twice: %v", err)
	}
	// Picking a time for a cancelled poll puts it on.
	ev, err = s.Pick(p.ID, slots[2].ID)
	if err != nil || ev.Kind != KindConfirmed {
		t.Fatalf("picking: %+v %v", ev, err)
	}
	// ...and picking another moves it.
	ev, err = s.Pick(p.ID, slots[0].ID)
	if err != nil || ev.Kind != KindChanged || ev.Text != "🔁 Friday dinner moved to "+Label(slots[0].Start)+" · https://halfway.example/i/"+p.InviteCode {
		t.Fatalf("moving: %+v %v", ev, err)
	}
	if ev, err := s.Pick(p.ID, slots[0].ID); ev != nil || err != nil {
		t.Errorf("picking the time it is at already: %+v %v", ev, err)
	}
	other, otherSlots := poll(t, s, anna, 0)
	if _, err := s.Pick(p.ID, otherSlots[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("picking another poll's time: %v", err)
	}
	if _, err := s.Vote(other.ID, slots[0].ID, anna.ID, Yes, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("answering another poll's time: %v", err)
	}
	evs, _ := s.Events(p.ID)
	if len(evs) != 3 {
		t.Errorf("%d events, want 3", len(evs))
	}
}

func TestLoweringTheQuorumCanDecideAtOnce(t *testing.T) {
	s := open(t)
	anna, ben := user(t, s, "Anna"), user(t, s, "Ben")
	p, slots := poll(t, s, anna, 4)
	vote(t, s, p, slots[1], anna, Yes)
	vote(t, s, p, slots[1], ben, Yes)
	if _, err := s.Reschedule(p.ID, at(3, 20), 4, now); !errors.Is(err, ErrInvalid) {
		t.Errorf("a deadline after the first time: %v", err)
	}
	ev, err := s.Reschedule(p.ID, at(2, 12), 2, now)
	if err != nil || ev == nil || ev.Kind != KindConfirmed {
		t.Fatalf("got %+v %v", ev, err)
	}
	if _, err := s.Reschedule(p.ID, at(2, 12), 2, now); !errors.Is(err, ErrClosed) {
		t.Errorf("rescheduling a decided poll: %v", err)
	}
}

func TestLeavingTakesYourAnswersWithYou(t *testing.T) {
	s := open(t)
	anna, ben := user(t, s, "Anna"), user(t, s, "Ben")
	p, slots := poll(t, s, anna, 0)
	s.Join(p.ID, ben.ID, false)
	vote(t, s, p, slots[0], ben, Yes)
	if err := s.Leave(p.ID, anna.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("the only organizer left: %v", err)
	}
	if err := s.Leave(p.ID, ben.ID); err != nil {
		t.Fatal(err)
	}
	slots, _ = s.Slots(p.ID)
	if slots[0].Yes != 0 {
		t.Error("Ben's answer stayed after he left")
	}
	if _, err := s.Role(p.ID, ben.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Ben is still on it: %v", err)
	}
	// Leaving a decided poll keeps the answers, as the record of how it was
	// decided, but somebody who has gone is not counted as having answered.
	chris := user(t, s, "Chris")
	s.Join(p.ID, chris.ID, false)
	vote(t, s, p, slots[1], chris, Yes)
	s.Pick(p.ID, slots[1].ID)
	s.Leave(p.ID, chris.ID)
	if slots, _ := s.Slots(p.ID); slots[1].Yes != 1 {
		t.Error("leaving a decided poll took the answer away")
	}
	if sum, _ := s.Polls(anna.ID); sum[0].Answered > sum[0].People {
		t.Errorf("%d answered of %d people", sum[0].Answered, sum[0].People)
	}

	// A second organizer can let the first one go.
	s.Join(p.ID, ben.ID, true)
	s.Join(p.ID, ben.ID, false) // the invite link does not demote anybody
	if org, _ := s.Role(p.ID, ben.ID); !org {
		t.Fatal("joining by invite took Ben's organizer role away")
	}
	if err := s.Leave(p.ID, anna.ID); err != nil {
		t.Errorf("Anna could not leave with Ben organizing: %v", err)
	}
}

func TestUnseenDecisions(t *testing.T) {
	s := open(t)
	anna, ben := user(t, s, "Anna"), user(t, s, "Ben")
	p, slots := poll(t, s, anna, 2)
	s.Join(p.ID, ben.ID, false)
	vote(t, s, p, slots[0], anna, Yes)
	s.MarkSeen(p.ID, anna.ID)
	vote(t, s, p, slots[0], ben, Yes) // decides it
	sum, _ := s.Polls(anna.ID)
	if len(sum) != 1 || !sum[0].Unseen || !sum[0].Mine || sum[0].People != 2 || sum[0].Answered != 2 || sum[0].Chosen != slots[0].Start {
		t.Fatalf("got %+v", sum)
	}
	if changed, _ := s.MarkSeen(p.ID, anna.ID); !changed {
		t.Error("looking at it changed nothing")
	}
	if changed, _ := s.MarkSeen(p.ID, anna.ID); changed {
		t.Error("looking again changed something")
	}
	sum, _ = s.Polls(anna.ID)
	if sum[0].Unseen {
		t.Error("still unseen after looking")
	}
	// Somebody who joins after the decision has nothing new to see.
	chris := user(t, s, "Chris")
	s.Join(p.ID, chris.ID, false)
	if sum, _ := s.Polls(chris.ID); sum[0].Unseen || sum[0].Mine {
		t.Errorf("got %+v", sum[0])
	}
}

func TestInviteLinks(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, _ := poll(t, s, anna, 0)
	if err := s.SetInviteOpen(p.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.PollByInvite(p.InviteCode)
	if err != nil || got.InviteOpen {
		t.Errorf("a closed link should still find its poll, closed: %+v %v", got, err)
	}
	s.ResetInvite(p.ID)
	if _, err := s.PollByInvite(p.InviteCode); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old link still works: %v", err)
	}
	got, _ = s.Poll(p.ID)
	if !got.InviteOpen {
		t.Error("a new link should start open")
	}
	s.ResetOrganizerLink(p.ID)
	if _, err := s.PollByOrganizerCode(p.OrganizerCode); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old organizer link still works: %v", err)
	}
}

func TestDeleteAndRestore(t *testing.T) {
	s := open(t)
	anna, ben := user(t, s, "Anna"), user(t, s, "Ben")
	p, _ := poll(t, s, anna, 0)
	s.Join(p.ID, ben.ID, false)
	if err := s.DeletePoll(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted poll is still there: %v", err)
	}
	if _, err := s.PollByInvite(p.InviteCode); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted poll's link still works: %v", err)
	}
	if err := s.RestorePoll(p.ID, ben.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("somebody who does not organize it restored it: %v", err)
	}
	if err := s.RestorePoll(p.ID, anna.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(p.ID); err != nil {
		t.Error(err)
	}
}

func TestChatsHearEachDecisionOnce(t *testing.T) {
	s := open(t)
	anna := user(t, s, "Anna")
	p, slots := poll(t, s, anna, 0)
	if _, err := s.LinkChat("nonsense", "telegram", "-100", "Friends"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a made-up code linked a chat: %v", err)
	}
	got, err := s.LinkChat(p.ChatCode, "telegram", "-100", "Friends")
	if err != nil || got != p.Title {
		t.Fatal(err)
	}
	s.LinkChat(p.ChatCode, "telegram", "-100", "Friends!") // twice is once
	if chats, _ := s.Chats(p.ID); len(chats) != 1 || chats[0].Title != "Friends!" {
		t.Fatalf("got %+v", chats)
	}
	if pend, _ := s.Pending(); len(pend) != 0 {
		t.Fatalf("something to send before anything was decided: %+v", pend)
	}
	s.Pick(p.ID, slots[0].ID)
	pend, _ := s.Pending()
	if len(pend) != 1 || pend[0].Target != "-100" || !strings.HasPrefix(pend[0].Text, "✅") {
		t.Fatalf("got %+v", pend)
	}
	s.Delivered(pend[0].EventID, pend[0].ChatID)
	if pend, _ := s.Pending(); len(pend) != 0 {
		t.Errorf("sent twice: %+v", pend)
	}

	// A broken chat is not sent anything, until it is connected again.
	s.ChatBroken("telegram", "-100")
	s.Pick(p.ID, slots[1].ID)
	if pend, _ := s.Pending(); len(pend) != 0 {
		t.Errorf("sent to a broken chat: %+v", pend)
	}
	s.ChatMoved("telegram", "-100", "-200")
	s.LinkChat(p.ChatCode, "telegram", "-200", "Friends")
	pend, _ = s.Pending()
	if len(pend) != 1 || pend[0].Target != "-200" {
		t.Errorf("got %+v", pend)
	}
	chats, _ := s.Chats(p.ID)
	if err := s.RemoveChat(p.ID, chats[0].ID); err != nil {
		t.Fatal(err)
	}
	if pend, _ := s.Pending(); len(pend) != 0 {
		t.Errorf("sent to a removed chat: %+v", pend)
	}
}

func TestNotifierHearsFromEveryoneOnThePoll(t *testing.T) {
	s := open(t)
	anna, ben, chris := user(t, s, "Anna"), user(t, s, "Ben"), user(t, s, "Chris")
	p, slots := poll(t, s, anna, 0)
	s.Join(p.ID, ben.ID, false)
	var heard map[int64]bool
	s.SetNotifier(func(ids []int64) {
		for _, id := range ids {
			heard[id] = true
		}
	})
	heard = map[int64]bool{}
	vote(t, s, p, slots[0], ben, Yes)
	if !heard[anna.ID] || !heard[ben.ID] || heard[chris.ID] {
		t.Errorf("heard %v", heard)
	}
	heard = map[int64]bool{}
	s.RenameUser(ben.ID, "Benjamin")
	if !heard[anna.ID] || !heard[ben.ID] || heard[chris.ID] {
		t.Errorf("a rename was heard by %v", heard)
	}
}
