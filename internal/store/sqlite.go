package store

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	databaseSchemaVersion = 7
	defaultChangeLogLimit = 5000
	secretPrefix          = "enc:v1:"
)

var entityTables = []string{
	"admins",
	"web_sessions",
	"apple_accounts",
	"mailboxes",
	"domain_mail_routes",
	"domain_mail_settings",
	"mailbox_leases",
	"messages",
	"events",
	"settings",
	"create_settings",
	"icloud_sessions",
	"mail_message_index",
	"mailbox_history_states",
}

// Change 描述一次已经提交到 SQLite 的数据变更，供 SSE 客户端增量刷新。
type Change struct {
	Sequence   int64           `json:"sequence"`
	Type       string          `json:"type"`
	Resource   string          `json:"resource"`
	ResourceID string          `json:"resource_id,omitempty"`
	Operation  string          `json:"operation"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// DatabaseStatus 是系统设置页展示的 SQLite 运行状态。
type DatabaseStatus struct {
	Path              string `json:"path"`
	SchemaVersion     int    `json:"schema_version"`
	JournalMode       string `json:"journal_mode"`
	DatabaseBytes     int64  `json:"database_bytes"`
	WALBytes          int64  `json:"wal_bytes"`
	ChangeLogCount    int    `json:"change_log_count"`
	LatestSequence    int64  `json:"latest_sequence"`
	EncryptedFields   bool   `json:"encrypted_fields"`
	LastMaintenanceAt string `json:"last_maintenance_at,omitempty"`
	LastMaintenance   string `json:"last_maintenance,omitempty"`
}

type changeHub struct {
	mu          sync.RWMutex
	nextID      uint64
	subscribers map[uint64]chan Change
}

func newChangeHub() *changeHub {
	return &changeHub{subscribers: make(map[uint64]chan Change)}
}

func (h *changeHub) subscribe(buffer int) (<-chan Change, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	channel := make(chan Change, buffer)
	h.subscribers[id] = channel
	h.mu.Unlock()
	return channel, func() {
		h.mu.Lock()
		if current, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(current)
		}
		h.mu.Unlock()
	}
}

func (h *changeHub) publish(changes []Change) {
	if len(changes) == 0 {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, change := range changes {
		for _, channel := range h.subscribers {
			select {
			case channel <- change:
			default:
			}
		}
	}
}

type secretCodec struct {
	aead cipher.AEAD
}

func loadSecretCodec(path string) (*secretCodec, error) {
	keyPath := path + ".key"
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("生成数据库加密密钥失败：%w", err)
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			return nil, fmt.Errorf("保存数据库加密密钥失败：%w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("读取数据库加密密钥失败：%w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("数据库加密密钥长度不正确")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretCodec{aead: aead}, nil
}

func (c *secretCodec) encrypt(value string) (string, error) {
	if value == "" || strings.HasPrefix(value, secretPrefix) {
		return value, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(value), nil)
	return secretPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *secretCodec) decrypt(value string) (string, error) {
	if !strings.HasPrefix(value, secretPrefix) {
		return value, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, secretPrefix))
	if err != nil || len(data) < c.aead.NonceSize() {
		return "", errors.New("数据库敏感字段密文损坏")
	}
	nonce := data[:c.aead.NonceSize()]
	plain, err := c.aead.Open(nil, nonce, data[c.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("数据库敏感字段解密失败")
	}
	return string(plain), nil
}

func (s *Store) openDatabase() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	codec, err := loadSecretCodec(s.path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", s.path)
	if err != nil {
		return err
	}
	// SQLite 写事务串行执行；单连接可确保每个连接都应用相同 PRAGMA。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA wal_autocheckpoint = 1000",
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return fmt.Errorf("初始化 SQLite 失败：%w", err)
		}
	}
	if err := migrateDatabase(db); err != nil {
		_ = db.Close()
		return err
	}
	s.db = db
	s.codec = codec
	if err := os.Chmod(s.path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = db.Close()
		return err
	}
	if err := s.encryptSensitiveRows(); err != nil {
		_ = db.Close()
		return err
	}
	return nil
}

type schemaMigration struct {
	version    int
	statements []string
}

func migrateDatabase(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("创建数据库迁移表失败：%w", err)
	}
	migrations := []schemaMigration{
		{version: 1, statements: migrationV1()},
		{version: 2, statements: migrationV2()},
		{version: 3, statements: migrationV3()},
		{version: 4, statements: migrationV4()},
		{version: 5, statements: migrationV5()},
		{version: 6, statements: migrationV6()},
		{version: 7, statements: migrationV7()},
	}
	for _, migration := range migrations {
		var applied int
		err := db.QueryRow(`SELECT 1 FROM schema_migrations WHERE version = ?`, migration.version).Scan(&applied)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, statement := range migration.statements {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("执行数据库迁移 v%d 失败：%w", migration.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, migration.version, time.Now().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func migrationV1() []string {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	for _, table := range entityTables {
		statements = append(statements, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			data_json BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`, table))
	}
	return append(statements,
		`CREATE TABLE IF NOT EXISTS change_log (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			event_type TEXT NOT NULL,
			resource TEXT NOT NULL,
			resource_id TEXT NOT NULL DEFAULT '',
			operation TEXT NOT NULL,
			payload_json BLOB NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_change_log_created ON change_log(created_at)`,
		`INSERT OR IGNORE INTO metadata(key, value) VALUES ('next_id', '1')`,
		`INSERT OR IGNORE INTO metadata(key, value) VALUES ('created_at', '')`,
		`INSERT OR IGNORE INTO metadata(key, value) VALUES ('updated_at', '')`,
	)
}

func migrationV2() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS runtime_states (
			id TEXT PRIMARY KEY,
			data_json BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS maintenance_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			operation TEXT NOT NULL,
			status TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mailboxes_status_account_created ON mailboxes (
			json_extract(data_json, '$.status'),
			json_extract(data_json, '$.account_id'),
			json_extract(data_json, '$.created_at') DESC
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_mailboxes_email ON mailboxes(lower(json_extract(data_json, '$.email')))`,
		`CREATE INDEX IF NOT EXISTS idx_messages_mailbox_received ON messages (
			json_extract(data_json, '$.mailbox_id'),
			json_extract(data_json, '$.received_at') DESC
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_remote ON messages (
			json_extract(data_json, '$.mailbox_id'),
			json_extract(data_json, '$.remote_id')
		) WHERE COALESCE(json_extract(data_json, '$.remote_id'), '') <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_leases_state_expiry ON mailbox_leases (
			json_extract(data_json, '$.state'),
			json_extract(data_json, '$.expires_at')
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_created ON events(json_extract(data_json, '$.created_at') DESC)`,
	}
}

func migrationV3() []string {
	return []string{
		`DELETE FROM messages WHERE json_extract(data_json, '$.mailbox_id') NOT IN (SELECT id FROM mailboxes)`,
		`DELETE FROM mailbox_leases WHERE json_extract(data_json, '$.mailbox_id') NOT IN (SELECT id FROM mailboxes)`,
		`CREATE TRIGGER IF NOT EXISTS messages_mailbox_insert_guard
		BEFORE INSERT ON messages
		WHEN NOT EXISTS (SELECT 1 FROM mailboxes WHERE id = json_extract(NEW.data_json, '$.mailbox_id'))
		BEGIN SELECT RAISE(ABORT, '邮件关联的邮箱不存在'); END`,
		`CREATE TRIGGER IF NOT EXISTS messages_mailbox_update_guard
		BEFORE UPDATE OF data_json ON messages
		WHEN NOT EXISTS (SELECT 1 FROM mailboxes WHERE id = json_extract(NEW.data_json, '$.mailbox_id'))
		BEGIN SELECT RAISE(ABORT, '邮件关联的邮箱不存在'); END`,
		`CREATE TRIGGER IF NOT EXISTS leases_mailbox_insert_guard
		BEFORE INSERT ON mailbox_leases
		WHEN NOT EXISTS (SELECT 1 FROM mailboxes WHERE id = json_extract(NEW.data_json, '$.mailbox_id'))
		BEGIN SELECT RAISE(ABORT, '租约关联的邮箱不存在'); END`,
	}
}

func migrationV4() []string {
	return []string{
		// 清理旧逐邮箱同步和会话访问产生的高频通知；业务实体仍完整保留。
		`DELETE FROM change_log WHERE event_type IN ('mailbox.updated', 'web-session.updated')`,
	}
}

func migrationV5() []string {
	return []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_canonical ON messages (
			json_extract(data_json, '$.mailbox_id'),
			json_extract(data_json, '$.canonical_id')
		) WHERE COALESCE(json_extract(data_json, '$.canonical_id'), '') <> ''`,
	}
}

func migrationV6() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS domain_mail_routes (
			id TEXT PRIMARY KEY,
			data_json BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS domain_mail_settings (
			id TEXT PRIMARY KEY,
			data_json BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_domain_mail_routes_domain ON domain_mail_routes(lower(json_extract(data_json, '$.domain')))`,
		`CREATE INDEX IF NOT EXISTS idx_mailboxes_kind_domain_route ON mailboxes (
			COALESCE(json_extract(data_json, '$.mailbox_kind'), 'icloud_hme'),
			json_extract(data_json, '$.domain_route_id'),
			json_extract(data_json, '$.created_at') DESC
		)`,
	}
}

func migrationV7() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS mail_message_index (
			id TEXT PRIMARY KEY,
			data_json BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mailbox_history_states (
			id TEXT PRIMARY KEY,
			data_json BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_mail_message_index_source_uid ON mail_message_index (
			json_extract(data_json, '$.source_key'),
			json_extract(data_json, '$.folder'),
			json_extract(data_json, '$.uid_validity'),
			json_extract(data_json, '$.uid')
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mail_message_index_source_received ON mail_message_index (
			json_extract(data_json, '$.source_key'),
			json_extract(data_json, '$.received_at') DESC
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mailbox_history_source_complete ON mailbox_history_states (
			json_extract(data_json, '$.source_key'),
			json_extract(data_json, '$.complete')
		)`,
	}
}

func (s *Store) encryptSensitiveRows() error {
	for _, table := range []string{"apple_accounts", "mailboxes", "settings", "icloud_sessions", "domain_mail_routes"} {
		rows, err := s.db.Query(`SELECT id, data_json FROM ` + table)
		if err != nil {
			return err
		}
		type rowData struct {
			id   string
			data []byte
		}
		items := []rowData{}
		for rows.Next() {
			var item rowData
			if err := rows.Scan(&item.id, &item.data); err != nil {
				_ = rows.Close()
				return err
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range items {
			protected, err := s.protectJSON(table, item.data)
			if err != nil {
				return err
			}
			if bytes.Equal(item.data, protected) {
				continue
			}
			if _, err := s.db.Exec(`UPDATE `+table+` SET data_json = ?, updated_at = ? WHERE id = ?`, protected, time.Now().Format(time.RFC3339Nano), item.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) protectJSON(table string, plain []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(plain, &value); err != nil {
		return nil, err
	}
	if err := s.transformSecrets(table, value, true); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (s *Store) unprotectJSON(table string, protected []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(protected, &value); err != nil {
		return nil, err
	}
	if err := s.transformSecrets(table, value, false); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (s *Store) transformSecrets(table string, value any, encrypt bool) error {
	keys := map[string]bool{}
	switch table {
	case "apple_accounts":
		keys["password"] = true
	case "mailboxes":
		keys["api_token"] = true
	case "settings":
		keys["public_api_key"] = true
		keys["server_chan_send_key"] = true
	case "icloud_sessions":
		for _, key := range []string{"value", "api_key", "data_access_token", "imap_app_password"} {
			keys[key] = true
		}
	case "domain_mail_routes":
		keys["imap_password"] = true
	}
	if len(keys) == 0 {
		return nil
	}
	var walk func(any) error
	walk = func(current any) error {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if keys[key] {
					if text, ok := child.(string); ok && text != "" {
						var transformed string
						var err error
						if encrypt {
							transformed, err = s.codec.encrypt(text)
						} else {
							transformed, err = s.codec.decrypt(text)
						}
						if err != nil {
							return err
						}
						typed[key] = transformed
					}
				}
				if err := walk(typed[key]); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func (s *Store) marshalEntity(table string, value any) ([]byte, []byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	protected, err := s.protectJSON(table, plain)
	return plain, protected, err
}

func (s *Store) decodeEntity(table string, data []byte, target any) error {
	plain, err := s.unprotectJSON(table, data)
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, target)
}

func (s *Store) readEntity(table, id string, target any) (bool, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT data_json FROM `+table+` WHERE id = ?`, strings.TrimSpace(id)).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.decodeEntity(table, data, target)
}

func (s *Store) readEntityTx(tx *sql.Tx, table, id string, target any) (bool, error) {
	var data []byte
	err := tx.QueryRow(`SELECT data_json FROM `+table+` WHERE id = ?`, strings.TrimSpace(id)).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.decodeEntity(table, data, target)
}

func (s *Store) upsertEntityTx(tx *sql.Tx, table, resource, id string, value any) (Change, bool, error) {
	plain, protected, err := s.marshalEntity(table, value)
	if err != nil {
		return Change{}, false, err
	}
	var existing []byte
	err = tx.QueryRow(`SELECT data_json FROM `+table+` WHERE id = ?`, id).Scan(&existing)
	operation := "updated"
	if errors.Is(err, sql.ErrNoRows) {
		operation = "created"
	} else if err != nil {
		return Change{}, false, err
	} else {
		existingPlain, err := s.unprotectJSON(table, existing)
		if err != nil {
			return Change{}, false, err
		}
		if bytes.Equal(existingPlain, plain) {
			return Change{}, false, nil
		}
	}
	now := time.Now()
	if _, err := tx.Exec(`INSERT INTO `+table+`(id, data_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET data_json = excluded.data_json, updated_at = excluded.updated_at`, id, protected, now.Format(time.RFC3339Nano)); err != nil {
		return Change{}, false, err
	}
	return makeEntityChange(table, resource, id, operation, plain, now), true, nil
}

func (s *Store) deleteEntityTx(tx *sql.Tx, table, resource, id string) (Change, bool, error) {
	result, err := tx.Exec(`DELETE FROM `+table+` WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return Change{}, false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return Change{}, false, nil
	}
	now := time.Now()
	return makeEntityChange(table, resource, id, "deleted", nil, now), true, nil
}

func makeEntityChange(table, resource, id, operation string, plain []byte, now time.Time) Change {
	payload := map[string]any{"id": id, "operation": operation}
	if len(plain) > 0 && table != "icloud_sessions" && table != "web_sessions" && table != "admins" {
		var data any
		if json.Unmarshal(plain, &data) == nil {
			redactSecrets(data)
			payload["data"] = data
		}
	}
	payloadData, _ := json.Marshal(payload)
	return Change{
		Type: resource + "." + operation, Resource: resource, ResourceID: id,
		Operation: operation, Payload: payloadData, CreatedAt: now,
	}
}

func redactSecrets(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch key {
			case "api_token", "public_api_key", "server_chan_send_key", "password", "password_hash", "value", "api_key", "data_access_token", "imap_app_password":
				delete(typed, key)
			default:
				redactSecrets(child)
			}
		}
	case []any:
		for _, child := range typed {
			redactSecrets(child)
		}
	}
}

func (s *Store) nextIDTx(tx *sql.Tx, prefix string) (string, error) {
	var raw string
	if err := tx.QueryRow(`SELECT value FROM metadata WHERE key = 'next_id'`).Scan(&raw); err != nil {
		return "", err
	}
	var next int
	if _, err := fmt.Sscanf(raw, "%d", &next); err != nil || next < 1 {
		next = 1
	}
	if _, err := tx.Exec(`UPDATE metadata SET value = ? WHERE key = 'next_id'`, fmt.Sprintf("%d", next+1)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%06d", prefix, next), nil
}

func (s *Store) commitTx(tx *sql.Tx, changes []Change) error {
	validChanges := changes[:0]
	for _, change := range changes {
		if strings.TrimSpace(change.Resource) != "" {
			validChanges = append(validChanges, change)
		}
	}
	changes = validChanges
	for index := range changes {
		sequence, err := insertChange(tx, changes[index])
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		changes[index].Sequence = sequence
	}
	limit := s.changeLogLimit
	if limit <= 0 {
		limit = defaultChangeLogLimit
	}
	if _, err := tx.Exec(`DELETE FROM change_log WHERE sequence <= (SELECT COALESCE(MAX(sequence), 0) - ? FROM change_log)`, limit); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO metadata(key, value) VALUES ('updated_at', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, time.Now().Format(time.RFC3339Nano)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changes.publish(changes)
	return nil
}

func insertChange(tx *sql.Tx, change Change) (int64, error) {
	result, err := tx.Exec(`INSERT INTO change_log(event_type, resource, resource_id, operation, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`, change.Type, change.Resource, change.ResourceID, change.Operation, []byte(change.Payload), change.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SubscribeChanges 订阅当前进程中已经提交的实时变更。
func (s *Store) SubscribeChanges(buffer int) (<-chan Change, func()) {
	return s.changes.subscribe(buffer)
}

// ChangesAfter 返回指定序号之后的持久化变更，用于 SSE 断线续传。
func (s *Store) ChangesAfter(sequence int64, limit int) ([]Change, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT sequence, event_type, resource, resource_id, operation, payload_json, created_at FROM change_log WHERE sequence > ? ORDER BY sequence ASC LIMIT ?`, sequence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := make([]Change, 0)
	for rows.Next() {
		var change Change
		var payload []byte
		var createdAt string
		if err := rows.Scan(&change.Sequence, &change.Type, &change.Resource, &change.ResourceID, &change.Operation, &payload, &createdAt); err != nil {
			return nil, err
		}
		change.Payload = payload
		change.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func (s *Store) LatestChangeSequence() (int64, error) {
	var sequence int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM change_log`).Scan(&sequence)
	return sequence, err
}

func (s *Store) SetChangeLogLimit(limit int) {
	if limit < 1000 {
		limit = defaultChangeLogLimit
	}
	s.mu.Lock()
	s.changeLogLimit = limit
	s.mu.Unlock()
}

func (s *Store) DatabaseStatus() DatabaseStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := DatabaseStatus{Path: s.path, SchemaVersion: databaseSchemaVersion, EncryptedFields: s.codec != nil}
	_ = s.db.QueryRow(`PRAGMA journal_mode`).Scan(&status.JournalMode)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&status.ChangeLogCount)
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM change_log`).Scan(&status.LatestSequence)
	var operation, createdAt string
	if err := s.db.QueryRow(`SELECT operation, created_at FROM maintenance_runs WHERE status = 'success' ORDER BY id DESC LIMIT 1`).Scan(&operation, &createdAt); err == nil {
		status.LastMaintenance, status.LastMaintenanceAt = operation, createdAt
	}
	if info, err := os.Stat(s.path); err == nil {
		status.DatabaseBytes = info.Size()
	}
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		status.WALBytes = info.Size()
	}
	return status
}

func (s *Store) RecordMaintenance(operation, status, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO maintenance_runs(operation, status, detail, created_at) VALUES (?, ?, ?, ?)`, strings.TrimSpace(operation), strings.TrimSpace(status), strings.TrimSpace(detail), time.Now().Format(time.RFC3339Nano))
	return err
}

// PublishRealtimeChange 发布不属于持久化实体的运行时变更。
func (s *Store) PublishRealtimeChange(resource, resourceID, operation string, payload any) error {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return errors.New("实时变更资源不能为空")
	}
	if operation = strings.TrimSpace(operation); operation == "" {
		operation = "updated"
	}
	payloadData, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if payload == nil {
		payloadData = []byte(`{}`)
	}
	change := Change{Type: resource + "." + operation, Resource: resource, ResourceID: strings.TrimSpace(resourceID), Operation: operation, Payload: payloadData, CreatedAt: time.Now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	return s.commitTx(tx, []Change{change})
}

// SaveRuntimeState 持久化后台运行状态，并按需发布一条实时变更。
func (s *Store) SaveRuntimeState(id string, value any, publish bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("运行状态标识不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	change, changed, err := s.upsertEntityTx(tx, "runtime_states", id, id, value)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !changed {
		_ = tx.Rollback()
		return nil
	}
	if !publish {
		change = Change{}
	}
	changes := []Change{}
	if change.Resource != "" {
		changes = append(changes, change)
	}
	return s.commitTx(tx, changes)
}

func (s *Store) LoadRuntimeState(id string, target any) (bool, error) {
	return s.readEntity("runtime_states", strings.TrimSpace(id), target)
}

// IntegrityCheck 执行 SQLite 快速完整性检查。
func (s *Store) IntegrityCheck() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result string
	err := s.db.QueryRow(`PRAGMA quick_check`).Scan(&result)
	return result, err
}

// Checkpoint 主动合并并截断 WAL 文件。
func (s *Store) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Vacuum 回收已经释放的数据库页。
func (s *Store) Vacuum() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`VACUUM`)
	return err
}

// BackupDatabase 使用 SQLite VACUUM INTO 创建一致性在线备份，并复制对应密钥。
func (s *Store) BackupDatabase(destination string) error {
	destination = filepath.Clean(strings.TrimSpace(destination))
	if destination == "." || destination == "" {
		return errors.New("备份路径不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		return errors.New("备份文件已经存在")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	escaped := strings.ReplaceAll(destination, "'", "''")
	if _, err := s.db.Exec(`VACUUM INTO '` + escaped + `'`); err != nil {
		return err
	}
	key, err := os.ReadFile(s.path + ".key")
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination+".key", key, 0o600); err != nil {
		return err
	}
	return os.Chmod(destination, 0o600)
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	_ = s.Checkpoint()
	return s.db.Close()
}
