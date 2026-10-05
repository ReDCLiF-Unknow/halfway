package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid input")
	// ErrClosed is an answer to a poll that has already been decided.
	ErrClosed = errors.New("the poll has been decided")
)

const (
	maxNameLen  = 40
	maxTitleLen = 80
	// trashRetention is how long a deleted poll stays restorable.
	trashRetention = "-1 day"
)

type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// TokenSaved is whether this person has said they have their sign-in key
	// somewhere safe. The key is the whole account, so until they say so,
	// every page reminds them.
	TokenSaved bool `json:"-"`
	// Device is which of their devices this is: 0 for the first, the one
	// with the key made with the name, and otherwise one signed in by link.
	Device int64 `json:"-"`
}

type Store struct {
	db     *sql.DB
	notify func(userIDs []int64)
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	token_hash  TEXT NOT NULL UNIQUE,
	token_saved INTEGER NOT NULL DEFAULT 0,
	created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS polls (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	title          TEXT NOT NULL,
	category       TEXT NOT NULL,
	deadline       TEXT NOT NULL,              -- local time, YYYY-MM-DDTHH:MM
	quorum         INTEGER NOT NULL DEFAULT 0, -- 0: no minimum
	status         TEXT NOT NULL DEFAULT 'open',
	chosen_slot    INTEGER,
	places         INTEGER NOT NULL DEFAULT 0, -- whether it also finds a place to meet
	chosen_venue   INTEGER,
	decision_seq   INTEGER NOT NULL DEFAULT 0, -- goes up with every decision, for "new since you looked"
	invite_code    TEXT NOT NULL UNIQUE,
	invite_open    INTEGER NOT NULL DEFAULT 1,
	organizer_code TEXT NOT NULL UNIQUE,
	chat_code      TEXT NOT NULL UNIQUE,
	origin         TEXT NOT NULL DEFAULT '',   -- where the poll was made, for links in chat messages
	created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	deleted_at     TIMESTAMP
);
CREATE INDEX IF NOT EXISTS polls_due ON polls(status, deadline);
-- Where people set off from, for polls that find a place. Rounded to about
-- 500 m before it gets here, shown to nobody but its owner, and deleted a
-- week after the event.
CREATE TABLE IF NOT EXISTS starts (
	poll_id    INTEGER NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	lat        REAL NOT NULL,
	lon        REAL NOT NULL,
	label      TEXT NOT NULL DEFAULT '',
	mode       TEXT NOT NULL DEFAULT 'transit',
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (poll_id, user_id)
);
CREATE TABLE IF NOT EXISTS venues (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	poll_id    INTEGER NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	ref        TEXT,                           -- node/123 from OpenStreetMap; NULL for one somebody added
	name       TEXT NOT NULL,
	address    TEXT NOT NULL DEFAULT '',
	lat        REAL NOT NULL,
	lon        REAL NOT NULL,
	added_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE (poll_id, ref)
);
CREATE TABLE IF NOT EXISTS venue_votes (
	venue_id INTEGER NOT NULL REFERENCES venues(id) ON DELETE CASCADE,
	user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	PRIMARY KEY (venue_id, user_id)
);
CREATE TABLE IF NOT EXISTS slots (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	poll_id INTEGER NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	starts  TEXT NOT NULL                      -- local time, YYYY-MM-DDTHH:MM
);
CREATE INDEX IF NOT EXISTS slots_poll ON slots(poll_id, starts);
CREATE TABLE IF NOT EXISTS participants (
	poll_id   INTEGER NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	organizer INTEGER NOT NULL DEFAULT 0,
	seen_seq  INTEGER NOT NULL DEFAULT 0,
	joined_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (poll_id, user_id)
);
CREATE INDEX IF NOT EXISTS participants_user ON participants(user_id);
CREATE TABLE IF NOT EXISTS votes (
	slot_id INTEGER NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	answer  INTEGER NOT NULL,                  -- 0 no, 1 if needed, 2 yes
	PRIMARY KEY (slot_id, user_id)
);
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	poll_id    INTEGER NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL,                  -- confirmed, cancelled, changed
	text       TEXT NOT NULL,                  -- the message chats are sent
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS chats (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	poll_id    INTEGER NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	platform   TEXT NOT NULL,
	target     TEXT NOT NULL,                  -- a Telegram chat id
	title      TEXT NOT NULL DEFAULT '',
	broken     INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE (poll_id, platform, target)
);
CREATE TABLE IF NOT EXISTS deliveries (
	event_id     INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
	chat_id      INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
	delivered_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (event_id, chat_id)
);
`

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	// Write-ahead logging lets readers carry on while somebody writes, which
	// matters because every change makes each open page fetch itself again.
	// Transactions take the write lock when they begin: a decision reads the
	// answers and then writes the outcome, and no answer may land in between.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// An in-memory database is private to its connection, so a second one
	// would open an empty database: those stay on one.
	conns := 8
	if strings.Contains(path, ":memory:") || strings.Contains(path, "mode=memory") {
		conns = 1
	}
	db.SetMaxOpenConns(conns)
	s := &Store{db: db}
	if _, err := db.Exec(schema + devicesSchema); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.purgeTrash(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate upgrades databases made by earlier versions: polls from v1.0 have
// no columns for places, which CREATE TABLE IF NOT EXISTS does not add.
func (s *Store) migrate() error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('polls')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	for _, col := range []struct{ name, def string }{
		{"places", "INTEGER NOT NULL DEFAULT 0"},
		{"chosen_venue", "INTEGER"},
	} {
		if !have[col.name] {
			if _, err := s.db.Exec(`ALTER TABLE polls ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// SetNotifier registers a callback fired after every change, with everyone
// whose pages show what changed. Their open pages refresh themselves.
func (s *Store) SetNotifier(fn func(userIDs []int64)) { s.notify = fn }

func (s *Store) changedUsers(ids ...int64) {
	if s.notify != nil && len(ids) > 0 {
		s.notify(ids)
	}
}

// changed tells everyone on a poll that it changed.
func (s *Store) changed(pollID int64) {
	if s.notify == nil {
		return
	}
	rows, err := s.db.Query(`SELECT user_id FROM participants WHERE poll_id = ?`, pollID)
	if err != nil {
		return
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	s.changedUsers(ids...)
}

// ---- users ------------------------------------------------------------

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// newCode makes a link code: 16 characters, 96 random bits, which nobody
// guesses.
func newCode() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxNameLen {
		return "", ErrInvalid
	}
	return name, nil
}

// CreateUser adds a person and returns their secret token. Only a hash of it
// is stored, so it cannot be recovered later from the database.
func (s *Store) CreateUser(name string) (User, string, error) {
	name, err := cleanName(name)
	if err != nil {
		return User{}, "", err
	}
	token := newToken()
	res, err := s.db.Exec(`INSERT INTO users (name, token_hash) VALUES (?, ?)`, name, hashToken(token))
	if err != nil {
		return User{}, "", err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Name: name}, token, nil
}

func (s *Store) UserByToken(token string) (User, error) {
	var u User
	if token == "" {
		return u, ErrNotFound
	}
	h := hashToken(token)
	err := s.db.QueryRow(`SELECT id, name, token_saved, 0 FROM users WHERE token_hash = ?
		UNION ALL SELECT u.id, u.name, u.token_saved, d.id FROM devices d JOIN users u ON u.id = d.user_id WHERE d.token_hash = ?`,
		h, h).Scan(&u.ID, &u.Name, &u.TokenSaved, &u.Device)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) RenameUser(id int64, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	if err := affected(s.db.Exec(`UPDATE users SET name = ? WHERE id = ?`, name, id)); err != nil {
		return err
	}
	// The name is next to their answers on every poll they are on.
	rows, err := s.db.Query(`SELECT DISTINCT b.user_id FROM participants a JOIN participants b ON b.poll_id = a.poll_id
		WHERE a.user_id = ?`, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ids := []int64{id}
	for rows.Next() {
		var other int64
		if rows.Scan(&other) == nil && other != id {
			ids = append(ids, other)
		}
	}
	s.changedUsers(ids...)
	return nil
}

// UserCount is how many people there are.
func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// MarkTokenSaved records that someone has their sign-in key somewhere safe,
// which stops the app reminding them about it.
func (s *Store) MarkTokenSaved(id int64) error {
	_, err := s.db.Exec(`UPDATE users SET token_saved = 1 WHERE id = ?`, id)
	if err == nil {
		s.changedUsers(id) // the banner goes on their other devices too
	}
	return err
}

// affected turns "no rows changed" into ErrNotFound.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
