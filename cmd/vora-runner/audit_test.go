package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// An operator who set VORA_AUDIT_DSN asked for durable audit. Serving on the
// ephemeral JSONL store instead would silently give them the weaker guarantee,
// so an unreachable database must stop the runner (systemd restarts it).
func TestOpenAuditFailsWhenPostgresUnreachable(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	lis.Close() // nothing listens here: the connection is refused at once

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dsn := fmt.Sprintf("postgres://vora:vora@127.0.0.1:%d/vora?sslmode=disable&connect_timeout=2", port)
	store, err := openAudit(ctx, dsn)
	if err == nil {
		store.Close()
		t.Fatal("openAudit with an unreachable database returned no error")
	}
}
