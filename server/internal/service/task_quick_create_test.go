package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestEnqueueQuickCreateTaskAttachmentsRequireTxStarter(t *testing.T) {
	svc := &TaskService{}
	_, err := svc.EnqueueQuickCreateTask(
		context.Background(),
		pgtype.UUID{},
		pgtype.UUID{},
		pgtype.UUID{},
		pgtype.UUID{},
		"prompt",
		"high",
		"",
		pgtype.UUID{},
		pgtype.UUID{},
		[]pgtype.UUID{{Valid: true}},
	)
	if !errors.Is(err, ErrQuickCreateTransactionRequired) {
		t.Fatalf("EnqueueQuickCreateTask error = %v, want %v", err, ErrQuickCreateTransactionRequired)
	}
}
