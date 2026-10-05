package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Stamp is how times are kept and typed: a local date and time to the
// minute, as a datetime-local input writes it. Everything is in the server's
// time zone, which is what "Thursday 19:30" means to everyone on a poll.
const Stamp = "2006-01-02T15:04"

// Answers to a time. A time somebody has not marked counts as No.
const (
	No       = 0
	IfNeeded = 1
	Yes      = 2
)

// Poll statuses.
const (
	StatusOpen      = "open"
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
)

// MaxSlots is how many times one poll offers at most. More than this and
// nobody reads them all.
const MaxSlots = 8

// Categories, in the order the form offers them.
var Categories = []string{"coffee", "dinner", "drinks", "hike", "other"}

type Poll struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Deadline string `json:"deadline"` // Stamp
	Quorum   int    `json:"quorum"`   // 0: no minimum
	Status   string `json:"status"`
	// ChosenSlot is the time it was decided for, once confirmed.
	ChosenSlot    int64  `json:"chosen_slot,omitempty"`
	DecisionSeq   int    `json:"-"`
	InviteCode    string `json:"-"`
	InviteOpen    bool   `json:"-"`
	OrganizerCode string `json:"-"`
	ChatCode      string `json:"-"`
	Origin        string `json:"-"`
	CreatedBy     int64  `json:"-"`
	// Places is whether the poll also finds somewhere to meet, and
	// ChosenVenue the place it settled on.
	Places      bool  `json:"places"`
	ChosenVenue int64 `json:"chosen_venue,omitempty"`
}

func (p Poll) IsOpen() bool      { return p.Status == StatusOpen }
func (p Poll) IsConfirmed() bool { return p.Status == StatusConfirmed }
func (p Poll) IsCancelled() bool { return p.Status == StatusCancelled }

// Slot is one of the times a poll offers, with everybody's answers to it.
type Slot struct {
	ID       int64  `json:"id"`
	Start    string `json:"start"` // Stamp
	Votes    []Vote `json:"votes"` // yes first, then if needed, then no
	Yes      int    `json:"yes"`
	IfNeeded int    `json:"if_needed"`
	No       int    `json:"no"`
}

// CanCome is how many people could make it, if needed or not.
func (s Slot) CanCome() int { return s.Yes + s.IfNeeded }

// Answer is what userID said about this time, and whether they said anything.
func (s Slot) Answer(userID int64) (int, bool) {
	for _, v := range s.Votes {
		if v.UserID == userID {
			return v.Answer, true
		}
	}
	return No, false
}

type Vote struct {
	UserID int64  `json:"user_id"`
	Name   string `json:"name"`
	Answer int    `json:"answer"`
}

type Participant struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Organizer bool   `json:"organizer"`
	// Answered is whether they have said anything about any of the times.
	Answered bool `json:"answered"`
}

// NewPoll is what the form for a new poll asks for.
type NewPoll struct {
	Title, Category, Deadline string
	Quorum                    int
	Slots                     []string // Stamps
	Origin                    string
	Places                    bool
}

func parseStamp(s string) (time.Time, bool) {
	t, err := time.ParseInLocation(Stamp, strings.TrimSpace(s), time.Local)
	return t, err == nil
}

// Label writes a time the way people say it: "Thu 1 Oct, 19:30".
func Label(stamp string) string {
	t, ok := parseStamp(stamp)
	if !ok {
		return stamp
	}
	return t.Format("Mon 2 Jan, 15:04")
}

func cleanTitle(title string) (string, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" || utf8.RuneCountInString(title) > maxTitleLen {
		return "", ErrInvalid
	}
	return title, nil
}

// cleanSlots checks the times a poll offers: at least one, at most MaxSlots,
// each in the future, and each once. They come back in order.
func cleanSlots(in []string, now time.Time) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue // an empty row on the form
		}
		t, ok := parseStamp(s)
		if !ok || !t.After(now) {
			return nil, ErrInvalid
		}
		s = t.Format(Stamp)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 || len(out) > MaxSlots {
		return nil, ErrInvalid
	}
	slices.Sort(out)
	return out, nil
}

// cleanDeadline checks when a poll decides: in the future, and no later than
// its first time, which would otherwise pass while people are still answering.
// A blank one is filled in: a day before the first time, or halfway there if
// that is sooner than a day away.
func cleanDeadline(deadline, first string, now time.Time) (string, error) {
	start, _ := parseStamp(first)
	if strings.TrimSpace(deadline) == "" {
		d := start.Add(-24 * time.Hour)
		if !d.After(now.Add(time.Hour)) {
			d = now.Add(start.Sub(now) / 2)
		}
		return d.Truncate(time.Minute).Format(Stamp), nil
	}
	d, ok := parseStamp(deadline)
	if !ok || !d.After(now) || d.After(start) {
		return "", ErrInvalid
	}
	return d.Format(Stamp), nil
}

func cleanQuorum(q int) (int, error) {
	if q < 0 || q > 100 || q == 1 {
		return 0, ErrInvalid // a quorum of one is no quorum
	}
	return q, nil
}

func cleanCategory(c string) string {
	if slices.Contains(Categories, c) {
		return c
	}
	return "other"
}

// CreatePoll makes a poll with userID as its organizer.
func (s *Store) CreatePoll(userID int64, in NewPoll, now time.Time) (Poll, error) {
	title, err := cleanTitle(in.Title)
	if err != nil {
		return Poll{}, err
	}
	slots, err := cleanSlots(in.Slots, now)
	if err != nil {
		return Poll{}, err
	}
	deadline, err := cleanDeadline(in.Deadline, slots[0], now)
	if err != nil {
		return Poll{}, err
	}
	quorum, err := cleanQuorum(in.Quorum)
	if err != nil {
		return Poll{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Poll{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO polls (title, category, deadline, quorum, invite_code, organizer_code, chat_code, origin, created_by, places)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		title, cleanCategory(in.Category), deadline, quorum, newCode(), newCode(), newCode(), in.Origin, userID, in.Places)
	if err != nil {
		return Poll{}, err
	}
	id, _ := res.LastInsertId()
	for _, start := range slots {
		if _, err := tx.Exec(`INSERT INTO slots (poll_id, starts) VALUES (?, ?)`, id, start); err != nil {
			return Poll{}, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO participants (poll_id, user_id, organizer) VALUES (?, ?, 1)`, id, userID); err != nil {
		return Poll{}, err
	}
	if err := tx.Commit(); err != nil {
		return Poll{}, err
	}
	s.changedUsers(userID)
	return s.Poll(id)
}

const pollSelect = `SELECT id, title, category, deadline, quorum, status, COALESCE(chosen_slot, 0), decision_seq,
	invite_code, invite_open, organizer_code, chat_code, origin, COALESCE(created_by, 0), places, COALESCE(chosen_venue, 0) FROM polls `

func scanPoll(row interface{ Scan(...any) error }) (Poll, error) {
	var p Poll
	err := row.Scan(&p.ID, &p.Title, &p.Category, &p.Deadline, &p.Quorum, &p.Status, &p.ChosenSlot, &p.DecisionSeq,
		&p.InviteCode, &p.InviteOpen, &p.OrganizerCode, &p.ChatCode, &p.Origin, &p.CreatedBy, &p.Places, &p.ChosenVenue)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// Poll is one poll, as long as it has not been deleted.
func (s *Store) Poll(id int64) (Poll, error) {
	return scanPoll(s.db.QueryRow(pollSelect+`WHERE id = ? AND deleted_at IS NULL`, id))
}

// PollByInvite finds the poll an invite link is for, closed or not. A link
// that has been replaced by a new one finds nothing.
func (s *Store) PollByInvite(code string) (Poll, error) {
	return scanPoll(s.db.QueryRow(pollSelect+`WHERE invite_code = ? AND deleted_at IS NULL`, code))
}

// PollByOrganizerCode finds the poll an organizer link is for.
func (s *Store) PollByOrganizerCode(code string) (Poll, error) {
	return scanPoll(s.db.QueryRow(pollSelect+`WHERE organizer_code = ? AND deleted_at IS NULL`, code))
}

// Role says whether userID is on the poll, and whether they organize it.
// Someone who is not on it gets ErrNotFound, as if it did not exist.
func (s *Store) Role(pollID, userID int64) (organizer bool, err error) {
	err = s.db.QueryRow(`SELECT pa.organizer FROM participants pa JOIN polls p ON p.id = pa.poll_id
		WHERE pa.poll_id = ? AND pa.user_id = ? AND p.deleted_at IS NULL`, pollID, userID).Scan(&organizer)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return organizer, err
}

// Slots is a poll's times, soonest first, with who said what about each.
func (s *Store) Slots(pollID int64) ([]Slot, error) {
	return slotsOf(s.db, pollID)
}

type querier interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func slotsOf(q querier, pollID int64) ([]Slot, error) {
	rows, err := q.Query(`SELECT s.id, s.starts, v.user_id, u.name, v.answer
		FROM slots s LEFT JOIN votes v ON v.slot_id = s.id LEFT JOIN users u ON u.id = v.user_id
		WHERE s.poll_id = ? ORDER BY s.starts, s.id, v.answer DESC, u.name COLLATE NOCASE`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slots := []Slot{}
	for rows.Next() {
		var (
			id     int64
			start  string
			uid    sql.NullInt64
			name   sql.NullString
			answer sql.NullInt64
		)
		if err := rows.Scan(&id, &start, &uid, &name, &answer); err != nil {
			return nil, err
		}
		if n := len(slots); n == 0 || slots[n-1].ID != id {
			slots = append(slots, Slot{ID: id, Start: start, Votes: []Vote{}})
		}
		if !uid.Valid {
			continue
		}
		sl := &slots[len(slots)-1]
		sl.Votes = append(sl.Votes, Vote{UserID: uid.Int64, Name: name.String, Answer: int(answer.Int64)})
		switch answer.Int64 {
		case Yes:
			sl.Yes++
		case IfNeeded:
			sl.IfNeeded++
		default:
			sl.No++
		}
	}
	return slots, rows.Err()
}

// Participants is everyone on a poll, organizers first, then in the order
// they joined.
func (s *Store) Participants(pollID int64) ([]Participant, error) {
	rows, err := s.db.Query(`SELECT u.id, u.name, pa.organizer,
		EXISTS (SELECT 1 FROM votes v JOIN slots sl ON sl.id = v.slot_id WHERE sl.poll_id = pa.poll_id AND v.user_id = u.id)
		FROM participants pa JOIN users u ON u.id = pa.user_id
		WHERE pa.poll_id = ? ORDER BY pa.organizer DESC, pa.joined_at, u.id`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	people := []Participant{}
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.ID, &p.Name, &p.Organizer, &p.Answered); err != nil {
			return nil, err
		}
		people = append(people, p)
	}
	return people, rows.Err()
}

// Join puts userID on a poll. Joining through the organizer link makes them
// an organizer; joining again through the invite link never takes that away.
func (s *Store) Join(pollID, userID int64, organizer bool) error {
	_, err := s.db.Exec(`INSERT INTO participants (poll_id, user_id, organizer, seen_seq)
		VALUES (?, ?, ?, (SELECT decision_seq FROM polls WHERE id = ?))
		ON CONFLICT (poll_id, user_id) DO UPDATE SET organizer = MAX(organizer, excluded.organizer)`,
		pollID, userID, organizer, pollID)
	if err == nil {
		s.changed(pollID)
	}
	return err
}

// Leave takes userID off a poll, and their answers with them: somebody who
// has left is not coming. The last organizer cannot leave, as then nobody
// could look after the poll.
func (s *Store) Leave(pollID, userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var organizer bool
	if err := tx.QueryRow(`SELECT organizer FROM participants WHERE poll_id = ? AND user_id = ?`, pollID, userID).Scan(&organizer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if organizer {
		var others int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM participants WHERE poll_id = ? AND organizer = 1 AND user_id <> ?`,
			pollID, userID).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrInvalid
		}
	}
	// Everyone still on it hears about it, and so does the one leaving, whose
	// sidebar loses the poll.
	var ids []int64
	rows, err := tx.Query(`SELECT user_id FROM participants WHERE poll_id = ?`, pollID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var status string
	if err := tx.QueryRow(`SELECT status FROM polls WHERE id = ?`, pollID).Scan(&status); err != nil {
		return err
	}
	// Once decided, the answers are the record of how it was decided.
	if status == StatusOpen {
		if _, err := tx.Exec(`DELETE FROM votes WHERE user_id = ? AND slot_id IN (SELECT id FROM slots WHERE poll_id = ?)`,
			userID, pollID); err != nil {
			return err
		}
	}
	if status == StatusOpen {
		if _, err := tx.Exec(`DELETE FROM venue_votes WHERE user_id = ? AND venue_id IN (SELECT id FROM venues WHERE poll_id = ?)`,
			userID, pollID); err != nil {
			return err
		}
	}
	// Where they set off from was only ever for this poll.
	if _, err := tx.Exec(`DELETE FROM starts WHERE poll_id = ? AND user_id = ?`, pollID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM participants WHERE poll_id = ? AND user_id = ?`, pollID, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changedUsers(ids...)
	return nil
}

// Vote records userID's answer to one of a poll's times. If that brings the
// poll to its quorum, it is decided there and then, and the decision is
// returned.
func (s *Store) Vote(pollID, slotID, userID int64, answer int, now time.Time) (*Event, error) {
	if answer != Yes && answer != IfNeeded && answer != No {
		return nil, ErrInvalid
	}
	// A poll whose deadline has passed is decided first, whether or not the
	// clock has got round to it, so nobody answers a poll that is over.
	if ev, err := s.DecideIfDue(pollID, now); err != nil {
		return nil, err
	} else if ev != nil {
		return ev, ErrClosed
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := scanPoll(tx.QueryRow(pollSelect+`WHERE id = ? AND deleted_at IS NULL`, pollID))
	if err != nil {
		return nil, err
	}
	if !p.IsOpen() {
		return nil, ErrClosed
	}
	if err := affected(tx.Exec(`INSERT INTO votes (slot_id, user_id, answer)
		SELECT id, ?, ? FROM slots WHERE id = ? AND poll_id = ?
		ON CONFLICT (slot_id, user_id) DO UPDATE SET answer = excluded.answer`, userID, answer, slotID, pollID)); err != nil {
		return nil, err
	}
	ev, err := s.decide(tx, p, now, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.changed(pollID)
	return ev, nil
}

// Rename changes a poll's title.
func (s *Store) Rename(pollID int64, title string) error {
	title, err := cleanTitle(title)
	if err != nil {
		return err
	}
	if err := affected(s.db.Exec(`UPDATE polls SET title = ? WHERE id = ? AND deleted_at IS NULL`, title, pollID)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// Reschedule changes when an open poll decides and how many it needs. A lower
// quorum can be reached already, in which case it is decided at once.
func (s *Store) Reschedule(pollID int64, deadline string, quorum int, now time.Time) (*Event, error) {
	quorum, err := cleanQuorum(quorum)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := scanPoll(tx.QueryRow(pollSelect+`WHERE id = ? AND deleted_at IS NULL`, pollID))
	if err != nil {
		return nil, err
	}
	if !p.IsOpen() {
		return nil, ErrClosed
	}
	var first string
	if err := tx.QueryRow(`SELECT MIN(starts) FROM slots WHERE poll_id = ?`, pollID).Scan(&first); err != nil {
		return nil, err
	}
	if strings.TrimSpace(deadline) == "" {
		return nil, ErrInvalid
	}
	if deadline, err = cleanDeadline(deadline, first, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE polls SET deadline = ?, quorum = ? WHERE id = ?`, deadline, quorum, pollID); err != nil {
		return nil, err
	}
	p.Deadline, p.Quorum = deadline, quorum
	ev, err := s.decide(tx, p, now, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.changed(pollID)
	return ev, nil
}

// ResetInvite gives a poll a new invite link. The old one stops working.
func (s *Store) ResetInvite(pollID int64) error {
	return s.pollUpdate(pollID, `UPDATE polls SET invite_code = ?, invite_open = 1 WHERE id = ? AND deleted_at IS NULL`, newCode())
}

// ResetOrganizerLink gives a poll a new organizer link. The old one stops
// working; the organizers it made stay organizers.
func (s *Store) ResetOrganizerLink(pollID int64) error {
	return s.pollUpdate(pollID, `UPDATE polls SET organizer_code = ? WHERE id = ? AND deleted_at IS NULL`, newCode())
}

// SetInviteOpen closes a poll's invite link to newcomers, or opens it again.
// People already on the poll are let in whatever it is.
func (s *Store) SetInviteOpen(pollID int64, open bool) error {
	return s.pollUpdate(pollID, `UPDATE polls SET invite_open = ? WHERE id = ? AND deleted_at IS NULL`, open)
}

func (s *Store) pollUpdate(pollID int64, query string, value any) error {
	if err := affected(s.db.Exec(query, value, pollID)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// MarkSeen records that userID has looked at a poll since its latest
// decision. It reports whether that changed anything.
func (s *Store) MarkSeen(pollID, userID int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE participants SET seen_seq = (SELECT decision_seq FROM polls WHERE id = ?)
		WHERE poll_id = ? AND user_id = ? AND seen_seq < (SELECT decision_seq FROM polls WHERE id = ?)`,
		pollID, pollID, userID, pollID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.changedUsers(userID) // the badge goes on their other devices too
	}
	return n > 0, nil
}

// DeletePoll moves a poll to the trash, from which RestorePoll can bring it
// back for a day.
func (s *Store) DeletePoll(pollID int64) error {
	var ids []int64
	rows, err := s.db.Query(`SELECT user_id FROM participants WHERE poll_id = ?`, pollID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if err := affected(s.db.Exec(`UPDATE polls SET deleted_at = CURRENT_TIMESTAMP WHERE id = ? AND deleted_at IS NULL`, pollID)); err != nil {
		return err
	}
	s.changedUsers(ids...)
	return nil
}

// RestorePoll takes a poll back out of the trash, for an organizer of it.
func (s *Store) RestorePoll(pollID, userID int64) error {
	if err := affected(s.db.Exec(`UPDATE polls SET deleted_at = NULL WHERE id = ? AND deleted_at >= datetime('now', ?)
		AND EXISTS (SELECT 1 FROM participants WHERE poll_id = ? AND user_id = ? AND organizer = 1)`,
		pollID, trashRetention, pollID, userID)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// purgeTrash forgets polls deleted more than a day ago.
func (s *Store) purgeTrash() error {
	_, err := s.db.Exec(`DELETE FROM polls WHERE deleted_at < datetime('now', ?)`, trashRetention)
	return err
}

// ---- lists of polls ---------------------------------------------------

// Summary is a poll as one person's lists show it.
type Summary struct {
	Poll
	People   int `json:"people"`
	Answered int `json:"answered"`
	// Mine is whether this person has answered it.
	Mine      bool `json:"answered_by_you"`
	Organizer bool `json:"organizer"`
	// Unseen is whether it was decided since they last looked.
	Unseen bool `json:"unseen"`
	// Chosen is the time it was decided for; First and Last span its times.
	Chosen string `json:"chosen,omitempty"`
	// Venue is the name of the place it was decided for, if it has one.
	Venue string `json:"venue,omitempty"`
	First string `json:"first"`
	Last  string `json:"last"`
}

// Past is whether the poll is over: decided for a time that has gone, or
// every time it offered has gone.
func (s Summary) Past(now string) bool {
	if s.IsConfirmed() {
		return s.Chosen < now
	}
	return s.Last < now
}

// Polls is every poll userID is on: the open ones by deadline, then the
// decided ones by when they happen.
func (s *Store) Polls(userID int64) ([]Summary, error) {
	rows, err := s.db.Query(`SELECT p.id, p.title, p.category, p.deadline, p.quorum, p.status, COALESCE(p.chosen_slot, 0), p.decision_seq,
			p.invite_code, p.invite_open, p.organizer_code, p.chat_code, p.origin, COALESCE(p.created_by, 0), p.places, COALESCE(p.chosen_venue, 0),
			pa.organizer, pa.seen_seq < p.decision_seq,
			(SELECT COUNT(*) FROM participants WHERE poll_id = p.id),
			(SELECT COUNT(DISTINCT v.user_id) FROM votes v JOIN slots s ON s.id = v.slot_id JOIN participants x ON x.poll_id = p.id AND x.user_id = v.user_id WHERE s.poll_id = p.id),
			EXISTS (SELECT 1 FROM votes v JOIN slots s ON s.id = v.slot_id WHERE s.poll_id = p.id AND v.user_id = pa.user_id),
			COALESCE((SELECT starts FROM slots WHERE id = p.chosen_slot), ''), COALESCE((SELECT name FROM venues WHERE id = p.chosen_venue), ''),
			(SELECT MIN(starts) FROM slots WHERE poll_id = p.id), (SELECT MAX(starts) FROM slots WHERE poll_id = p.id)
		FROM polls p JOIN participants pa ON pa.poll_id = p.id AND pa.user_id = ?
		WHERE p.deleted_at IS NULL
		ORDER BY p.status <> 'open', CASE WHEN p.status = 'open' THEN p.deadline ELSE COALESCE((SELECT starts FROM slots WHERE id = p.chosen_slot), p.deadline) END, p.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var x Summary
		p := &x.Poll
		if err := rows.Scan(&p.ID, &p.Title, &p.Category, &p.Deadline, &p.Quorum, &p.Status, &p.ChosenSlot, &p.DecisionSeq,
			&p.InviteCode, &p.InviteOpen, &p.OrganizerCode, &p.ChatCode, &p.Origin, &p.CreatedBy, &p.Places, &p.ChosenVenue,
			&x.Organizer, &x.Unseen, &x.People, &x.Answered, &x.Mine, &x.Chosen, &x.Venue, &x.First, &x.Last); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
