package e2e_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/testutil"
)

// writeRepo lays out a throwaway repository and returns its path.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestManifestIgnoreReachesTheWalk indexes a repository whose own .ragota.yaml
// excludes a directory, and asserts through the API that the exclusion took
// effect. The merge itself is unit-tested; what this covers is the wiring —
// that the per-repo pattern list actually reaches the file walk, which a
// compiler check alone cannot show.
func TestManifestIgnoreReachesTheWalk(t *testing.T) {
	srv, _ := testutil.SetupServer(t)
	client := &http.Client{}

	repoPath := writeRepo(t, map[string]string{
		".ragota.yaml": "ignore:\n  - \"**/generated/**\"\n",
		"api/handler.go": `package api

func HandwrittenHandler() string { return "kept" }
`,
		"internal/generated/stub.go": `package generated

func GeneratedStub() string { return "excluded" }
`,
		// Same directory name nested deeper: the "**/dir/**" form has to reach
		// it, which the "dir/**" form the pattern replaced would not.
		"services/orders/generated/client.go": `package generated

func GeneratedClient() string { return "excluded" }
`,
	})

	id := addRepo(t, client, srv.URL, "manifest-ignore", repoPath)
	indexRepo(t, client, srv.URL, id)
	waitIdle(t, client, srv.URL, id)

	symbol := func(name string) int {
		t.Helper()
		return postJSON[symbolResponse](t, client, srv.URL+"/api/v1/nav/symbol", map[string]any{
			"repo_id": id, "name": name, "limit": 10,
		}).Total
	}

	if got := symbol("HandwrittenHandler"); got == 0 {
		t.Error("HandwrittenHandler missing — the repository indexed nothing at all")
	}
	for _, name := range []string{"GeneratedStub", "GeneratedClient"} {
		if got := symbol(name); got != 0 {
			t.Errorf("%s indexed %d time(s); .ragota.yaml excludes it", name, got)
		}
	}
}

// Without the manifest the very same tree indexes in full — otherwise the test
// above would pass on a build that indexes nothing.
func TestWithoutManifestGeneratedIsIndexed(t *testing.T) {
	srv, _ := testutil.SetupServer(t)
	client := &http.Client{}

	repoPath := writeRepo(t, map[string]string{
		"api/handler.go": `package api

func HandwrittenHandler() string { return "kept" }
`,
		"internal/generated/stub.go": `package generated

func GeneratedStub() string { return "kept too" }
`,
	})

	id := addRepo(t, client, srv.URL, "no-manifest", repoPath)
	indexRepo(t, client, srv.URL, id)
	waitIdle(t, client, srv.URL, id)

	res := postJSON[symbolResponse](t, client, srv.URL+"/api/v1/nav/symbol", map[string]any{
		"repo_id": id, "name": "GeneratedStub", "limit": 10,
	})
	if res.Total == 0 {
		t.Error("GeneratedStub missing without a manifest — the exclusion test proves nothing")
	}
}
