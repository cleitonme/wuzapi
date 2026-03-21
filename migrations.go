package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

type Migration struct {
	ID      int
	Name    string
	UpSQL   string
	DownSQL string
	// NoTransaction indica que esta migration deve rodar fora de uma
	// transação explícita. Necessário para CREATE INDEX CONCURRENTLY
	// no PostgreSQL, que falha dentro de um bloco de transação.
	NoTransaction bool
}

var migrations = []Migration{
	{
		ID:    1,
		Name:  "initial_schema",
		UpSQL: initialSchemaSQL,
	},
	{
		ID:   2,
		Name: "add_proxy_url",
		UpSQL: `
            -- PostgreSQL version
            DO $$
            BEGIN
                IF NOT EXISTS (
                    SELECT 1 FROM information_schema.columns
                    WHERE table_name = 'users' AND column_name = 'proxy_url'
                ) THEN
                    ALTER TABLE users ADD COLUMN proxy_url TEXT DEFAULT '';
                END IF;
            END $$;

            -- SQLite version (handled in code)
            `,
	},
	{
		ID:    3,
		Name:  "change_id_to_string",
		UpSQL: changeIDToStringSQL,
	},
	{
		ID:    4,
		Name:  "add_s3_support",
		UpSQL: addS3SupportSQL,
	},
	{
		ID:    5,
		Name:  "add_message_history",
		UpSQL: addMessageHistorySQL,
	},
	{
		ID:    6,
		Name:  "add_quoted_message_id",
		UpSQL: addQuotedMessageIDSQL,
	},
	{
		ID:    7,
		Name:  "add_hmac_key",
		UpSQL: addHmacKeySQL,
	},
	{
		ID:    8,
		Name:  "add_data_json",
		UpSQL: addDataJsonSQL,
	},
	{
		// NoTransaction=true: CREATE INDEX não pode rodar dentro de
		// uma transação no PostgreSQL quando usa certas formas DDL.
		// Sem isso causa FATAL "cannot run inside a transaction block".
		ID:            9,
		Name:          "add_whatsmeow_message_secrets_message_id_idx",
		UpSQL:         addWhatsmeowMessageSecretsMessageIDIndexSQL,
		NoTransaction: true,
	},
	{
		// NoTransaction=true pelo mesmo motivo — DDL de índices fora de tx.
		ID:            10,
		Name:          "add_performance_indexes",
		UpSQL:         addPerformanceIndexesSQL,
		NoTransaction: true,
	},
}

const changeIDToStringSQL = `
-- Migration to change ID from integer to random string
DO $$
BEGIN
    -- Only execute if the column is currently integer type
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'users' AND column_name = 'id' AND data_type = 'integer'
    ) THEN
        -- For PostgreSQL
        ALTER TABLE users ADD COLUMN new_id TEXT;
		UPDATE users SET new_id = md5(random()::text || id::text || clock_timestamp()::text);
		ALTER TABLE users DROP CONSTRAINT users_pkey;
        ALTER TABLE users DROP COLUMN id CASCADE;
        ALTER TABLE users RENAME COLUMN new_id TO id;
        ALTER TABLE users ALTER COLUMN id SET NOT NULL;
        ALTER TABLE users ADD PRIMARY KEY (id);
    END IF;
END $$;
`

const initialSchemaSQL = `
-- PostgreSQL version
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'users') THEN
        CREATE TABLE users (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            token TEXT NOT NULL,
            webhook TEXT NOT NULL DEFAULT '',
            jid TEXT NOT NULL DEFAULT '',
            qrcode TEXT NOT NULL DEFAULT '',
            connected INTEGER,
            expiration INTEGER,
            events TEXT NOT NULL DEFAULT '',
            proxy_url TEXT DEFAULT ''
        );
    END IF;
END $$;

-- SQLite version (handled in code)
`

const addS3SupportSQL = `
-- PostgreSQL version
DO $$
BEGIN
    -- Add S3 configuration columns if they don't exist
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_enabled') THEN
        ALTER TABLE users ADD COLUMN s3_enabled BOOLEAN DEFAULT FALSE;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_endpoint') THEN
        ALTER TABLE users ADD COLUMN s3_endpoint TEXT DEFAULT '';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_region') THEN
        ALTER TABLE users ADD COLUMN s3_region TEXT DEFAULT '';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_bucket') THEN
        ALTER TABLE users ADD COLUMN s3_bucket TEXT DEFAULT '';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_access_key') THEN
        ALTER TABLE users ADD COLUMN s3_access_key TEXT DEFAULT '';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_secret_key') THEN
        ALTER TABLE users ADD COLUMN s3_secret_key TEXT DEFAULT '';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_path_style') THEN
        ALTER TABLE users ADD COLUMN s3_path_style BOOLEAN DEFAULT TRUE;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_public_url') THEN
        ALTER TABLE users ADD COLUMN s3_public_url TEXT DEFAULT '';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'media_delivery') THEN
        ALTER TABLE users ADD COLUMN media_delivery TEXT DEFAULT 'base64';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 's3_retention_days') THEN
        ALTER TABLE users ADD COLUMN s3_retention_days INTEGER DEFAULT 30;
    END IF;
END $$;
`

const addMessageHistorySQL = `
-- PostgreSQL version
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'message_history') THEN
        CREATE TABLE message_history (
            id SERIAL PRIMARY KEY,
            user_id TEXT NOT NULL,
            chat_jid TEXT NOT NULL,
            sender_jid TEXT NOT NULL,
            message_id TEXT NOT NULL,
            timestamp TIMESTAMP NOT NULL,
            message_type TEXT NOT NULL,
            text_content TEXT,
            media_link TEXT,
            UNIQUE(user_id, message_id)
        );
        CREATE INDEX idx_message_history_user_chat_timestamp ON message_history (user_id, chat_jid, timestamp DESC);
    END IF;

    -- Add history column to users table if it doesn't exist
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'history') THEN
        ALTER TABLE users ADD COLUMN history INTEGER DEFAULT 0;
    END IF;
END $$;
`

const addQuotedMessageIDSQL = `
-- PostgreSQL version
DO $$
BEGIN
    -- Add quoted_message_id column to message_history table if it doesn't exist
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'message_history' AND column_name = 'quoted_message_id') THEN
        ALTER TABLE message_history ADD COLUMN quoted_message_id TEXT;
    END IF;
END $$;
`

const addDataJsonSQL = `
-- PostgreSQL version
DO $$
BEGIN
    -- Add dataJson column to message_history table if it doesn't exist
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'message_history' AND column_name = 'datajson') THEN
        ALTER TABLE message_history ADD COLUMN datajson TEXT;
    END IF;
END $$;

-- SQLite version (handled in code)
`

// Removido CONCURRENTLY — IF NOT EXISTS já garante idempotência
// e evita problemas em ambientes gerenciados (RDS, Cloud SQL, etc).
const addWhatsmeowMessageSecretsMessageIDIndexSQL = `
CREATE INDEX IF NOT EXISTS whatsmeow_message_secrets_message_id_idx
ON whatsmeow_message_secrets (message_id);
`

const addPerformanceIndexesSQL = `
DO $$
BEGIN

    -- ----------------------------------------------------------------
    -- GRUPO 1: tabela users
    -- ----------------------------------------------------------------

    -- Lookup por token: hit em TODA requisição autenticada (authalice).
    -- Sem este índice o PostgreSQL faz full scan na tabela users.
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'users' AND indexname = 'idx_users_token'
    ) THEN
        CREATE UNIQUE INDEX idx_users_token ON users (token);
    END IF;

    -- Covering index por id: evita heap fetch nas colunas mais lidas.
    -- Nota: hmac_key (BYTEA) removido do INCLUDE — pode exceder 8191 bytes
    -- e causar erro "index row requires X bytes, maximum size is 8191".
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'users' AND indexname = 'idx_users_id_covering'
    ) THEN
        CREATE INDEX idx_users_id_covering ON users (id)
        INCLUDE (s3_enabled, media_delivery, webhook, events, history);
    END IF;

    -- Índice parcial para connectOnStartup: WHERE connected = 1.
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'users' AND indexname = 'idx_users_connected_partial'
    ) THEN
        CREATE INDEX idx_users_connected_partial ON users (connected)
        WHERE connected = 1;
    END IF;

    -- ----------------------------------------------------------------
    -- GRUPO 2: tabela message_history
    -- ----------------------------------------------------------------

    -- Covering index para trimMessageHistory (chamado após CADA mensagem).
    -- INCLUDE apenas id e message_id — colunas pequenas e fixas.
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'message_history' AND indexname = 'idx_message_history_trim_covering'
    ) THEN
        CREATE INDEX idx_message_history_trim_covering
        ON message_history (user_id, chat_jid, timestamp DESC)
        INCLUDE (id, message_id);
    END IF;

    -- Unique index para deduplicação no INSERT (saveMessageToHistory).
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'message_history' AND indexname = 'idx_message_history_unique_msg'
    ) THEN
        CREATE UNIQUE INDEX idx_message_history_unique_msg
        ON message_history (user_id, message_id);
    END IF;

    -- Índice para GetHistory.
    -- Nota: text_content e media_link removidos do INCLUDE — são TEXT sem
    -- limite de tamanho e causam erro "index row requires X bytes, maximum 8191".
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'message_history' AND indexname = 'idx_message_history_chat_lookup'
    ) THEN
        CREATE INDEX idx_message_history_chat_lookup
        ON message_history (user_id, chat_jid, timestamp DESC)
        INCLUDE (id, sender_jid, message_id, message_type);
    END IF;

    -- Índice para GROUP BY user_id, chat_jid com MAX(timestamp).
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'message_history' AND indexname = 'idx_message_history_group_by'
    ) THEN
        CREATE INDEX idx_message_history_group_by
        ON message_history (user_id, chat_jid, timestamp DESC);
    END IF;

END $$;
`

// GenerateRandomID creates a random string ID
func GenerateRandomID() (string, error) {
	bytes := make([]byte, 16) // 128 bits
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// Initialize the database with migrations
func initializeSchema(db *sqlx.DB) error {
	if err := createMigrationsTable(db); err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	applied, err := getAppliedMigrations(db)
	if err != nil {
		return fmt.Errorf("failed to get applied migrations: %w", err)
	}

	for _, migration := range migrations {
		if _, ok := applied[migration.ID]; !ok {
			if err := applyMigration(db, migration); err != nil {
				return fmt.Errorf("failed to apply migration %d: %w", migration.ID, err)
			}
		}
	}

	return nil
}

func createMigrationsTable(db *sqlx.DB) error {
	var tableExists bool
	var err error

	switch db.DriverName() {
	case "postgres":
		err = db.Get(&tableExists, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_name = 'migrations'
			)`)
	case "sqlite":
		err = db.Get(&tableExists, `
			SELECT EXISTS (
				SELECT 1 FROM sqlite_master
				WHERE type='table' AND name='migrations'
			)`)
	default:
		return fmt.Errorf("unsupported database driver: %s", db.DriverName())
	}

	if err != nil {
		return fmt.Errorf("failed to check migrations table existence: %w", err)
	}

	if tableExists {
		return nil
	}

	_, err = db.Exec(`
		CREATE TABLE migrations (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`)
	if err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	return nil
}

func getAppliedMigrations(db *sqlx.DB) (map[int]struct{}, error) {
	applied := make(map[int]struct{})
	var rows []struct {
		ID   int    `db:"id"`
		Name string `db:"name"`
	}

	err := db.Select(&rows, "SELECT id, name FROM migrations ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("failed to query applied migrations: %w", err)
	}

	for _, row := range rows {
		applied[row.ID] = struct{}{}
	}

	return applied, nil
}

// applyMigration decide se executa com ou sem transação baseado no flag NoTransaction.
func applyMigration(db *sqlx.DB, migration Migration) error {
	if migration.NoTransaction && db.DriverName() == "postgres" {
		return applyMigrationNoTx(db, migration)
	}
	return applyMigrationWithTx(db, migration)
}

// applyMigrationNoTx executa a migration SEM transação (PostgreSQL only).
// Usado para DDL como CREATE INDEX que não pode rodar dentro de tx.
// O registro na tabela migrations é feito em transação separada após o SQL principal.
func applyMigrationNoTx(db *sqlx.DB, migration Migration) error {
	// Executa o SQL principal sem transação
	if _, err := db.Exec(migration.UpSQL); err != nil {
		return fmt.Errorf("failed to execute migration SQL (no-tx): %w", err)
	}

	// Registra em transação separada
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for migration record: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(`
		INSERT INTO migrations (id, name)
		VALUES ($1, $2)`, migration.ID, migration.Name); err != nil {
		return fmt.Errorf("failed to record migration: %w", err)
	}

	return tx.Commit()
}

// applyMigrationWithTx executa a migration dentro de uma transação (comportamento padrão).
func applyMigrationWithTx(db *sqlx.DB, migration Migration) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if migration.ID == 1 {
		if db.DriverName() == "sqlite" {
			err = createTableIfNotExistsSQLite(tx, "users", `
                CREATE TABLE users (
                    id TEXT PRIMARY KEY,
                    name TEXT NOT NULL,
                    token TEXT NOT NULL,
                    webhook TEXT NOT NULL DEFAULT '',
                    jid TEXT NOT NULL DEFAULT '',
                    qrcode TEXT NOT NULL DEFAULT '',
                    connected INTEGER,
                    expiration INTEGER,
                    events TEXT NOT NULL DEFAULT '',
                    proxy_url TEXT DEFAULT ''
                )`)
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 2 {
		if db.DriverName() == "sqlite" {
			err = addColumnIfNotExistsSQLite(tx, "users", "proxy_url", "TEXT DEFAULT ''")
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 3 {
		if db.DriverName() == "sqlite" {
			err = migrateSQLiteIDToString(tx)
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 4 {
		if db.DriverName() == "sqlite" {
			err = addColumnIfNotExistsSQLite(tx, "users", "s3_enabled", "BOOLEAN DEFAULT 0")
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_endpoint", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_region", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_bucket", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_access_key", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_secret_key", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_path_style", "BOOLEAN DEFAULT 1")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_public_url", "TEXT DEFAULT ''")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "media_delivery", "TEXT DEFAULT 'base64'")
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "s3_retention_days", "INTEGER DEFAULT 30")
			}
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 5 {
		if db.DriverName() == "sqlite" {
			err = createTableIfNotExistsSQLite(tx, "message_history", `
				CREATE TABLE message_history (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					user_id TEXT NOT NULL,
					chat_jid TEXT NOT NULL,
					sender_jid TEXT NOT NULL,
					message_id TEXT NOT NULL,
					timestamp DATETIME NOT NULL,
					message_type TEXT NOT NULL,
					text_content TEXT,
					media_link TEXT,
					UNIQUE(user_id, message_id)
				)`)
			if err == nil {
				_, err = tx.Exec(`
					CREATE INDEX IF NOT EXISTS idx_message_history_user_chat_timestamp
					ON message_history (user_id, chat_jid, timestamp DESC)`)
			}
			if err == nil {
				err = addColumnIfNotExistsSQLite(tx, "users", "history", "INTEGER DEFAULT 0")
			}
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 6 {
		if db.DriverName() == "sqlite" {
			err = addColumnIfNotExistsSQLite(tx, "message_history", "quoted_message_id", "TEXT")
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 7 {
		if db.DriverName() == "sqlite" {
			err = addColumnIfNotExistsSQLite(tx, "users", "hmac_key", "BLOB")
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 8 {
		if db.DriverName() == "sqlite" {
			err = addColumnIfNotExistsSQLite(tx, "message_history", "datajson", "TEXT")
		} else {
			_, err = tx.Exec(migration.UpSQL)
		}
	} else if migration.ID == 9 {
		// SQLite: cria índice simples (sem CONCURRENTLY)
		// PostgreSQL: tratado em applyMigrationNoTx (NoTransaction=true)
		if db.DriverName() == "sqlite" {
			_, err = tx.Exec(`
				CREATE INDEX IF NOT EXISTS whatsmeow_message_secrets_message_id_idx
				ON whatsmeow_message_secrets (message_id)`)
		}
	} else if migration.ID == 10 {
		// SQLite: cria índices sem INCLUDE (não suportado)
		// PostgreSQL: tratado em applyMigrationNoTx (NoTransaction=true)
		if db.DriverName() == "sqlite" {
			err = applyPerformanceIndexesSQLite(tx)
		}
	} else {
		_, err = tx.Exec(migration.UpSQL)
	}

	if err != nil {
		return fmt.Errorf("failed to execute migration SQL: %w", err)
	}

	if _, err = tx.Exec(`
        INSERT INTO migrations (id, name)
        VALUES ($1, $2)`, migration.ID, migration.Name); err != nil {
		return fmt.Errorf("failed to record migration: %w", err)
	}

	return tx.Commit()
}

// applyPerformanceIndexesSQLite cria os índices de performance para SQLite.
// SQLite não suporta CONCURRENTLY nem INCLUDE.
func applyPerformanceIndexesSQLite(tx *sqlx.Tx) error {
	indexes := []struct {
		name string
		sql  string
	}{
		{
			name: "idx_users_token",
			sql:  `CREATE UNIQUE INDEX IF NOT EXISTS idx_users_token ON users (token)`,
		},
		{
			name: "idx_users_connected_partial",
			sql:  `CREATE INDEX IF NOT EXISTS idx_users_connected_partial ON users (connected) WHERE connected = 1`,
		},
		{
			name: "idx_message_history_trim_covering",
			sql:  `CREATE INDEX IF NOT EXISTS idx_message_history_trim_covering ON message_history (user_id, chat_jid, timestamp DESC)`,
		},
		{
			name: "idx_message_history_unique_msg",
			sql:  `CREATE UNIQUE INDEX IF NOT EXISTS idx_message_history_unique_msg ON message_history (user_id, message_id)`,
		},
		{
			name: "idx_message_history_chat_lookup",
			sql:  `CREATE INDEX IF NOT EXISTS idx_message_history_chat_lookup ON message_history (user_id, chat_jid, timestamp DESC)`,
		},
		{
			name: "idx_message_history_group_by",
			sql:  `CREATE INDEX IF NOT EXISTS idx_message_history_group_by ON message_history (user_id, chat_jid, timestamp DESC)`,
		},
	}

	for _, idx := range indexes {
		if _, err := tx.Exec(idx.sql); err != nil {
			return fmt.Errorf("failed to create SQLite index %s: %w", idx.name, err)
		}
	}
	return nil
}

func createTableIfNotExistsSQLite(tx *sqlx.Tx, tableName, createSQL string) error {
	var exists int
	err := tx.Get(&exists, `
        SELECT COUNT(*) FROM sqlite_master
        WHERE type='table' AND name=?`, tableName)
	if err != nil {
		return err
	}

	if exists == 0 {
		_, err = tx.Exec(createSQL)
		return err
	}
	return nil
}

func sqliteChangeIDType(tx *sqlx.Tx) error {
	var tableInfo string
	err := tx.Get(&tableInfo, `
        SELECT sql FROM sqlite_master
        WHERE type='table' AND name='users'`)
	if err != nil {
		return fmt.Errorf("failed to get table info: %w", err)
	}

	newTableSQL := strings.Replace(tableInfo,
		"CREATE TABLE users (",
		"CREATE TABLE users_new (id TEXT PRIMARY KEY, ", 1)
	newTableSQL = strings.Replace(newTableSQL,
		"id INTEGER PRIMARY KEY AUTOINCREMENT,", "", 1)

	if _, err = tx.Exec(newTableSQL); err != nil {
		return fmt.Errorf("failed to create new table: %w", err)
	}

	columns, err := getTableColumns(tx, "users")
	if err != nil {
		return fmt.Errorf("failed to get table columns: %w", err)
	}

	var filteredColumns []string
	for _, col := range columns {
		if col != "id" {
			filteredColumns = append(filteredColumns, col)
		}
	}

	columnList := strings.Join(filteredColumns, ", ")
	if _, err = tx.Exec(fmt.Sprintf(`
        INSERT INTO users_new (id, %s)
        SELECT gen_random_uuid(), %s FROM users`,
		columnList, columnList)); err != nil {
		return fmt.Errorf("failed to copy data: %w", err)
	}

	if _, err = tx.Exec("DROP TABLE users"); err != nil {
		return fmt.Errorf("failed to drop old table: %w", err)
	}

	if _, err = tx.Exec("ALTER TABLE users_new RENAME TO users"); err != nil {
		return fmt.Errorf("failed to rename table: %w", err)
	}

	return nil
}

func getTableColumns(tx *sqlx.Tx, tableName string) ([]string, error) {
	var columns []string
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return nil, fmt.Errorf("failed to get table info: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dfltValue interface{}
		var pk int

		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("failed to scan column info: %w", err)
		}
		columns = append(columns, name)
	}

	return columns, nil
}

func migrateSQLiteIDToString(tx *sqlx.Tx) error {
	var currentType string
	err := tx.QueryRow(`
        SELECT type FROM pragma_table_info('users')
        WHERE name = 'id'`).Scan(&currentType)
	if err != nil {
		return fmt.Errorf("failed to check column type: %w", err)
	}

	if currentType != "INTEGER" {
		return nil
	}

	_, err = tx.Exec(`
        CREATE TABLE users_new (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            token TEXT NOT NULL,
            webhook TEXT NOT NULL DEFAULT '',
            jid TEXT NOT NULL DEFAULT '',
            qrcode TEXT NOT NULL DEFAULT '',
            connected INTEGER,
            expiration INTEGER,
            events TEXT NOT NULL DEFAULT '',
            proxy_url TEXT DEFAULT ''
        )`)
	if err != nil {
		return fmt.Errorf("failed to create new table: %w", err)
	}

	_, err = tx.Exec(`
        INSERT INTO users_new
        SELECT
            hex(randomblob(16)),
            name, token, webhook, jid, qrcode,
            connected, expiration, events, proxy_url
        FROM users`)
	if err != nil {
		return fmt.Errorf("failed to copy data: %w", err)
	}

	_, err = tx.Exec(`DROP TABLE users`)
	if err != nil {
		return fmt.Errorf("failed to drop old table: %w", err)
	}

	_, err = tx.Exec(`ALTER TABLE users_new RENAME TO users`)
	if err != nil {
		return fmt.Errorf("failed to rename table: %w", err)
	}

	return nil
}

func addColumnIfNotExistsSQLite(tx *sqlx.Tx, tableName, columnName, columnDef string) error {
	var exists int
	err := tx.Get(&exists, `
        SELECT COUNT(*) FROM pragma_table_info(?)
        WHERE name = ?`, tableName, columnName)
	if err != nil {
		return fmt.Errorf("failed to check column existence: %w", err)
	}

	if exists == 0 {
		_, err = tx.Exec(fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN %s %s",
			tableName, columnName, columnDef))
		if err != nil {
			return fmt.Errorf("failed to add column: %w", err)
		}
	}
	return nil
}

const addHmacKeySQL = `
-- PostgreSQL version - Add encrypted HMAC key column
DO $$
BEGIN
    -- Add hmac_key column as BYTEA for encrypted data
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'hmac_key') THEN
        ALTER TABLE users ADD COLUMN hmac_key BYTEA;
    END IF;
END $$;

-- SQLite version (handled in code)
`
