package repos

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAt(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(svcs []Service) map[string]Service {
	m := map[string]Service{}
	for _, s := range svcs {
		m[s.Name] = s
	}
	return m
}

func TestRagotaYAMLOverridesEverything(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, ".ragota.yaml", "services:\n  - name: api\n    root: backend\n  - name: ui\n    root: frontend\n")
	writeAt(t, root, "docker-compose.yml", "services:\n  other:\n    build: ./other\n")

	svcs, err := DetectServices(root, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Fatalf("expected 2 services, got %+v", svcs)
	}
	m := names(svcs)
	if m["api"].Root != "backend" || m["api"].DetectedBy != "ragota.yaml" {
		t.Errorf("api = %+v", m["api"])
	}
	if m["ui"].Root != "frontend" {
		t.Errorf("ui = %+v", m["ui"])
	}
}

func TestComposeSkipsImageOnlyServices(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "docker-compose.yml",
		"services:\n  app:\n    build: ./app\n  db:\n    image: postgres:16\n")
	writeAt(t, root, "app/Dockerfile", "FROM scratch\n")

	svcs, err := DetectServices(root, "repo")
	if err != nil {
		t.Fatal(err)
	}
	m := names(svcs)
	if len(svcs) != 1 || m["app"].Root != "app" {
		t.Fatalf("expected only app service, got %+v", svcs)
	}
}

func TestCmdConvention(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "cmd/worker/main.go", "package main\n")
	writeAt(t, root, "cmd/api/main.go", "package main\n")
	writeAt(t, root, "go.mod", "module m\n")

	svcs, err := DetectServices(root, "repo")
	if err != nil {
		t.Fatal(err)
	}
	m := names(svcs)
	if m["worker"].Root != "cmd/worker" || m["api"].Root != "cmd/api" {
		t.Fatalf("expected cmd services, got %+v", svcs)
	}
}

func TestSingleServiceFallback(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "go.mod", "module m\n")
	writeAt(t, root, "main.go", "package main\n")

	svcs, err := DetectServices(root, "myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 1 || svcs[0].Name != "myrepo" || svcs[0].Root != "" {
		t.Fatalf("expected single root service, got %+v", svcs)
	}
}

// TestNestedServicesGetDistinctNames: two services whose directories share a
// basename must not collapse into one node named "api".
func TestNestedServicesGetDistinctNames(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "services/orders/api/pom.xml", "<project/>\n")
	writeAt(t, root, "services/users/api/pom.xml", "<project/>\n")
	writeAt(t, root, "services/orders/worker/pom.xml", "<project/>\n")

	svcs, err := DetectServices(root, "repo")
	if err != nil {
		t.Fatal(err)
	}
	m := names(svcs)
	if len(svcs) != 3 {
		t.Fatalf("expected 3 services, got %+v", svcs)
	}
	if m["orders/api"].Root != "services/orders/api" {
		t.Errorf("orders/api = %+v", m["orders/api"])
	}
	if m["users/api"].Root != "services/users/api" {
		t.Errorf("users/api = %+v", m["users/api"])
	}
	// A basename that is already unique keeps its short name.
	if m["worker"].Root != "services/orders/worker" {
		t.Errorf("worker = %+v", m["worker"])
	}
}

// TestDeepManifestIsDetected: the walk must reach a service manifest five
// directories below the repository root.
func TestDeepManifestIsDetected(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "apps/backend/services/orders/api/pom.xml", "<project/>\n")

	svcs, err := DetectServices(root, "repo")
	if err != nil {
		t.Fatal(err)
	}
	m := names(svcs)
	if len(svcs) != 1 || m["api"].Root != "apps/backend/services/orders/api" {
		t.Fatalf("expected the deep api service, got %+v", svcs)
	}
}

func TestRootTail(t *testing.T) {
	tests := []struct {
		root string
		n    int
		want string
	}{
		{"services/orders/api", 2, "orders/api"},
		{"services/orders/api", 3, "services/orders/api"},
		{"services/orders/api", 9, "services/orders/api"},
		{"api", 2, "api"},
		{"", 2, ""},
	}
	for _, tt := range tests {
		if got := rootTail(tt.root, tt.n); got != tt.want {
			t.Errorf("rootTail(%q, %d) = %q, want %q", tt.root, tt.n, got, tt.want)
		}
	}
}

func TestServiceFor(t *testing.T) {
	svcs := []Service{
		{Name: "gateway", Root: "services/gateway"},
		{Name: "orders", Root: "services/orders"},
	}
	if got := ServiceFor(svcs, "services/gateway/main.go"); got != "gateway" {
		t.Errorf("ServiceFor = %q", got)
	}
	if got := ServiceFor(svcs, "proto/orders.proto"); got != "" {
		t.Errorf("ServiceFor unowned = %q", got)
	}
}

func TestMergeServiceHints(t *testing.T) {
	detected := []Service{
		{Name: "gateway", Root: "services/gateway", Manifest: "docker-compose.yml", DetectedBy: "docker-compose"},
		{Name: "web", Root: "", DetectedBy: "root"},
	}
	hints := []Service{
		{Name: "gw-from-llm", Root: "services/gateway/"}, // root taken -> dropped
		{Name: "payments", Root: "/pay/"},                // new root -> added, normalized
		{Name: "", Root: "x"},                            // nameless -> dropped
		{Name: "web-llm", Root: ""},                      // repo root taken -> dropped
	}

	out := MergeServiceHints(detected, hints)
	if len(out) != 3 {
		t.Fatalf("MergeHints returned %d services, want 3: %+v", len(out), out)
	}
	byName := map[string]Service{}
	for _, s := range out {
		byName[s.Name] = s
	}
	if g, ok := byName["gateway"]; !ok || g.DetectedBy != "docker-compose" || g.Root != "services/gateway" {
		t.Errorf("detected service must win per root, got %+v", g)
	}
	if p, ok := byName["payments"]; !ok || p.DetectedBy != "llm" || p.Root != "pay" {
		t.Errorf("hint service = %+v, want DetectedBy llm, Root pay", p)
	}
	if _, ok := byName["gw-from-llm"]; ok {
		t.Error("hint with an already-claimed root must be dropped")
	}
	if _, ok := byName["web-llm"]; ok {
		t.Error("hint claiming the taken repo root must be dropped")
	}
}

func TestMergeHintsNoHints(t *testing.T) {
	detected := []Service{{Name: "svc", Root: "", DetectedBy: "root"}}
	out := MergeServiceHints(detected, nil)
	if len(out) != 1 || out[0] != detected[0] {
		t.Fatalf("MergeServiceHints(detected, nil) = %+v, want detected unchanged", out)
	}
}
