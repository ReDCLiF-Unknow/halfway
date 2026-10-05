package store

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Event is a decision: a poll confirmed, cancelled, or moved to another time.
// Each one is posted once to every chat connected to the poll.
type Event struct {
	ID     int64  `json:"id"`
	PollID int64  `json:"poll_id"`
	Kind   string `json:"kind"` // confirmed, cancelled, changed
	Text   string `json:"text"`
}

// Kinds of event.
const (
	KindConfirmed = "confirmed"
	KindCancelled = "cancelled"
	KindChanged   = "changed"
)

// best is the time most people said yes to, with "if needed" breaking a tie,
// and the earlier time a tie after that. ok is false when there are none.
func best(slots []Slot) (Slot, bool) {
	var top Slot
	found := false
	for _, s := range slots {
		switch {
		case !found:
		case s.Yes != top.Yes:
			if s.Yes < top.Yes {
				continue
			}
		case s.IfNeeded != top.IfNeeded:
			if s.IfNeeded < top.IfNeeded {
				continue
			}
		case s.Start >= top.Start:
			continue
		}
		top, found = s, true
	}
	return top, found
}

// Leading is the best time that anybody can make so far, and whether there
// is one: what the poll is heading for, quorum or not.
func Leading(slots []Slot) (Slot, bool) {
	var some []Slot
	for _, s := range slots {
		if s.CanCome() > 0 {
			some = append(some, s)
		}
	}
	return best(some)
}

// Outcome is what a poll comes to given the answers so far: still open,
// confirmed for a time, or cancelled. due is whether it has to be decided now,
// because its deadline has passed or an organizer asked.
//
// With a quorum, the first time enough people can make ("if needed" counts:
// they said they can come) settles it at once, and if none has enough by the
// deadline it is off. Without one, the deadline settles it on the best time
// anybody can make, and it is off only if nobody can make any.
func Outcome(p Poll, slots []Slot, due bool) (status string, slotID int64) {
	var candidates []Slot
	for _, s := range slots {
		if s.CanCome() > 0 && (p.Quorum == 0 || s.CanCome() >= p.Quorum) {
			candidates = append(candidates, s)
		}
	}
	if p.Quorum == 0 && !due {
		return StatusOpen, 0
	}
	if top, ok := best(candidates); ok {
		return StatusConfirmed, top.ID
	}
	if due {
		return StatusCancelled, 0
	}
	return StatusOpen, 0
}

// decide settles poll p, inside tx, if its answers or its deadline say it is
// time. It returns the decision, or nil if it stays open.
func (s *Store) decide(tx *sql.Tx, p Poll, now time.Time, force bool) (*Event, error) {
	if !p.IsOpen() {
		return nil, nil
	}
	slots, err := slotsOf(tx, p.ID)
	if err != nil {
		return nil, err
	}
	due := force || p.Deadline <= now.Format(Stamp)
	status, slotID := Outcome(p, slots, due)
	if status == StatusOpen {
		return nil, nil
	}
	return s.settle(tx, p, status, slotID, 0, slots)
}

// settle records a decision and the message that announces it. venueID is
// the place, for a poll that finds one: 0 keeps the one chosen already, or
// for a poll that has none yet, settles on the best of its places.
func (s *Store) settle(tx *sql.Tx, p Poll, status string, slotID, venueID int64, slots []Slot) (*Event, error) {
	if venueID == 0 {
		venueID = p.ChosenVenue
	}
	if status == StatusConfirmed && p.Places && venueID == 0 {
		venues, err := venuesOf(tx, p.ID)
		if err != nil {
			return nil, err
		}
		starts, err := startsOf(tx, p.ID)
		if err != nil {
			return nil, err
		}
		if v, ok := BestVenue(venues, starts); ok {
			venueID = v.ID
		}
	}
	var chosenSlot, chosenVenue any
	if slotID != 0 {
		chosenSlot = slotID
	}
	if venueID != 0 {
		chosenVenue = venueID
	}
	if _, err := tx.Exec(`UPDATE polls SET status = ?, chosen_slot = ?, chosen_venue = ?, decision_seq = decision_seq + 1 WHERE id = ?`,
		status, chosenSlot, chosenVenue, p.ID); err != nil {
		return nil, err
	}
	// The group's memory: how long everyone took to get there.
	if p.Places {
		kept := venueID
		if status != StatusConfirmed {
			kept = 0
		}
		if err := recordTrips(tx, p, kept); err != nil {
			return nil, err
		}
	}
	kind := status
	if p.IsConfirmed() && status == StatusConfirmed {
		kind = KindChanged
	}
	venue := ""
	if venueID != 0 && status == StatusConfirmed {
		if err := tx.QueryRow(`SELECT name FROM venues WHERE id = ?`, venueID).Scan(&venue); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	moved := kind == KindChanged && slotID != p.ChosenSlot
	ev := &Event{PollID: p.ID, Kind: kind, Text: message(p, kind, slotID, slots, venue, moved)}
	res, err := tx.Exec(`INSERT INTO events (poll_id, kind, text) VALUES (?, ?, ?)`, ev.PollID, ev.Kind, ev.Text)
	if err != nil {
		return nil, err
	}
	ev.ID, _ = res.LastInsertId()
	return ev, nil
}

// message is what a chat is told about a decision. venue is the place's
// name, if it has one; moved says whether a change was to the time (rather
// than only to the place).
func message(p Poll, kind string, slotID int64, slots []Slot, venue string, moved bool) string {
	when := ""
	for _, s := range slots {
		if s.ID == slotID {
			when = Label(s.Start)
		}
	}
	link := ""
	if p.Origin != "" {
		link = " · " + p.Origin + "/i/" + p.InviteCode
	}
	at := ""
	if venue != "" {
		at = " at " + venue
	}
	switch {
	case kind == KindConfirmed:
		return "✅ " + p.Title + " is on: " + when + at + link
	case kind == KindChanged && moved && venue != "":
		return "🔁 " + p.Title + " moved to " + when + ", same place" + link
	case kind == KindChanged && moved:
		return "🔁 " + p.Title + " moved to " + when + link
	case kind == KindChanged:
		return "🔁 " + p.Title + " is now" + at + ", same time" + link
	}
	if p.Quorum > 0 {
		return "❌ " + p.Title + " didn't reach " + strconv.Itoa(p.Quorum) + " people and is cancelled"
	}
	return "❌ " + p.Title + " is cancelled: nobody could make any of the times"
}

// DecideIfDue settles one poll if its deadline has passed.
func (s *Store) DecideIfDue(pollID int64, now time.Time) (*Event, error) {
	// Every view of a poll asks, and nearly always the answer is no. Find
	// that out with a read, rather than taking the write lock to find it out.
	var due bool
	err := s.db.QueryRow(`SELECT status = 'open' AND deadline <= ? FROM polls WHERE id = ? AND deleted_at IS NULL`,
		now.Format(Stamp), pollID).Scan(&due)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil || !due {
		return nil, err
	}
	return s.decideOne(pollID, now, false)
}

// DecideNow settles an open poll straight away, as its organizer asked,
// exactly as its deadline would have.
func (s *Store) DecideNow(pollID int64, now time.Time) (*Event, error) {
	ev, err := s.decideOne(pollID, now, true)
	if err == nil && ev == nil {
		return nil, ErrClosed
	}
	return ev, err
}

func (s *Store) decideOne(pollID int64, now time.Time, force bool) (*Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := scanPoll(tx.QueryRow(pollSelect+`WHERE id = ? AND deleted_at IS NULL`, pollID))
	if err != nil {
		return nil, err
	}
	ev, err := s.decide(tx, p, now, force)
	if err != nil || ev == nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.changed(pollID)
	return ev, nil
}

// DecideDue settles every open poll whose deadline has passed. The server
// calls it every few seconds.
func (s *Store) DecideDue(now time.Time) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id FROM polls WHERE status = 'open' AND deleted_at IS NULL AND deadline <= ?`, now.Format(Stamp))
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var events []Event
	for _, id := range ids {
		ev, err := s.DecideIfDue(id, now)
		if err != nil {
			return events, err
		}
		if ev != nil {
			events = append(events, *ev)
		}
	}
	return events, nil
}

// Pick is an organizer overruling the poll: it happens at this time, whatever
// the answers say, and whether or not it was decided already.
func (s *Store) Pick(pollID, slotID int64) (*Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := scanPoll(tx.QueryRow(pollSelect+`WHERE id = ? AND deleted_at IS NULL`, pollID))
	if err != nil {
		return nil, err
	}
	if p.IsConfirmed() && p.ChosenSlot == slotID {
		return nil, nil // it is at that time already
	}
	slots, err := slotsOf(tx, pollID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, sl := range slots {
		found = found || sl.ID == slotID
	}
	if !found {
		return nil, ErrNotFound
	}
	ev, err := s.settle(tx, p, StatusConfirmed, slotID, 0, slots)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.changed(pollID)
	return ev, nil
}

// ---- chats ------------------------------------------------------------

// Chat is a group chat told about a poll's decisions.
type Chat struct {
	ID       int64  `json:"id"`
	PollID   int64  `json:"poll_id"`
	Platform string `json:"platform"`
	Target   string `json:"-"`
	Title    string `json:"title"`
	// Broken is set when the chat stopped taking messages: the bot was
	// removed, or the group deleted. Connecting it again mends it.
	Broken bool `json:"broken"`
}

// LinkChat connects a chat to the poll, or the group, whose connect code it
// was given, and returns that poll's or group's name. Connecting the same
// chat again mends it if it was broken.
func (s *Store) LinkChat(code, platform, target, title string) (string, error) {
	p, err := scanPoll(s.db.QueryRow(pollSelect+`WHERE chat_code = ? AND deleted_at IS NULL`, code))
	if errors.Is(err, ErrNotFound) {
		var group int64
		var name string
		if err := s.db.QueryRow(`SELECT id, name FROM groups WHERE chat_code = ?`, code).Scan(&group, &name); errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		} else if err != nil {
			return "", err
		}
		return name, s.AddGroupChat(group, platform, target, title)
	}
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT INTO chats (poll_id, platform, target, title) VALUES (?, ?, ?, ?)
		ON CONFLICT (poll_id, platform, target) DO UPDATE SET title = excluded.title, broken = 0`,
		p.ID, platform, target, title); err != nil {
		return "", err
	}
	s.changed(p.ID)
	return p.Title, nil
}

// AddChat connects a chat to a poll directly, as a pasted webhook does.
// Connecting the same one again mends it if it was broken.
func (s *Store) AddChat(pollID int64, platform, target, title string) error {
	if _, err := s.db.Exec(`INSERT INTO chats (poll_id, platform, target, title) VALUES (?, ?, ?, ?)
		ON CONFLICT (poll_id, platform, target) DO UPDATE SET title = excluded.title, broken = 0`,
		pollID, platform, target, cleanLabel(title, maxTitleLen)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// Chats is every chat connected to a poll, in the order they were connected.
func (s *Store) Chats(pollID int64) ([]Chat, error) {
	rows, err := s.db.Query(`SELECT id, poll_id, platform, target, title, broken FROM chats WHERE poll_id = ? ORDER BY id`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chats := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.PollID, &c.Platform, &c.Target, &c.Title, &c.Broken); err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// RemoveChat disconnects a chat from a poll.
func (s *Store) RemoveChat(pollID, chatID int64) error {
	if err := affected(s.db.Exec(`DELETE FROM chats WHERE id = ? AND poll_id = ?`, chatID, pollID)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// ChatBroken marks every connection to a chat as broken, so its organizers
// are asked to reconnect it.
func (s *Store) ChatBroken(platform, target string) error {
	return s.eachChatPoll(platform, target, `UPDATE chats SET broken = 1 WHERE platform = ? AND target = ?`, platform, target)
}

// ChatMoved follows a chat that has a new id, as a Telegram group gets when
// it becomes a supergroup.
func (s *Store) ChatMoved(platform, from, to string) error {
	return s.eachChatPoll(platform, from, `UPDATE OR IGNORE chats SET target = ? WHERE platform = ? AND target = ?`, to, platform, from)
}

func (s *Store) eachChatPoll(platform, target, query string, args ...any) error {
	rows, err := s.db.Query(`SELECT DISTINCT poll_id FROM chats WHERE platform = ? AND target = ?`, platform, target)
	if err != nil {
		return err
	}
	var polls []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		polls = append(polls, id)
	}
	rows.Close()
	if _, err := s.db.Exec(query, args...); err != nil {
		return err
	}
	for _, id := range polls {
		s.changed(id)
	}
	return nil
}

// Delivery is one message owed to one chat.
type Delivery struct {
	EventID  int64
	ChatID   int64
	Platform string
	Target   string
	Text     string
}

// Pending is every decision not yet posted to a chat that should hear about
// it. A chat hears about decisions made after it was connected, and none
// older than a day: a server that was down for a week does not wake up and
// post a week of stale news.
func (s *Store) Pending() ([]Delivery, error) {
	rows, err := s.db.Query(`SELECT e.id, c.id, c.platform, c.target, e.text
		FROM events e JOIN chats c ON c.poll_id = e.poll_id JOIN polls p ON p.id = e.poll_id
		WHERE c.broken = 0 AND p.deleted_at IS NULL AND c.created_at <= e.created_at
		  AND e.created_at >= datetime('now', '-1 day')
		  AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = e.id AND d.chat_id = c.id)
		ORDER BY e.id, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.EventID, &d.ChatID, &d.Platform, &d.Target, &d.Text); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Delivered records that a chat has been told about a decision, so it is
// never told twice.
func (s *Store) Delivered(eventID, chatID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO deliveries (event_id, chat_id) VALUES (?, ?)`, eventID, chatID)
	return err
}

// Events is a poll's decisions, oldest first.
func (s *Store) Events(pollID int64) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, poll_id, kind, text FROM events WHERE poll_id = ? ORDER BY id`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.PollID, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
