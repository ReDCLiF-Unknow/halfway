package store

import (
	"database/sql"
	"errors"
	"time"
)

// A person's first device holds the key made when they picked their name.
// Every other device they sign in on gets a key of its own, here, so that one
// can be signed out without the others noticing.

// Device is a device somebody signed in on with a one-time link.
type Device struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"` // "Chrome on Android", from the browser that used the link
	CreatedAt time.Time `json:"created_at"`
}

// DeviceLinkLife is how long a one-time sign-in link works.
const DeviceLinkLife = 10 * time.Minute

const devicesSchema = `
CREATE TABLE IF NOT EXISTS devices (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS device_links (
	code_hash  TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL,             -- Unix seconds
	used       INTEGER NOT NULL DEFAULT 0
);
`

// NewDeviceLink makes a one-time code that signs another device in as
// userID, for DeviceLinkLife. Only its hash is kept.
func (s *Store) NewDeviceLink(userID int64, now time.Time) (string, time.Time, error) {
	if _, err := s.db.Exec(`DELETE FROM device_links WHERE expires_at < ?`, now.Unix()); err != nil {
		return "", time.Time{}, err
	}
	code := newCode()
	expires := now.Add(DeviceLinkLife)
	if _, err := s.db.Exec(`INSERT INTO device_links (code_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(code), userID, expires.Unix()); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// PeekDeviceLink is whose a sign-in link is, if it still works, without
// using it up: a link preview fetching it must not spend it.
func (s *Store) PeekDeviceLink(code string, now time.Time) (User, error) {
	var u User
	err := s.db.QueryRow(`SELECT u.id, u.name, u.token_saved FROM device_links l JOIN users u ON u.id = l.user_id
		WHERE l.code_hash = ? AND l.used = 0 AND l.expires_at >= ?`, hashToken(code), now.Unix()).Scan(&u.ID, &u.Name, &u.TokenSaved)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// UseDeviceLink spends a sign-in link: the device using it gets a key of its
// own for the link's owner, which is returned.
func (s *Store) UseDeviceLink(code, deviceName string, now time.Time) (User, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return User{}, "", err
	}
	defer tx.Rollback()
	var u User
	err = tx.QueryRow(`SELECT u.id, u.name, u.token_saved FROM device_links l JOIN users u ON u.id = l.user_id
		WHERE l.code_hash = ? AND l.used = 0 AND l.expires_at >= ?`, hashToken(code), now.Unix()).Scan(&u.ID, &u.Name, &u.TokenSaved)
	if errors.Is(err, sql.ErrNoRows) {
		return u, "", ErrNotFound
	} else if err != nil {
		return u, "", err
	}
	if _, err := tx.Exec(`UPDATE device_links SET used = 1 WHERE code_hash = ?`, hashToken(code)); err != nil {
		return u, "", err
	}
	token := newToken()
	res, err := tx.Exec(`INSERT INTO devices (user_id, token_hash, name) VALUES (?, ?, ?)`, u.ID, hashToken(token), cleanLabel(deviceName, 60))
	if err != nil {
		return u, "", err
	}
	u.Device, _ = res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return u, "", err
	}
	s.changedUsers(u.ID) // the new device shows up in their profile elsewhere
	return u, token, nil
}

// Devices is every device userID signed in on with a link, newest first.
func (s *Store) Devices(userID int64) ([]Device, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM devices WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RemoveDevice signs one of userID's devices out: its key stops working.
func (s *Store) RemoveDevice(userID, deviceID int64) error {
	if err := affected(s.db.Exec(`DELETE FROM devices WHERE id = ? AND user_id = ?`, deviceID, userID)); err != nil {
		return err
	}
	s.changedUsers(userID)
	return nil
}
