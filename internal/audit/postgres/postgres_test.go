package postgres_test

import (
	"os"
	"testing"

	"github.com/vorash/vora/internal/audit/postgres/postgrestest"
)

// TestPostgres runs the store's suite against the server VORA_TEST_POSTGRES_DSN
// names, for working where no container can run. It creates and drops its own
// databases there. CI runs the same suite in tests/integration, on testcontainers.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("VORA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("VORA_TEST_POSTGRES_DSN is not set")
	}
	postgrestest.Run(t, dsn)
}
