package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"halfway/internal/places"
)

// Start is where somebody on a poll sets off from, and how. Only its owner
// is ever shown where; everyone else sees travel times.
type Start struct {
	UserID int64
	Name   string
	At     places.Point
	Label  string // what they searched for, or "Your location"
	Mode   string
}

// Venue is a place a poll could meet at, with who would go there.
type Venue struct {
	ID      int64
	PollID  int64
	Ref     string // node/123 from OpenStreetMap; empty for one somebody added
	Name    string
	Address string
	At      places.Point
	Voters  []Participant
}

// Custom is whether somebody added the place, rather than Halfway finding it.
func (v Venue) Custom() bool { return v.Ref == "" }

// startsKept is how long starting points outlive the event they were for.
const startsKept = 7 * 24 * time.Hour

const maxLabelLen = 80

func cleanLabel(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

// SetStart records where userID sets off from for a poll. The point is
// rounded to about 500 m here, so nothing more precise is ever stored.
func (s *Store) SetStart(pollID, userID int64, at places.Point, label, mode string) error {
	if !at.Valid() {
		return ErrInvalid
	}
	at = places.Snap(at)
	if err := affected(s.db.Exec(`INSERT INTO starts (poll_id, user_id, lat, lon, label, mode)
		SELECT ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM participants WHERE poll_id = ? AND user_id = ?)
		ON CONFLICT (poll_id, user_id) DO UPDATE SET lat = excluded.lat, lon = excluded.lon, label = excluded.label,
			mode = excluded.mode, updated_at = CURRENT_TIMESTAMP`,
		pollID, userID, at.Lat, at.Lon, cleanLabel(label, maxLabelLen), places.CleanMode(mode), pollID, userID)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// SetMode changes only how somebody travels, keeping where from.
func (s *Store) SetMode(pollID, userID int64, mode string) error {
	if err := affected(s.db.Exec(`UPDATE starts SET mode = ? WHERE poll_id = ? AND user_id = ?`,
		places.CleanMode(mode), pollID, userID)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// ClearStart forgets where userID sets off from.
func (s *Store) ClearStart(pollID, userID int64) error {
	if _, err := s.db.Exec(`DELETE FROM starts WHERE poll_id = ? AND user_id = ?`, pollID, userID); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// Starts is where everyone still on a poll sets off from.
func (s *Store) Starts(pollID int64) ([]Start, error) {
	return startsOf(s.db, pollID)
}

func startsOf(q querier, pollID int64) ([]Start, error) {
	rows, err := q.Query(`SELECT st.user_id, u.name, st.lat, st.lon, st.label, st.mode
		FROM starts st JOIN users u ON u.id = st.user_id
		JOIN participants pa ON pa.poll_id = st.poll_id AND pa.user_id = st.user_id
		WHERE st.poll_id = ? ORDER BY pa.joined_at, u.id`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Start{}
	for rows.Next() {
		var st Start
		if err := rows.Scan(&st.UserID, &st.Name, &st.At.Lat, &st.At.Lon, &st.Label, &st.Mode); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// PlacesStarts turns starts into what the places package measures with.
func PlacesStarts(starts []Start) []places.Start {
	out := make([]places.Start, 0, len(starts))
	for _, st := range starts {
		out = append(out, places.Start{ID: st.UserID, Name: st.Name, At: st.At, Mode: st.Mode})
	}
	return out
}

// Venues is every place a poll could meet at, in the order they were added,
// with who would go to each.
func (s *Store) Venues(pollID int64) ([]Venue, error) {
	return venuesOf(s.db, pollID)
}

func venuesOf(q querier, pollID int64) ([]Venue, error) {
	rows, err := q.Query(`SELECT v.id, v.poll_id, COALESCE(v.ref, ''), v.name, v.address, v.lat, v.lon, u.id, u.name
		FROM venues v LEFT JOIN venue_votes vv ON vv.venue_id = v.id
		LEFT JOIN participants pa ON pa.poll_id = v.poll_id AND pa.user_id = vv.user_id
		LEFT JOIN users u ON u.id = pa.user_id
		WHERE v.poll_id = ? ORDER BY v.id, u.name COLLATE NOCASE`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Venue{}
	for rows.Next() {
		var (
			v    Venue
			uid  sql.NullInt64
			name sql.NullString
		)
		if err := rows.Scan(&v.ID, &v.PollID, &v.Ref, &v.Name, &v.Address, &v.At.Lat, &v.At.Lon, &uid, &name); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != v.ID {
			v.Voters = []Participant{}
			out = append(out, v)
		}
		if uid.Valid {
			last := &out[len(out)-1]
			last.Voters = append(last.Voters, Participant{ID: uid.Int64, Name: name.String})
		}
	}
	return out, rows.Err()
}

// MaxVenues is how many places one poll considers at most.
const MaxVenues = 8

// Suggest replaces the places Halfway suggested before, other than ones
// somebody has voted for or that have been chosen, with these. Places people
// added themselves are never touched.
func (s *Store) Suggest(pollID int64, suggested []places.Venue) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM venues WHERE poll_id = ? AND ref IS NOT NULL
		AND id NOT IN (SELECT venue_id FROM venue_votes)
		AND id <> (SELECT COALESCE(chosen_venue, 0) FROM polls WHERE id = ?)`, pollID, pollID); err != nil {
		return err
	}
	for _, v := range suggested {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM venues WHERE poll_id = ?`, pollID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxVenues {
			break
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO venues (poll_id, ref, name, address, lat, lon) VALUES (?, ?, ?, ?, ?, ?)`,
			pollID, v.Ref, cleanLabel(v.Name, maxTitleLen), cleanLabel(v.Address, maxLabelLen), v.At.Lat, v.At.Lon); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// AddVenue adds a place somebody chose themselves.
func (s *Store) AddVenue(pollID, userID int64, name, address string, at places.Point) (Venue, error) {
	name = cleanLabel(name, maxTitleLen)
	if name == "" || !at.Valid() {
		return Venue{}, ErrInvalid
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM venues WHERE poll_id = ?`, pollID).Scan(&n); err != nil {
		return Venue{}, err
	}
	if n >= MaxVenues {
		return Venue{}, ErrInvalid
	}
	res, err := s.db.Exec(`INSERT INTO venues (poll_id, name, address, lat, lon, added_by) VALUES (?, ?, ?, ?, ?, ?)`,
		pollID, name, cleanLabel(address, maxLabelLen), at.Lat, at.Lon, userID)
	if err != nil {
		return Venue{}, err
	}
	id, _ := res.LastInsertId()
	s.changed(pollID)
	return Venue{ID: id, PollID: pollID, Name: name, At: at}, nil
}

// RemoveVenue takes a place off a poll, unless it is the one chosen.
func (s *Store) RemoveVenue(pollID, venueID int64) error {
	var chosen int64
	if err := s.db.QueryRow(`SELECT COALESCE(chosen_venue, 0) FROM polls WHERE id = ?`, pollID).Scan(&chosen); err != nil {
		return err
	}
	if chosen == venueID {
		return ErrInvalid
	}
	if err := affected(s.db.Exec(`DELETE FROM venues WHERE id = ? AND poll_id = ?`, venueID, pollID)); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// ToggleVenueVote says userID would go to a place, or takes that back. Once
// the poll is decided, nobody votes any more.
func (s *Store) ToggleVenueVote(pollID, venueID, userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT p.status FROM venues v JOIN polls p ON p.id = v.poll_id
		WHERE v.id = ? AND v.poll_id = ? AND p.deleted_at IS NULL`, venueID, pollID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if status != StatusOpen {
		return ErrClosed
	}
	res, err := tx.Exec(`DELETE FROM venue_votes WHERE venue_id = ? AND user_id = ?`, venueID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.Exec(`INSERT INTO venue_votes (venue_id, user_id) VALUES (?, ?)`, venueID, userID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changed(pollID)
	return nil
}

// BestVenue is the place a poll would settle on now: the most votes, then
// the fairest trips, then the one added first.
func BestVenue(venues []Venue, starts []Start) (Venue, bool) {
	ps := PlacesStarts(starts)
	var top Venue
	var topF places.Fairness
	found := false
	for _, v := range venues {
		f := places.Measure(v.At, ps)
		switch {
		case !found:
		case len(v.Voters) != len(top.Voters):
			if len(v.Voters) < len(top.Voters) {
				continue
			}
		case !places.Fairer(f, topF):
			continue
		}
		top, topF, found = v, f, true
	}
	return top, found
}

// PickVenue is an organizer choosing the place. On a confirmed poll that
// moves it, and connected chats are told; before that, it is the place the
// poll will settle on when it is decided.
func (s *Store) PickVenue(pollID, venueID int64) (*Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := scanPoll(tx.QueryRow(pollSelect+`WHERE id = ? AND deleted_at IS NULL`, pollID))
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM venues WHERE id = ? AND poll_id = ?)`, venueID, pollID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	if p.ChosenVenue == venueID {
		return nil, nil
	}
	var ev *Event
	if p.IsConfirmed() {
		slots, err := slotsOf(tx, pollID)
		if err != nil {
			return nil, err
		}
		if ev, err = s.settle(tx, p, StatusConfirmed, p.ChosenSlot, venueID, slots); err != nil {
			return nil, err
		}
	} else if _, err := tx.Exec(`UPDATE polls SET chosen_venue = ? WHERE id = ?`, venueID, pollID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.changed(pollID)
	return ev, nil
}

// PurgeStarts forgets where people set off from, a week after the event
// they set off for: the decided time, or the last time offered.
func (s *Store) PurgeStarts(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM starts WHERE poll_id IN (
		SELECT p.id FROM polls p WHERE COALESCE(
			(SELECT starts FROM slots WHERE id = p.chosen_slot),
			(SELECT MAX(starts) FROM slots WHERE poll_id = p.id)) < ?)`,
		now.Add(-startsKept).Format(Stamp))
	return err
}
