package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNoManifestIsNotAnError(t *testing.T) {
	m, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if m != nil {
		t.Fatalf("manifest = %+v, want nil", m)
	}
}

// The sections are independent: a repository that only wants to exclude a
// directory must not have to describe its services to do it, and vice versa.
func TestSectionsAreIndependent(t *testing.T) {
	t.Run("ignore only", func(t *testing.T) {
		m, err := Load(write(t, ".ragota.yaml", "ignore:\n  - \"**/generated/**\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Services) != 0 {
			t.Errorf("services = %+v, want none", m.Services)
		}
		if want := []string{"**/generated/**"}; !reflect.DeepEqual(m.Ignore, want) {
			t.Errorf("ignore = %q, want %q", m.Ignore, want)
		}
	})

	t.Run("services only", func(t *testing.T) {
		m, err := Load(write(t, ".ragota.yaml", "services:\n  - name: orders\n    root: services/orders\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Ignore) != 0 {
			t.Errorf("ignore = %q, want none", m.Ignore)
		}
		want := []Service{{Name: "orders", Root: "services/orders"}}
		if !reflect.DeepEqual(m.Services, want) {
			t.Errorf("services = %+v, want %+v", m.Services, want)
		}
	})
}

func TestYmlExtensionIsAccepted(t *testing.T) {
	m, err := Load(write(t, ".ragota.yml", "ignore: [\"docs/**\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.File != ".ragota.yml" {
		t.Errorf("File = %q, want .ragota.yml", m.File)
	}
}

// A typo must not read as "this repository excludes nothing" — that silently
// indexes the tree the manifest exists to keep out.
func TestMalformedManifestIsAnError(t *testing.T) {
	_, err := Load(write(t, ".ragota.yaml", "ignore:\n  - \"**/x/**\"\n   bad indentation: [\n"))
	if err == nil {
		t.Fatal("want an error for a malformed manifest")
	}
}

func TestNormalization(t *testing.T) {
	m, err := Load(write(t, ".ragota.yaml", `
services:
  - name: "  orders  "
    root: "/services/orders/"
  - name: ""
    root: nameless
  - name: root-service
ignore:
  - "  **/generated/**  "
  - ""
  - "   "
`))
	if err != nil {
		t.Fatal(err)
	}

	wantSvcs := []Service{
		{Name: "orders", Root: "services/orders"},
		{Name: "root-service", Root: ""},
	}
	if !reflect.DeepEqual(m.Services, wantSvcs) {
		t.Errorf("services = %+v, want %+v", m.Services, wantSvcs)
	}
	if want := []string{"**/generated/**"}; !reflect.DeepEqual(m.Ignore, want) {
		t.Errorf("ignore = %q, want %q", m.Ignore, want)
	}
}

func TestEmptyManifestLoadsAsEmpty(t *testing.T) {
	m, err := Load(write(t, ".ragota.yaml", "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("an empty but present manifest should still load")
	}
	if len(m.Services) != 0 || len(m.Ignore) != 0 {
		t.Errorf("manifest = %+v, want both sections empty", m)
	}
}
