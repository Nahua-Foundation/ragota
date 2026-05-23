package state

// Unit-тесты для in-memory шины состояния. Покрывают:
//   - NewBus: дефолты (root/started/maxItems) и пустые мапы;
//   - SetIndexer: запись и обновление, UpdatedAt;
//   - AddRecent: упорядочение newest-first и ограничение maxItems;
//   - IncMCPCall: счётчики calls/errors, флаг Running;
//   - SetMCPRunning: создание новой записи и обновление существующей;
//   - SetDocker: UpdatedAt и копирование services;
//   - AddLSPError: ring buffer и trim;
//   - Snapshot: deep-copy (изменение исходных мапов/слайсов не задевает снапшот);
//   - Persist: запись и чтение JSON.
// Внешних зависимостей нет.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewBus_Defaults(t *testing.T) {
	b := NewBus("/repo")
	if b.root != "/repo" {
		t.Errorf("root: %q", b.root)
	}
	if b.started.IsZero() {
		t.Error("started must be set")
	}
	if b.maxItems != 50 {
		t.Errorf("maxItems: %d", b.maxItems)
	}
	if b.indexers == nil || b.mcp == nil {
		t.Error("maps must be initialised")
	}
	snap := b.Snapshot()
	if snap.Root != "/repo" || len(snap.Indexers) != 0 || len(snap.MCP) != 0 || len(snap.Recent) != 0 {
		t.Errorf("snap defaults: %+v", snap)
	}
}

func TestSetIndexer_NewAndUpdate(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) {
		i.Status = "scanning"
		i.FilesTotal = 10
	})
	snap := b.Snapshot()
	v := snap.Indexers["vector"]
	if v.Name != "vector" || v.Status != "scanning" || v.FilesTotal != 10 {
		t.Errorf("indexer: %+v", v)
	}
	if v.UpdatedAt.IsZero() {
		t.Error("UpdatedAt must be set")
	}
	first := v.UpdatedAt

	time.Sleep(2 * time.Millisecond)
	b.SetIndexer("vector", func(i *Indexer) {
		i.Status = "indexing"
		i.FilesIndexed = 5
	})
	v2 := b.Snapshot().Indexers["vector"]
	if v2.Status != "indexing" || v2.FilesIndexed != 5 || v2.FilesTotal != 10 {
		t.Errorf("update merge: %+v", v2)
	}
	if !v2.UpdatedAt.After(first) {
		t.Error("UpdatedAt must advance")
	}
}

func TestAddRecent_OrderAndLimit(t *testing.T) {
	b := NewBus("/r")
	b.maxItems = 3
	for i := 0; i < 5; i++ {
		b.AddRecent(FileEntry{Path: string(rune('a' + i)), Kind: "write"})
	}
	snap := b.Snapshot()
	if len(snap.Recent) != 3 {
		t.Fatalf("len: %d", len(snap.Recent))
	}
	// newest-first: 'e','d','c'
	if snap.Recent[0].Path != "e" || snap.Recent[2].Path != "c" {
		t.Errorf("order: %+v", snap.Recent)
	}
	if snap.Recent[0].IndexedAt.IsZero() {
		t.Error("IndexedAt must be set")
	}
}

func TestIncMCPCall(t *testing.T) {
	b := NewBus("/r")
	b.IncMCPCall("srv", "search", false)
	b.IncMCPCall("srv", "search", false)
	b.IncMCPCall("srv", "definition", true)
	s := b.Snapshot().MCP["srv"]
	if s.Calls["search"] != 2 || s.Calls["definition"] != 1 {
		t.Errorf("calls: %+v", s.Calls)
	}
	if s.Errors != 1 {
		t.Errorf("errors: %d", s.Errors)
	}
	if !s.Running {
		t.Error("Running must be true after call")
	}
}

func TestSetMCPRunning_CreateAndUpdate(t *testing.T) {
	b := NewBus("/r")
	b.SetMCPRunning("srv", true)
	s := b.Snapshot().MCP["srv"]
	if !s.Running || s.Calls == nil {
		t.Errorf("create: %+v", s)
	}
	b.IncMCPCall("srv", "x", false)
	b.SetMCPRunning("srv", false)
	s2 := b.Snapshot().MCP["srv"]
	if s2.Running {
		t.Error("must be off")
	}
	if s2.Calls["x"] != 1 {
		t.Errorf("calls must persist: %+v", s2.Calls)
	}
}

func TestSetDocker(t *testing.T) {
	b := NewBus("/r")
	b.SetDocker(DockerStatus{Running: true, Services: []string{"qdrant", "ollama"}})
	d := b.Snapshot().Docker
	if !d.Running || len(d.Services) != 2 {
		t.Errorf("docker: %+v", d)
	}
	if d.UpdatedAt.IsZero() {
		t.Error("UpdatedAt must be set")
	}
}

func TestAddLSPError_RingBuffer(t *testing.T) {
	b := NewBus("/r")
	b.maxItems = 2
	b.AddLSPError("definition", "/a", 1, 2, errors.New("e1"))
	b.AddLSPError("hover", "/b", 3, 4, errors.New("e2"))
	b.AddLSPError("references", "/c", 5, 6, errors.New("e3"))
	snap := b.Snapshot()
	if len(snap.LSP) != 2 {
		t.Fatalf("len: %d", len(snap.LSP))
	}
	if snap.LSP[0].Method != "references" || snap.LSP[1].Method != "hover" {
		t.Errorf("order: %+v", snap.LSP)
	}
	if snap.LSP[0].Error != "e3" || snap.LSP[0].Line != 5 || snap.LSP[0].Char != 6 {
		t.Errorf("fields: %+v", snap.LSP[0])
	}
}

func TestSnapshot_DeepCopy(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) { i.Status = "idle" })
	b.IncMCPCall("srv", "search", false)
	b.AddRecent(FileEntry{Path: "a"})

	snap := b.Snapshot()
	// мутируем снимок — оригинал не должен пострадать
	snap.Indexers["vector"] = Indexer{Status: "MUTATED"}
	snap.MCP["srv"].Calls["search"] = 999
	snap.Recent[0].Path = "MUTATED"

	snap2 := b.Snapshot()
	if snap2.Indexers["vector"].Status != "idle" {
		t.Errorf("indexers map shared: %+v", snap2.Indexers["vector"])
	}
	if snap2.MCP["srv"].Calls["search"] != 1 {
		t.Errorf("mcp calls map shared: %+v", snap2.MCP["srv"].Calls)
	}
	if snap2.Recent[0].Path != "a" {
		t.Errorf("recent slice shared: %+v", snap2.Recent)
	}
}

func TestPersist_WritesValidJSON(t *testing.T) {
	b := NewBus("/r")
	b.SetIndexer("vector", func(i *Indexer) { i.Status = "indexing"; i.Chunks = 7 })
	b.AddRecent(FileEntry{Path: "x.go", Kind: "write", Chunks: 3})
	b.IncMCPCall("mcp", "tool", false)
	b.SetDocker(DockerStatus{Running: true, Services: []string{"qdrant"}})

	path := filepath.Join(t.TempDir(), "stats.json")
	if err := b.Persist(path); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if snap.Root != "/r" {
		t.Errorf("root: %q", snap.Root)
	}
	if snap.Indexers["vector"].Chunks != 7 {
		t.Errorf("indexer roundtrip: %+v", snap.Indexers["vector"])
	}
	if len(snap.Recent) != 1 || snap.Recent[0].Path != "x.go" {
		t.Errorf("recent roundtrip: %+v", snap.Recent)
	}
	if snap.MCP["mcp"].Calls["tool"] != 1 {
		t.Errorf("mcp roundtrip: %+v", snap.MCP)
	}
	if !snap.Docker.Running || len(snap.Docker.Services) != 1 {
		t.Errorf("docker roundtrip: %+v", snap.Docker)
	}
}

func TestPersist_BadPath(t *testing.T) {
	b := NewBus("/r")
	// директория, в которую нельзя писать — сабпуть от файла
	tmp := t.TempDir()
	blockerFile := filepath.Join(tmp, "f")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Persist(filepath.Join(blockerFile, "stats.json")); err == nil {
		t.Error("expected error writing under non-directory")
	}
}
