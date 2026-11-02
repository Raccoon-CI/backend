package store

import (
	"context"
	"errors"
	"testing"
)

func TestDocumentsAreReplacedAsAWholeAndAMissingOneIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.GetDocument(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.PutDocument(ctx, "x", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDocument(ctx, "x", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDocument(ctx, "y", []byte("other")); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDocument(ctx, "x")
	if err != nil || string(d.Data) != "two" || d.UpdatedAt.IsZero() {
		t.Fatalf("document = %+v, %v", d, err)
	}
}
