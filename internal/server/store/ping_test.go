package store

import (
	"context"
	"testing"
	"time"
)

func TestPingSucceedsOnAWorkingDatabaseAndFailsWhileAnotherWriterHoldsItForGood(t *testing.T) {
	s := open(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("a working database: %v", err)
	}
	// Another connection holds the write lock (a stuck transaction): the server
	// cannot write, and Ping must say so within the time it is given.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO documents (name, data, updated_at) VALUES ('x', x'00', 0)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := s.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded while the write lock was held")
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("Ping took %v; it must give up with its context", took)
	}
	tx.Rollback()
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("after the lock was let go: %v", err)
	}
	// And it wrote nothing.
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&n)
	if n != 0 {
		t.Errorf("Ping left %d rows", n)
	}
}
