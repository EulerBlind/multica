package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/migrations"
)

var attachmentMigrationVersions = []string{
	"193_attachment_lifecycle",
	"194_attachment_quick_create_reservation",
	"195_attachment_quick_create_reservation_index",
}

var legacyAttachmentMigrationLedger = []string{
	"164_attachment_lifecycle",
	"164_attachment_task_id",
	"165_attachment_quick_create_reservation",
	"165_attachment_task_id_index",
	"166_attachment_quick_create_reservation_index",
	"166_project_dates",
}

func TestAttachmentMigrationsSupportFreshAndLegacyNumberedSchemas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
	}{
		{name: "fresh schema"},
		{name: "legacy 164 165 166 schema", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := newAttachmentMigrationTestPool(t)
			createAttachmentMigrationBaseSchema(t, pool)
			if tc.legacy {
				createLegacyAttachmentMigrationState(t, pool)
			}

			dir, err := migrations.ResolveDir()
			if err != nil {
				t.Fatalf("resolve migrations: %v", err)
			}
			files := make([]string, 0, len(attachmentMigrationVersions))
			for _, version := range attachmentMigrationVersions {
				path := filepath.Join(dir, version+".up.sql")
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("stat migration %s: %v", path, err)
				}
				files = append(files, path)
			}

			if err := runMigrations(context.Background(), pool, runOptions{
				Direction:             "up",
				Files:                 files,
				SchemaMigrationsTable: schema + ".schema_migrations",
				AdvisoryLockKey:       time.Now().UnixNano(),
			}); err != nil {
				t.Fatalf("apply attachment migrations: %v", err)
			}

			assertAttachmentMigrationSchema(t, pool, schema, tc.legacy)
		})
	}
}

func newAttachmentMigrationTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	basePool := openTestPool(t)
	schema := "attachment_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := basePool.Exec(context.Background(), "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := basePool.Exec(ctx, "DROP SCHEMA IF EXISTS "+quotedSchema+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	config, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open schema pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping schema pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, schema
}

func createAttachmentMigrationBaseSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		CREATE TABLE agent_task_queue (
			id UUID PRIMARY KEY,
			status TEXT NOT NULL
		);
		CREATE TABLE attachment (
			id UUID PRIMARY KEY,
			issue_id UUID,
			comment_id UUID,
			chat_message_id UUID
		);
		INSERT INTO attachment (id, issue_id) VALUES
			('00000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001'),
			('00000000-0000-0000-0000-000000000002', NULL);
	`)
	if err != nil {
		t.Fatalf("create base schema: %v", err)
	}
}

func createLegacyAttachmentMigrationState(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		ALTER TABLE attachment
		  ADD COLUMN lifecycle TEXT NOT NULL DEFAULT 'draft'
		    CHECK (lifecycle IN ('draft', 'committed'));
		UPDATE attachment
		SET lifecycle = 'committed'
		WHERE issue_id IS NOT NULL
		   OR comment_id IS NOT NULL
		   OR chat_message_id IS NOT NULL;

		ALTER TABLE attachment
		  ADD COLUMN quick_create_task_id UUID
		    REFERENCES agent_task_queue(id) ON DELETE SET NULL;

		CREATE OR REPLACE FUNCTION release_quick_create_attachments_on_terminal_state()
		RETURNS TRIGGER AS $$
		BEGIN
		  IF NEW.status IN ('completed', 'failed', 'cancelled')
		     AND OLD.status IS DISTINCT FROM NEW.status THEN
		    UPDATE attachment
		    SET quick_create_task_id = NULL
		    WHERE quick_create_task_id = NEW.id
		      AND lifecycle = 'draft';
		  END IF;
		  RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		CREATE TRIGGER trg_release_quick_create_attachments
		  AFTER UPDATE OF status ON agent_task_queue
		  FOR EACH ROW
		  EXECUTE FUNCTION release_quick_create_attachments_on_terminal_state();

		CREATE TABLE schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		INSERT INTO schema_migrations (version) VALUES
			('164_attachment_lifecycle'),
			('164_attachment_task_id'),
			('165_attachment_quick_create_reservation'),
			('165_attachment_task_id_index'),
			('166_attachment_quick_create_reservation_index'),
			('166_project_dates');
	`)
	if err != nil {
		t.Fatalf("create legacy attachment schema: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		CREATE INDEX CONCURRENTLY idx_attachment_quick_create_task
		  ON attachment(quick_create_task_id)
		  WHERE quick_create_task_id IS NOT NULL
	`); err != nil {
		t.Fatalf("create legacy attachment index: %v", err)
	}
}

func assertAttachmentMigrationSchema(t *testing.T, pool *pgxpool.Pool, schema string, legacy bool) {
	t.Helper()
	ctx := context.Background()

	for id, want := range map[string]string{
		"00000000-0000-0000-0000-000000000001": "committed",
		"00000000-0000-0000-0000-000000000002": "draft",
	} {
		var got string
		if err := pool.QueryRow(ctx, "SELECT lifecycle FROM attachment WHERE id = $1", id).Scan(&got); err != nil {
			t.Fatalf("read lifecycle for %s: %v", id, err)
		}
		if got != want {
			t.Fatalf("attachment %s lifecycle = %q, want %q", id, got, want)
		}
	}

	for _, constraint := range []string{
		"attachment_lifecycle_check",
		"attachment_quick_create_task_id_fkey",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = $1 AND conrelid = 'attachment'::regclass
			)
		`, constraint).Scan(&exists); err != nil {
			t.Fatalf("check constraint %s: %v", constraint, err)
		}
		if !exists {
			t.Fatalf("constraint %s is missing", constraint)
		}
	}

	var indexExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = $1 AND indexname = 'idx_attachment_quick_create_task'
		)
	`, schema).Scan(&indexExists); err != nil {
		t.Fatalf("check reservation index: %v", err)
	}
	if !indexExists {
		t.Fatal("idx_attachment_quick_create_task is missing")
	}

	const taskID = "20000000-0000-0000-0000-000000000001"
	const attachmentID = "30000000-0000-0000-0000-000000000001"
	if _, err := pool.Exec(ctx, "INSERT INTO agent_task_queue (id, status) VALUES ($1, 'queued')", taskID); err != nil {
		t.Fatalf("insert trigger task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO attachment (id, lifecycle, quick_create_task_id)
		VALUES ($1, 'draft', $2)
	`, attachmentID, taskID); err != nil {
		t.Fatalf("insert reserved attachment: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_task_queue SET status = 'failed' WHERE id = $1", taskID); err != nil {
		t.Fatalf("mark trigger task failed: %v", err)
	}
	var reservation *string
	if err := pool.QueryRow(ctx, "SELECT quick_create_task_id::text FROM attachment WHERE id = $1", attachmentID).Scan(&reservation); err != nil {
		t.Fatalf("read terminal reservation: %v", err)
	}
	if reservation != nil {
		t.Fatalf("terminal reservation = %q, want NULL", *reservation)
	}

	var newCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM schema_migrations WHERE version = ANY($1)
	`, attachmentMigrationVersions).Scan(&newCount); err != nil {
		t.Fatalf("count new migration versions: %v", err)
	}
	if newCount != len(attachmentMigrationVersions) {
		t.Fatalf("new migration version count = %d, want %d", newCount, len(attachmentMigrationVersions))
	}

	if legacy {
		var oldCount int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM schema_migrations
			WHERE version = ANY($1)
		`, legacyAttachmentMigrationLedger).Scan(&oldCount); err != nil {
			t.Fatalf("count legacy migration versions: %v", err)
		}
		if oldCount != len(legacyAttachmentMigrationLedger) {
			t.Fatalf("legacy migration version count = %d, want %d", oldCount, len(legacyAttachmentMigrationLedger))
		}
	}

	t.Logf("attachment migrations verified: schema=%s legacy=%t versions=%s", schema, legacy, fmt.Sprint(attachmentMigrationVersions))
}
