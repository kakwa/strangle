package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/store"
)

func TestDBCreateAndListOrgs(t *testing.T) {
	d, _ := openTestDB(t)
	defer d.Close()
	ctx := context.Background()

	created := make(map[string]store.OrgRecord, 2)
	for _, name := range []string{"alpha", "beta"} {
		rec, err := d.CreateOrg(ctx, name)
		if err != nil {
			t.Fatalf("CreateOrg(%q) error: %v", name, err)
		}
		if rec.ID == 0 || rec.Name != name {
			t.Errorf("CreateOrg(%q) = %+v", name, rec)
		}
		created[name] = rec
	}
	if created["alpha"].ID == created["beta"].ID {
		t.Errorf("two orgs share ID %d", created["alpha"].ID)
	}
	if _, err := d.CreateOrg(ctx, "alpha"); err == nil {
		t.Error("CreateOrg() with a duplicate name must fail")
	}

	orgs, err := d.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("ListOrgs() error: %v", err)
	}
	names := make([]string, 0, len(orgs))
	byName := make(map[string]store.OrgRecord, len(orgs))
	for _, org := range orgs {
		names = append(names, org.Name)
		byName[org.Name] = org
	}
	// store.Open seeds the default org; ListOrgs orders by name.
	if got, want := strings.Join(names, ","), "alpha,beta,"+cfg.DefaultOrg; got != want {
		t.Errorf("ListOrgs() names = %q, want %q", got, want)
	}
	for _, name := range []string{"alpha", "beta"} {
		if rec := byName[name]; rec.ID != created[name].ID {
			t.Errorf("ListOrgs() %s = %+v, want ID %d", name, rec, created[name].ID)
		}
	}
}
