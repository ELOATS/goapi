package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"ELOATS/goapi/internal/domain"
	"ELOATS/goapi/internal/store"
)

type failingEvents struct{}

func (failingEvents) Append(context.Context, domain.Event) error { return errors.New("boom") }

type countingTx struct{ calls int }

func (c *countingTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	c.calls++
	return fn(ctx)
}

func newMemService() *UserService {
	return NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
}

func TestCreateRunsInTxAndFailsWhenEventFails(t *testing.T) {
	tx := &countingTx{}
	svc := NewUserService(store.NewMemoryUserStore(), failingEvents{}, tx)

	u, err := svc.Create(context.Background(), "A", "a@example.com")
	if err == nil {
		t.Fatal("expected error when event append fails")
	}
	if u.ID != "" {
		t.Fatalf("expected zero user on error, got %+v", u)
	}
	if tx.calls != 1 {
		t.Fatalf("WithTx calls = %d, want 1", tx.calls)
	}
}

func TestListPaginatesWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	svc := newMemService()
	for i := range 5 {
		if _, err := svc.Create(ctx, fmt.Sprintf("u%d", i), fmt.Sprintf("u%d@example.com", i)); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	var after *domain.UserCursor
	pages := 0
	for {
		page, err := svc.List(ctx, domain.ListUsersParams{Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, u := range page.Items {
			if seen[u.ID] {
				t.Fatalf("duplicate user %s on page %d", u.ID, pages)
			}
			seen[u.ID] = true
		}
		if page.Next == nil {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		after = page.Next
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("seen=%d pages=%d, want 5 and 3", len(seen), pages)
	}
}

func TestListDefaultsAndEmpty(t *testing.T) {
	page, err := newMemService().List(context.Background(), domain.ListUsersParams{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items == nil || len(page.Items) != 0 || page.Next != nil {
		t.Fatalf("unexpected empty page: %+v", page)
	}
}

func TestCreateRecordsActor(t *testing.T) {
	events := store.NewMemoryEventStore()
	svc := NewUserService(store.NewMemoryUserStore(), events, store.NoopTxManager{})

	ctx := domain.WithActor(context.Background(), "alice")
	if _, err := svc.Create(ctx, "A", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	got := events.Events()
	if len(got) != 1 || got[0].Actor != "alice" {
		t.Fatalf("events = %+v, want one event with actor alice", got)
	}
}
