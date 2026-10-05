package store

import (
	"database/sql"
	"errors"
	"sort"
)

// A group is people who meet again and again: a dinner club, a team. Its
// polls take in everyone in it, chats connected to it hear about every one
// of them, and it remembers who has travelled farthest, so the places it
// meets at even out over time.

type Group struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	InviteCode string `json:"-"`
	ChatCode   string `json:"-"`
	Members    int    `json:"members"`
	Organizer  bool   `json:"organizer"` // whether the viewer runs it
}

// Member is somebody in a group.
type Member struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Organizer bool   `json:"organizer"`
}

// Share is how much one member has travelled at a group's meetups compared
// with everyone else: positive is more than their share.
type Share struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Extra    float64 `json:"extra_minutes"` // summed over the meetups counted
	Meetups  int     `json:"meetups"`
	Minutes  int     `json:"minutes"` // travelled in all
	Organize bool    `json:"-"`
}

// rotationMeetups is how many of a group's latest meetups its memory counts.
// Old trips fade out: who travelled far last spring matters less now.
const rotationMeetups = 10

const groupsSchema = `
CREATE TABLE IF NOT EXISTS groups (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	invite_code TEXT NOT NULL UNIQUE,
	chat_code   TEXT NOT NULL UNIQUE,
	created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS group_members (
	group_id  INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	organizer INTEGER NOT NULL DEFAULT 0,
	joined_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS group_members_user ON group_members(user_id);
CREATE TABLE IF NOT EXISTS group_chats (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	group_id   INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	platform   TEXT NOT NULL,
	target     TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE (group_id, platform, target)
);
-- How long each person took to get to a decided place: minutes only, never
-- where from. It is what a group's rotation memory is made of.
CREATE TABLE IF NOT EXISTS trips (
	poll_id INTEGER NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	minutes INTEGER NOT NULL,
	PRIMARY KEY (poll_id, user_id)
);
`

// CreateGroup makes a group with userID running it.
func (s *Store) CreateGroup(userID int64, name string) (Group, error) {
	name, err := cleanTitle(name)
	if err != nil {
		return Group{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO groups (name, invite_code, chat_code, created_by) VALUES (?, ?, ?, ?)`,
		name, newCode(), newCode(), userID)
	if err != nil {
		return Group{}, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO group_members (group_id, user_id, organizer) VALUES (?, ?, 1)`, id, userID); err != nil {
		return Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return Group{}, err
	}
	s.changedUsers(userID)
	return s.Group(id, userID)
}

const groupSelect = `SELECT g.id, g.name, g.invite_code, g.chat_code,
	(SELECT COUNT(*) FROM group_members WHERE group_id = g.id),
	COALESCE((SELECT organizer FROM group_members WHERE group_id = g.id AND user_id = ?), 0)
	FROM groups g `

func scanGroup(row interface{ Scan(...any) error }) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.InviteCode, &g.ChatCode, &g.Members, &g.Organizer)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

// Group is one group, as viewer sees it. Somebody not in it gets
// ErrNotFound, as if it did not exist.
func (s *Store) Group(id, viewer int64) (Group, error) {
	return scanGroup(s.db.QueryRow(groupSelect+`WHERE g.id = ? AND EXISTS
		(SELECT 1 FROM group_members WHERE group_id = g.id AND user_id = ?)`, viewer, id, viewer))
}

// GroupByInvite finds the group an invite link is for.
func (s *Store) GroupByInvite(code string, viewer int64) (Group, error) {
	return scanGroup(s.db.QueryRow(groupSelect+`WHERE g.invite_code = ?`, viewer, code))
}

// Groups is every group userID is in, by name.
func (s *Store) Groups(userID int64) ([]Group, error) {
	rows, err := s.db.Query(groupSelect+`JOIN group_members m ON m.group_id = g.id AND m.user_id = ?
		ORDER BY g.name COLLATE NOCASE, g.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Members is everyone in a group, organizers first, then by when they joined.
func (s *Store) Members(groupID int64) ([]Member, error) {
	rows, err := s.db.Query(`SELECT u.id, u.name, m.organizer FROM group_members m JOIN users u ON u.id = m.user_id
		WHERE m.group_id = ? ORDER BY m.organizer DESC, m.joined_at, u.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.Organizer); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// groupChanged tells everyone in a group that it changed.
func (s *Store) groupChanged(groupID int64) {
	rows, err := s.db.Query(`SELECT user_id FROM group_members WHERE group_id = ?`, groupID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	s.changedUsers(ids...)
}

// JoinGroup puts userID in a group, and on every one of its polls still
// open, so nobody has to be invited to each.
func (s *Store) JoinGroup(groupID, userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO group_members (group_id, user_id) VALUES (?, ?)`, groupID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO participants (poll_id, user_id, seen_seq)
		SELECT id, ?, decision_seq FROM polls WHERE group_id = ? AND status = 'open' AND deleted_at IS NULL`, userID, groupID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.groupChanged(groupID)
	return nil
}

// LeaveGroup takes userID out of a group. Their polls stay theirs. The last
// organizer cannot leave.
func (s *Store) LeaveGroup(groupID, userID int64) error {
	var organizer bool
	if err := s.db.QueryRow(`SELECT organizer FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, userID).Scan(&organizer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if organizer {
		var others int
		s.db.QueryRow(`SELECT COUNT(*) FROM group_members WHERE group_id = ? AND organizer = 1 AND user_id <> ?`, groupID, userID).Scan(&others)
		if others == 0 {
			return ErrInvalid
		}
	}
	s.groupChanged(groupID)
	_, err := s.db.Exec(`DELETE FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, userID)
	s.changedUsers(userID)
	return err
}

// RenameGroup changes a group's name.
func (s *Store) RenameGroup(groupID int64, name string) error {
	name, err := cleanTitle(name)
	if err != nil {
		return err
	}
	if err := affected(s.db.Exec(`UPDATE groups SET name = ? WHERE id = ?`, name, groupID)); err != nil {
		return err
	}
	s.groupChanged(groupID)
	return nil
}

// ResetGroupInvite gives a group a new invite link; the old one stops working.
func (s *Store) ResetGroupInvite(groupID int64) error {
	if err := affected(s.db.Exec(`UPDATE groups SET invite_code = ? WHERE id = ?`, newCode(), groupID)); err != nil {
		return err
	}
	s.groupChanged(groupID)
	return nil
}

// DeleteGroup deletes a group. Its polls stay, belonging to nobody's group.
func (s *Store) DeleteGroup(groupID int64) error {
	s.groupChanged(groupID)
	_, err := s.db.Exec(`UPDATE polls SET group_id = NULL WHERE group_id = ?`, groupID)
	if err != nil {
		return err
	}
	return affected(s.db.Exec(`DELETE FROM groups WHERE id = ?`, groupID))
}

// GroupChats is every chat connected to a group.
func (s *Store) GroupChats(groupID int64) ([]Chat, error) {
	rows, err := s.db.Query(`SELECT id, platform, target, title FROM group_chats WHERE group_id = ? ORDER BY id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Platform, &c.Target, &c.Title); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddGroupChat connects a chat to a group: every poll the group makes from
// now on tells it, and so do the group's polls that are still open.
func (s *Store) AddGroupChat(groupID int64, platform, target, title string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO group_chats (group_id, platform, target, title) VALUES (?, ?, ?, ?)
		ON CONFLICT (group_id, platform, target) DO UPDATE SET title = excluded.title`,
		groupID, platform, target, cleanLabel(title, maxTitleLen)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO chats (poll_id, platform, target, title)
		SELECT id, ?, ?, ? FROM polls WHERE group_id = ? AND status = 'open' AND deleted_at IS NULL
		ON CONFLICT (poll_id, platform, target) DO UPDATE SET broken = 0`, platform, target, cleanLabel(title, maxTitleLen), groupID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.groupChanged(groupID)
	return nil
}

// RemoveGroupChat disconnects a chat from a group's future polls. Polls that
// already have it keep it, until disconnected there.
func (s *Store) RemoveGroupChat(groupID, chatID int64) error {
	if err := affected(s.db.Exec(`DELETE FROM group_chats WHERE id = ? AND group_id = ?`, chatID, groupID)); err != nil {
		return err
	}
	s.groupChanged(groupID)
	return nil
}

// GroupPolls is every poll of a group that viewer is on, as their lists show
// them.
func (s *Store) GroupPolls(groupID, viewer int64) ([]Summary, error) {
	all, err := s.Polls(viewer)
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, p := range all {
		if p.GroupID == groupID {
			out = append(out, p)
		}
	}
	return out, nil
}

// Shares is how a group's travelling has been shared out over its latest
// meetups: for each member, how many minutes more (or less) than the
// meetup's average they travelled, added up. The most owed come first.
func (s *Store) Shares(groupID int64) ([]Share, error) {
	extra, meetups, minutes, err := sharesOf(s.db, groupID, 0)
	if err != nil {
		return nil, err
	}
	members, err := s.Members(groupID)
	if err != nil {
		return nil, err
	}
	out := []Share{}
	for _, m := range members {
		out = append(out, Share{ID: m.ID, Name: m.Name, Extra: extra[m.ID], Meetups: meetups[m.ID], Minutes: minutes[m.ID], Organize: m.Organizer})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Extra > out[j].Extra })
	return out, nil
}

// sharesOf adds up a group's trips over its latest decided meetups, leaving
// out the poll being decided now.
func sharesOf(q querier, groupID, except int64) (extra map[int64]float64, meetups, minutes map[int64]int, err error) {
	extra, meetups, minutes = map[int64]float64{}, map[int64]int{}, map[int64]int{}
	rows, err := q.Query(`SELECT t.poll_id, t.user_id, t.minutes FROM trips t
		WHERE t.poll_id IN (SELECT id FROM polls WHERE group_id = ? AND id <> ? AND status = 'confirmed' AND deleted_at IS NULL
			ORDER BY id DESC LIMIT ?)`, groupID, except, rotationMeetups)
	if err != nil {
		return nil, nil, nil, err
	}
	type trip struct{ user, minutes int64 }
	byPoll := map[int64][]trip{}
	for rows.Next() {
		var poll int64
		var t trip
		if err := rows.Scan(&poll, &t.user, &t.minutes); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		byPoll[poll] = append(byPoll[poll], t)
	}
	rows.Close()
	for _, trips := range byPoll {
		if len(trips) < 2 {
			continue // alone, nobody travelled more than anybody
		}
		total := 0.0
		for _, t := range trips {
			total += float64(t.minutes)
		}
		avg := total / float64(len(trips))
		for _, t := range trips {
			extra[t.user] += float64(t.minutes) - avg
			meetups[t.user]++
			minutes[t.user] += int(t.minutes)
		}
	}
	return extra, meetups, minutes, nil
}

// recordTrips keeps how long everyone took to get to a poll's place, for its
// group's memory. Only minutes are kept, never where anyone came from.
func recordTrips(tx *sql.Tx, p Poll, venueID int64) error {
	if _, err := tx.Exec(`DELETE FROM trips WHERE poll_id = ?`, p.ID); err != nil {
		return err
	}
	if venueID == 0 {
		return nil
	}
	venues, err := venuesOf(tx, p.ID)
	if err != nil {
		return err
	}
	starts, err := startsOf(tx, p.ID)
	if err != nil {
		return err
	}
	for _, v := range venues {
		if v.ID != venueID {
			continue
		}
		for _, st := range starts {
			m := placesMinutes(st, v)
			if _, err := tx.Exec(`INSERT INTO trips (poll_id, user_id, minutes) VALUES (?, ?, ?)`, p.ID, st.UserID, m); err != nil {
				return err
			}
		}
	}
	return nil
}
