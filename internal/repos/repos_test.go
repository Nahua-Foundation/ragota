package repos

import (
	"os"
	"path/filepath"
	"testing"
)

func mkDir(t *testing.T, root, rel string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return p
}

func touchGit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git in %s: %v", dir, err)
	}
}

func TestDiscoverSingleRepo(t *testing.T) {
	root := t.TempDir()
	touchGit(t, root)
	mkDir(t, root, "pkg")

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ожидалось 1 репо, получили %d: %+v", len(got), got)
	}
	if got[0].Name != filepath.Base(root) {
		t.Errorf("name=%q want %q", got[0].Name, filepath.Base(root))
	}
	if !got[0].HasGit {
		t.Errorf("HasGit=false, ожидалось true")
	}
}

func TestDiscoverMultiRepoWorkspace(t *testing.T) {
	root := t.TempDir()
	mkDir(t, root, "alpha")
	touchGit(t, filepath.Join(root, "alpha"))
	mkDir(t, root, "beta")
	touchGit(t, filepath.Join(root, "beta"))
	// «прицепленная» поддиректория без .git — тоже репа.
	mkDir(t, root, "docs")
	// Скрытая директория — игнорируется.
	mkDir(t, root, ".cache")

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	names := map[string]Repo{}
	for _, r := range got {
		names[r.Name] = r
	}
	for _, want := range []string{"alpha", "beta", "docs"} {
		if _, ok := names[want]; !ok {
			t.Errorf("ожидалось репо %q в выдаче, получили %+v", want, got)
		}
	}
	if _, ok := names[".cache"]; ok {
		t.Errorf("скрытая директория .cache не должна попадать в репо")
	}
	if !names["alpha"].HasGit || !names["beta"].HasGit {
		t.Errorf("alpha/beta должны быть HasGit=true: %+v", got)
	}
	if names["docs"].HasGit {
		t.Errorf("docs не должна быть HasGit=true: %+v", names["docs"])
	}
}

func TestDiscoverEmptyDir(t *testing.T) {
	root := t.TempDir()
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].HasGit {
		t.Errorf("пустая папка должна давать 1 fallback-репо без .git, got %+v", got)
	}
}

func TestResolverFor(t *testing.T) {
	root := t.TempDir()
	a := mkDir(t, root, "alpha")
	b := mkDir(t, root, "beta")
	rsv := NewResolver([]Repo{{Name: "alpha", Path: a}, {Name: "beta", Path: b}})

	cases := []struct {
		path string
		want string
	}{
		{filepath.Join(a, "x.go"), "alpha"},
		{filepath.Join(b, "sub", "y.go"), "beta"},
		{filepath.Join(root, "outside.go"), ""},
		{a, "alpha"},
	}
	for _, tc := range cases {
		if got := rsv.For(tc.path); got != tc.want {
			t.Errorf("For(%q) = %q want %q", tc.path, got, tc.want)
		}
	}
}

func TestSignatureStable(t *testing.T) {
	a := []Repo{{Name: "x", Path: "/p/x"}, {Name: "y", Path: "/p/y"}}
	b := []Repo{{Name: "y", Path: "/p/y"}, {Name: "x", Path: "/p/x"}}
	if Signature(a) != Signature(b) {
		t.Errorf("Signature должен быть инвариантен к порядку: %s vs %s", Signature(a), Signature(b))
	}
	c := []Repo{{Name: "x", Path: "/p/x"}}
	if Signature(a) == Signature(c) {
		t.Errorf("Signature должен меняться при смене состава")
	}
}
