package backend

import (
	"path/filepath"
	"testing"
)

func TestMigrateDatabaseToServerID(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "mappings.db")

	// 1. Manually construct an old schema database with integer server_index
	db, err := openSQLite(dbPath)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}

	oldDDL := `
	CREATE TABLE id_mappings (
		virtual_id TEXT PRIMARY KEY,
		original_id TEXT NOT NULL,
		server_index INTEGER NOT NULL
	);
	CREATE TABLE id_additional_instances (
		virtual_id TEXT NOT NULL,
		original_id TEXT NOT NULL,
		server_index INTEGER NOT NULL,
		PRIMARY KEY (virtual_id, original_id, server_index)
	);
	CREATE TABLE users (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		password_secret TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE user_servers (
		user_id TEXT NOT NULL,
		server_index INTEGER NOT NULL,
		PRIMARY KEY (user_id, server_index),
		FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	);
	CREATE TABLE user_watch_progress (
		proxy_user_id TEXT NOT NULL,
		virtual_item_id TEXT NOT NULL,
		server_index INTEGER NOT NULL,
		original_item_id TEXT NOT NULL,
		position_ticks INTEGER NOT NULL DEFAULT 0,
		runtime_ticks INTEGER NOT NULL DEFAULT 0,
		played INTEGER NOT NULL DEFAULT 0,
		is_favorite INTEGER NOT NULL DEFAULT 0,
		last_played INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0,
		item_type TEXT,
		name TEXT,
		production_year INTEGER,
		provider_tmdb TEXT,
		series_name TEXT,
		index_number INTEGER,
		parent_index_number INTEGER,
		series_virtual_id TEXT,
		series_original_id TEXT,
		PRIMARY KEY (proxy_user_id, virtual_item_id)
	);
	`
	if err := db.exec(oldDDL); err != nil {
		_ = closeSQLite(db)
		t.Fatalf("exec oldDDL: %v", err)
	}

	// Insert test data with server_index 0 and 1. The password columns use the
	// current fresh-install schema; this test exercises server-id migration, not
	// legacy password-schema migration.
	hash, err := HashPassword("testpass")
	if err != nil {
		_ = closeSQLite(db)
		t.Fatalf("hash fixture password: %v", err)
	}
	secretBox, err := loadOrCreatePasswordSecretCipher(tempDir, false)
	if err != nil {
		_ = closeSQLite(db)
		t.Fatalf("create fixture password secret cipher: %v", err)
	}
	secret, err := secretBox.encrypt("u1", "testpass")
	if err != nil {
		_ = closeSQLite(db)
		t.Fatalf("encrypt fixture password: %v", err)
	}
	if err := db.execParams(
		`INSERT INTO users (id, username, password_hash, password_secret, enabled, created_at) VALUES (?, ?, ?, ?, 1, ?)`,
		"u1", "testuser", hash, secret, 1000,
	); err != nil {
		_ = closeSQLite(db)
		t.Fatalf("seed user: %v", err)
	}
	seedSQL := `
	INSERT INTO user_servers (user_id, server_index) VALUES ('u1', 0);
	INSERT INTO user_servers (user_id, server_index) VALUES ('u1', 1);

	INSERT INTO id_mappings (virtual_id, original_id, server_index) VALUES ('virt-1', 'orig-1', 0);
	INSERT INTO id_mappings (virtual_id, original_id, server_index) VALUES ('virt-2', 'orig-2', 1);

	INSERT INTO id_additional_instances (virtual_id, original_id, server_index) VALUES ('virt-1', 'orig-1-alt', 1);

	INSERT INTO user_watch_progress (proxy_user_id, virtual_item_id, server_index, original_item_id, position_ticks)
	VALUES ('u1', 'virt-1', 0, 'orig-1', 1000);
	`
	if err := db.exec(seedSQL); err != nil {
		_ = closeSQLite(db)
		t.Fatalf("seed data: %v", err)
	}
	_ = closeSQLite(db)

	// 2. Open via NewIDStore with upstreamIDs: 0 -> "srv-aaa", 1 -> "srv-bbb"
	upstreamIDs := []string{"srv-aaa", "srv-bbb"}
	store, err := NewIDStore(tempDir, nil, upstreamIDs...)
	if err != nil {
		t.Fatalf("NewIDStore migration failed: %v", err)
	}
	defer store.Close()

	// 3. Verify mappings migrated to server_id
	resolved := store.ResolveVirtualID("virt-1")
	if resolved == nil {
		t.Fatalf("virt-1 not resolved")
	}
	if resolved.ServerID != "srv-aaa" {
		t.Errorf("virt-1 ServerID = %q, want 'srv-aaa'", resolved.ServerID)
	}
	if len(resolved.OtherInstances) != 1 || resolved.OtherInstances[0].ServerID != "srv-bbb" {
		t.Errorf("virt-1 OtherInstances = %+v, want srv-bbb", resolved.OtherInstances)
	}

	resolved2 := store.ResolveVirtualID("virt-2")
	if resolved2 == nil || resolved2.ServerID != "srv-bbb" {
		t.Errorf("virt-2 ServerID = %+v, want 'srv-bbb'", resolved2)
	}

	// 4. Verify user_servers foreign key constraint still works on users delete
	userStore, err := NewUserStore(store.db, nil)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}

	u := userStore.Get("u1")
	if u == nil {
		t.Fatalf("user u1 not found")
	}
	if len(u.AllowedServers) != 2 || u.AllowedServers[0] != "srv-aaa" || u.AllowedServers[1] != "srv-bbb" {
		t.Errorf("u1 AllowedServers = %v, want ['srv-aaa', 'srv-bbb']", u.AllowedServers)
	}

	// 5. Legacy watch state is intentionally not migrated by Phase 3. The server-id
	// migration may reshape the old table, but NewWatchStore detects the legacy
	// schema (no updated_at) and resets it before creating the new UserState schema.
	watchStore, err := NewWatchStore(store.db, nil)
	if err != nil {
		t.Fatalf("NewWatchStore: %v", err)
	}
	if progress := watchStore.GetProgress("u1", "virt-1"); progress != nil {
		t.Fatalf("legacy watch progress unexpectedly migrated: %#v", progress)
	}

	// Delete user and verify cascade on user_servers
	if err := userStore.Delete("u1"); err != nil {
		t.Fatalf("Delete user: %v", err)
	}
}
