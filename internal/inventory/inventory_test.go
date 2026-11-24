package inventory

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadMergesMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "one.yml", `
all:
  vars:
    ansible_user: deploy
  children:
    webservers:
      hosts:
        web1:
          ansible_host: 10.0.0.1
`)
	writeFile(t, dir, "two.yml", `
all:
  children:
    dbs:
      hosts:
        db1:
          ansible_host: 10.0.0.2
          ansible_user: dba
        db2:
          ansible_host: 10.0.0.3
    webservers:
      hosts:
        web2:
          ansible_host: 10.0.0.4
`)

	inv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(inv.Hosts) != 4 {
		t.Fatalf("got %d hosts, want 4: %+v", len(inv.Hosts), inv.Hosts)
	}

	byName := map[string]Host{}
	for _, h := range inv.Hosts {
		byName[h.Name] = h
	}

	if got := byName["web1"].User; got != "deploy" {
		t.Errorf("web1.User = %q, want deploy (global default)", got)
	}
	if got := byName["db1"].User; got != "dba" {
		t.Errorf("db1.User = %q, want dba (host override)", got)
	}
	if got := byName["web1"].IP; got != "10.0.0.1" {
		t.Errorf("web1.IP = %q", got)
	}
	if got := byName["web2"].Groups; !reflect.DeepEqual(got, []string{"webservers"}) {
		t.Errorf("web2.Groups = %v", got)
	}
	if got := byName["db2"].Groups; !reflect.DeepEqual(got, []string{"dbs"}) {
		t.Errorf("db2.Groups = %v", got)
	}
}

func TestMembers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "inv.yml", `
all:
  children:
    parent:
      children:
        child:
          hosts:
            h1:
            h2:
        other:
          hosts:
            h3:
    solo:
      hosts:
        h4:
`)

	inv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got := inv.Members("child"); !reflect.DeepEqual(got, []string{"h1", "h2"}) {
		t.Errorf("Members(child) = %v", got)
	}
	if got := inv.Members("parent"); !reflect.DeepEqual(got, []string{"h1", "h2", "h3"}) {
		t.Errorf("Members(parent) = %v", got)
	}
	if got := inv.Members("h4"); !reflect.DeepEqual(got, []string{"h4"}) {
		t.Errorf("Members(h4) = %v", got)
	}
	if got := inv.Members("missing"); len(got) != 0 {
		t.Errorf("Members(missing) = %v, want empty", got)
	}
	if got := inv.Members("all"); !reflect.DeepEqual(got, []string{"h1", "h2", "h3", "h4"}) {
		t.Errorf("Members(all) = %v", got)
	}
}

func TestLoadIgnoresNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "inv.yml", "all:\n  hosts:\n    h1:\n")
	writeFile(t, dir, "notes.txt", "ignore me")

	inv, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(inv.Hosts) != 1 {
		t.Errorf("got %d hosts, want 1", len(inv.Hosts))
	}
}
