package backend

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type User struct {
	ID             string
	Username       string
	PasswordHash   string `json:"-"` // scrypt hash used for authentication
	PasswordSecret string `json:"-"` // AES-GCM encrypted recoverable password
	Enabled        bool
	AllowedServers []string
	CreatedAt      int64 // Unix milliseconds
}

type UserStore struct {
	db      *sqliteDB
	secrets *passwordSecretCipher
	mu      sync.RWMutex
	users   map[string]*User // key: User.ID
	byName  map[string]*User // key: lowercase(Username)
	logger  *Logger
}

func NewUserStore(db *sqliteDB, logger *Logger) (*UserStore, error) {
	if db == nil {
		return nil, fmt.Errorf("user_store: SQLite database handle is nil")
	}
	if err := db.exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT UNIQUE NOT NULL COLLATE NOCASE,
			password_hash TEXT NOT NULL,
			password_secret TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS user_servers (
			user_id TEXT NOT NULL,
			server_id TEXT NOT NULL,
			PRIMARY KEY (user_id, server_id),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		);
	`); err != nil {
		return nil, fmt.Errorf("user_store: create tables: %w", err)
	}
	hasEncryptedSecrets, err := userPasswordSecretsExist(db)
	if err != nil {
		return nil, fmt.Errorf("user_store: inspect password secrets: %w", err)
	}
	secrets, err := loadOrCreatePasswordSecretCipher(filepath.Dir(db.path), hasEncryptedSecrets)
	if err != nil {
		return nil, fmt.Errorf("user_store: initialize password secrets: %w", err)
	}
	store := &UserStore{
		db:      db,
		secrets: secrets,
		users:   make(map[string]*User),
		byName:  make(map[string]*User),
		logger:  logger,
	}
	if err := store.loadAll(); err != nil {
		return nil, fmt.Errorf("user_store: load: %w", err)
	}
	if logger != nil {
		logger.Infof("UserStore initialized: %d user(s) loaded", len(store.users))
	}
	return store, nil
}

func (s *UserStore) loadAll() error {
	s.users = make(map[string]*User)
	s.byName = make(map[string]*User)

	stmt, err := s.db.prepare(`SELECT id, username, password_hash, password_secret, enabled, created_at FROM users`)
	if err != nil {
		return err
	}
	defer stmt.finalize()
	for {
		hasRow, err := stmt.step()
		if err != nil {
			return err
		}
		if !hasRow {
			break
		}
		user := &User{
			ID:             stmt.columnText(0),
			Username:       stmt.columnText(1),
			PasswordHash:   stmt.columnText(2),
			PasswordSecret: stmt.columnText(3),
			Enabled:        stmt.columnInt(4) != 0,
			CreatedAt:      stmt.columnInt64(5),
		}
		if _, err := s.passwordPlaintext(user); err != nil {
			return fmt.Errorf("user %s password secret: %w", user.ID, err)
		}
		s.users[user.ID] = user
		s.byName[strings.ToLower(user.Username)] = user
	}

	stmtServers, err := s.db.prepare(`SELECT user_id, server_id FROM user_servers ORDER BY server_id`)
	if err != nil {
		return err
	}
	defer stmtServers.finalize()
	for {
		hasRow, err := stmtServers.step()
		if err != nil {
			return err
		}
		if !hasRow {
			break
		}
		userID := stmtServers.columnText(0)
		serverID := stmtServers.columnText(1)
		if user, ok := s.users[userID]; ok {
			user.AllowedServers = append(user.AllowedServers, serverID)
		}
	}
	return nil
}

func (s *UserStore) Create(username, password string, allowedServers []string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.byName[strings.ToLower(username)]; exists {
		return nil, fmt.Errorf("username already exists")
	}

	hashed, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	id := randomHex(16)
	secret, err := s.secrets.encrypt(id, password)
	if err != nil {
		return nil, fmt.Errorf("encrypt password: %w", err)
	}
	now := time.Now().UnixMilli()

	if err := s.db.withWriteTx(func() error {
		if err := s.db.execParams(
			`INSERT INTO users (id, username, password_hash, password_secret, enabled, created_at) VALUES (?, ?, ?, ?, 1, ?)`,
			id, username, hashed, secret, now,
		); err != nil {
			return err
		}
		return s.replaceAllowedServersParams(id, allowedServers)
	}); err != nil {
		return nil, err
	}

	user := &User{
		ID:             id,
		Username:       username,
		PasswordHash:   hashed,
		PasswordSecret: secret,
		Enabled:        true,
		AllowedServers: append([]string(nil), allowedServers...),
		CreatedAt:      now,
	}
	s.users[id] = user
	s.byName[strings.ToLower(username)] = user
	return user, nil
}

func (s *UserStore) Authenticate(username, password string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.byName[strings.ToLower(username)]
	if !ok || !user.Enabled {
		// Same reason as AuthManager.Authenticate: an unknown or disabled account must not
		// answer faster than a wrong password.
		spendVerifyTime(password)
		return nil
	}
	if !VerifyPassword(password, user.PasswordHash) {
		return nil
	}
	// Return a copy
	return s.copyUser(user)
}

// ContainsUserID reports whether value is a locally registered proxy user ID.
// Membership stays inside the store rather than copying List() to every caller.
func (s *UserStore) ContainsUserID(value string) bool {
	if value == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.users[value]
	return ok
}

func (s *UserStore) Get(id string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	user, ok := s.users[id]
	if !ok {
		return nil
	}
	return s.copyUser(user)
}

func (s *UserStore) GetByUsername(username string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	user, ok := s.byName[strings.ToLower(username)]
	if !ok {
		return nil
	}
	return s.copyUser(user)
}

func (s *UserStore) List() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*User, 0, len(s.users))
	for _, user := range s.users {
		result = append(result, s.copyUser(user))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt < result[j].CreatedAt
	})
	return result
}

func (s *UserStore) Update(id string, username *string, password *string, enabled *bool, allowedServers *[]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[id]
	if !ok {
		return fmt.Errorf("user not found: %s", id)
	}

	if username != nil && strings.ToLower(*username) != strings.ToLower(user.Username) {
		if _, exists := s.byName[strings.ToLower(*username)]; exists {
			return fmt.Errorf("username already exists")
		}
	}

	hashed := ""
	secret := ""
	if password != nil {
		value, err := HashPassword(*password)
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		valueSecret, err := s.secrets.encrypt(id, *password)
		if err != nil {
			return fmt.Errorf("encrypt password: %w", err)
		}
		hashed = value
		secret = valueSecret
	}

	if err := s.db.withWriteTx(func() error {
		if username != nil {
			if err := s.db.execParams(`UPDATE users SET username = ? WHERE id = ?`, *username, id); err != nil {
				return err
			}
		}
		if password != nil {
			if err := s.db.execParams(`UPDATE users SET password_hash = ?, password_secret = ? WHERE id = ?`, hashed, secret, id); err != nil {
				return err
			}
		}
		if enabled != nil {
			if err := s.db.execParams(`UPDATE users SET enabled = ? WHERE id = ?`, boolToInt(*enabled), id); err != nil {
				return err
			}
		}
		if allowedServers != nil {
			return s.replaceAllowedServersParams(id, *allowedServers)
		}
		return nil
	}); err != nil {
		return err
	}

	// Applied only after the transaction committed, so a failed write cannot leave the
	// in-memory user ahead of the database.
	if username != nil {
		delete(s.byName, strings.ToLower(user.Username))
		user.Username = *username
		s.byName[strings.ToLower(user.Username)] = user
	}
	if password != nil {
		user.PasswordHash = hashed
		user.PasswordSecret = secret
	}
	if enabled != nil {
		user.Enabled = *enabled
	}
	if allowedServers != nil {
		user.AllowedServers = append([]string(nil), *allowedServers...)
	}

	return nil
}

func (s *UserStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[id]
	if !ok {
		return fmt.Errorf("user not found: %s", id)
	}

	// foreign_keys is already on for this connection (set when the store was created); a
	// PRAGMA issued inside a transaction would be silently ignored anyway.
	if err := s.db.withWriteTx(func() error {
		if err := s.db.execParams(`DELETE FROM user_servers WHERE user_id = ?`, id); err != nil {
			return err
		}
		return s.db.execParams(`DELETE FROM users WHERE id = ?`, id)
	}); err != nil {
		return err
	}

	delete(s.byName, strings.ToLower(user.Username))
	delete(s.users, id)
	return nil
}

// RemoveServerGrants removes all access grants for the deleted upstream server.
func (s *UserStore) RemoveServerGrants(serverID string) error {
	if serverID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		if err := s.db.execParams(`DELETE FROM user_servers WHERE server_id = ?`, serverID); err != nil {
			return err
		}
	}

	for _, user := range s.users {
		kept := make([]string, 0, len(user.AllowedServers))
		for _, id := range user.AllowedServers {
			if id != serverID {
				kept = append(kept, id)
			}
		}
		user.AllowedServers = kept
	}
	return nil
}

// replaceAllowedServersParams replaces a user's stored server list using the caller's
// transaction or write lock. The caller holds the store lock.
func (s *UserStore) replaceAllowedServersParams(id string, servers []string) error {
	if err := s.db.execParams(`DELETE FROM user_servers WHERE user_id = ?`, id); err != nil {
		return err
	}
	for _, serverID := range servers {
		if err := s.db.execParams(
			`INSERT INTO user_servers (user_id, server_id) VALUES (?, ?)`,
			id, serverID,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *UserStore) passwordPlaintext(user *User) (string, error) {
	if user == nil {
		return "", fmt.Errorf("user_store: nil user")
	}
	if s.secrets == nil {
		return "", fmt.Errorf("user_store: password secret cipher unavailable")
	}
	return s.secrets.decrypt(user.ID, user.PasswordSecret)
}

func (s *UserStore) hasPassword(user *User) (bool, error) {
	password, err := s.passwordPlaintext(user)
	if err != nil {
		return false, err
	}
	return password != "", nil
}

func userPasswordSecretsExist(db *sqliteDB) (bool, error) {
	stmt, err := db.prepare(`SELECT COUNT(*) FROM users WHERE password_secret <> ''`)
	if err != nil {
		return false, err
	}
	defer stmt.finalize()
	hasRow, err := stmt.step()
	if err != nil {
		return false, err
	}
	if !hasRow {
		return false, fmt.Errorf("password secret count query returned no row")
	}
	return stmt.columnInt(0) > 0, nil
}

func (s *UserStore) copyUser(user *User) *User {
	return &User{
		ID:             user.ID,
		Username:       user.Username,
		PasswordHash:   user.PasswordHash,
		PasswordSecret: user.PasswordSecret,
		Enabled:        user.Enabled,
		AllowedServers: append([]string(nil), user.AllowedServers...),
		CreatedAt:      user.CreatedAt,
	}
}
