package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kakwa/strangle/internal/auth"
)

func TestUserStoreCRUD(t *testing.T) {
	d, _ := openTestDB(t)
	defer d.Close()

	ctx := context.Background()
	users := d.Users()

	if n, err := users.Count(ctx); err != nil || n != 0 {
		t.Fatalf("Count() = %d, %v; want 0, nil", n, err)
	}

	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	if err := users.CreateUser(ctx, "alice", hash); err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
	if err := users.CreateUser(ctx, "alice", hash); err == nil {
		t.Error("CreateUser() on an existing name must fail")
	}
	if err := users.CreateUser(ctx, "bob", hash); err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
	if err := users.CreateUser(ctx, "  ", hash); err == nil {
		t.Error("CreateUser() with a blank name must fail")
	}

	u, err := users.GetUser(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUser() error: %v", err)
	}
	if u.Name != "alice" || u.Hash != hash {
		t.Errorf("GetUser() = %+v", u)
	}
	if !auth.VerifyPassword(u.Hash, "s3cret") {
		t.Error("stored hash does not verify against the password")
	}
	if _, err := users.GetUser(ctx, "nobody"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetUser() missing: err = %v, want auth.ErrNotFound", err)
	}

	usersList, err := users.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() error: %v", err)
	}
	if len(usersList) != 2 || usersList[0].Name != "alice" || usersList[1].Name != "bob" {
		t.Errorf("ListUsers() = %+v, want alice then bob", usersList)
	}
	if n, err := users.Count(ctx); err != nil || n != 2 {
		t.Errorf("Count() = %d, %v; want 2", n, err)
	}

	if err := users.RemoveUser(ctx, "alice"); err != nil {
		t.Fatalf("RemoveUser() error: %v", err)
	}
	if _, err := users.GetUser(ctx, "alice"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetUser() after removal: err = %v, want auth.ErrNotFound", err)
	}
	if err := users.RemoveUser(ctx, "alice"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("RemoveUser() twice: err = %v, want auth.ErrNotFound", err)
	}
	if n, err := users.Count(ctx); err != nil || n != 1 {
		t.Errorf("Count() after removal = %d, %v; want 1", n, err)
	}

	// Leave the table empty so a server pointed at the test database does
	// not boot with a stray account (which would enable authentication).
	if err := users.RemoveUser(ctx, "bob"); err != nil {
		t.Fatalf("RemoveUser() cleanup error: %v", err)
	}
}
