package ast

import (
	"context"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// A store with no query language and no mapped entities: hashicorp/go-memdb
// takes the table name as the first argument of every transaction method, and
// that argument is the only thing a data-access edge can be keyed on. Consul's
// state store is one of these, with 437 accesses none of which was recognized.

func TestGoMemdbTableAccess(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		writes []string
		reads  []string
		absent []string
	}{
		{
			name: "table named by a literal",
			src: `package state

import "github.com/hashicorp/go-memdb"

func (s *Store) KVSSet(tx *memdb.Txn, e *structs.DirEntry) error {
	existing, err := tx.First("kvs", "id", e.Key)
	if err != nil {
		return err
	}
	return tx.Insert("kvs", e)
}
`,
			writes: []string{"db:kvs"},
			reads:  []string{"db:kvs"},
		},
		{
			name: "table named by a constant declared in the same file",
			src: `package state

import "github.com/hashicorp/go-memdb"

const tableSessions = "sessions"

func sessionsByNode(tx *memdb.Txn, node string) (memdb.ResultIterator, error) {
	if err := tx.DeleteAll(tableSessions, indexNode, node); err != nil {
		return nil, err
	}
	return tx.Get(tableSessions, indexNode, node)
}
`,
			writes: []string{"db:sessions"},
			reads:  []string{"db:sessions"},
		},
		{
			name: "every read method of the transaction API",
			src: `package state

import "github.com/hashicorp/go-memdb"

func lookups(tx *memdb.Txn) {
	tx.FirstWatch("nodes", "id")
	tx.LastWatch("nodes", "id")
	tx.GetReverse("nodes", "id")
	tx.LongestPrefix("nodes", "id_prefix")
	tx.LowerBound("nodes", "id")
}
`,
			reads: []string{"db:nodes"},
		},
		{
			name: "a mock's ret.Get(0) in a file that happens to import memdb is not a table read",
			src: `package reportingmock

import "github.com/hashicorp/go-memdb"

func (_m *StateDelegate) Get() memdb.Txn {
	ret := _m.Called()
	return ret.Get(0).(memdb.Txn)
}
`,
			absent: []string{"db:0"},
		},
		{
			name: "a transaction in a file that does not import memdb names no table",
			src: `package sqlstore

func save(tx *sql.Tx, o *Order) error {
	_, err := tx.Insert("orders", o)
	return err
}
`,
			absent: []string{"db:orders"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, "go", "agent/consul/state/store.go", tt.src)
			for _, want := range tt.writes {
				if findEdge(edges, storage.EdgeWritesTo, want) == nil {
					t.Errorf("writes_to %q missing from %v", want, edgeNamesOfKind(edges, storage.EdgeWritesTo))
				}
			}
			for _, want := range tt.reads {
				if findEdge(edges, storage.EdgeReadsFrom, want) == nil {
					t.Errorf("reads_from %q missing from %v", want, edgeNamesOfKind(edges, storage.EdgeReadsFrom))
				}
			}
			for _, not := range tt.absent {
				for _, kind := range []string{storage.EdgeWritesTo, storage.EdgeReadsFrom} {
					if findEdge(edges, kind, not) != nil {
						t.Errorf("%s %q was emitted and should not be", kind, not)
					}
				}
			}
		})
	}
}

// The receiver name is the only thing separating a memdb transaction from the
// other things a state-store file calls Get on.
func TestGoTxnReceiver(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"tx", true},
		{"txn", true},
		{"tx2", true},
		{"s.tx", true},
		{"readTxn", true},
		{"wTx", true},
		{"ctx", false},
		{"ret", false},
		{"params", false},
		{"updater", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := goTxnReceiver(tt.in); got != tt.want {
			t.Errorf("goTxnReceiver(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// The table names live in a schema file and the queries in the files beside
// it, which is where 368 of Consul's 437 accesses take theirs from.
func TestPackageScopedTableNames(t *testing.T) {
	schema := `package state

import "github.com/hashicorp/go-memdb"

const (
	tableNodes    = "nodes"
	tableServices = "services"
	indexID       = "id"
)

func nodesTableSchema() *memdb.TableSchema { return &memdb.TableSchema{Name: tableNodes} }
`
	catalog := `package state

import "github.com/hashicorp/go-memdb"

func (s *Store) EnsureNode(tx *memdb.Txn, node *structs.Node) error {
	existing, err := tx.First(tableNodes, indexID, node.Node)
	if err != nil {
		return err
	}
	_ = existing
	return tx.Insert(tableNodes, node)
}

func (s *Store) Services(tx *memdb.Txn) (memdb.ResultIterator, error) {
	return tx.Get(tableServices, indexID)
}
`
	// Same constant name, another package: the join is by identifier, so it
	// must not cross a directory.
	other := `package inmem

import "github.com/hashicorp/go-memdb"

func list(tx *memdb.Txn) (memdb.ResultIterator, error) {
	return tx.Get(tableNodes, indexID)
}
`
	files := []*indexing.FileToIndex{
		{Path: "agent/consul/state/catalog_schema.go", Language: "go", Content: []byte(schema)},
		{Path: "agent/consul/state/catalog.go", Language: "go", Content: []byte(catalog)},
		{Path: "internal/storage/inmem/store.go", Language: "go", Content: []byte(other)},
	}

	mem := &memStorage{}
	idx := New(&Config{Storage: mem})
	RegisterDefaultParsers(idx)
	result, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r", Files: files})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	got := map[string]string{}
	for _, e := range mem.edges {
		if e.Kind != storage.EdgeReadsFrom && e.Kind != storage.EdgeWritesTo {
			continue
		}
		got[e.Kind+" "+e.DstName] = e.FilePath
		if e.FilePath == "internal/storage/inmem/store.go" {
			t.Errorf("%s %q resolved from another package's constant", e.Kind, e.DstName)
		}
	}
	for _, want := range []string{"reads_from db:nodes", "writes_to db:nodes", "reads_from db:services"} {
		if got[want] != "agent/consul/state/catalog.go" {
			t.Errorf("edge %q attributed to %q, want the querying file; edges: %v", want, got[want], got)
		}
	}

	// The four sites are candidates; the three the package could name are
	// edges, and the fourth is the gap the report exists to show.
	want := storage.CoverageCounts{Candidates: 4, Edges: 3}
	if got := result.ContractCoverage()[storage.ContractKindDB]; got != want {
		t.Errorf("db coverage = %+v, want %+v", got, want)
	}
}

// A file offers the rest of its package the names a table could have, not
// every string it declares.
func TestPublishedConstsAreTableNames(t *testing.T) {
	src := `package state

import "github.com/hashicorp/go-memdb"

const (
	tableNodes = "nodes"
	tableACLs  = "acl-tokens"
	errMsg     = "failed to insert node into the state store"
	indexURL   = "https://example.com/index"
)

func f(tx *memdb.Txn) { tx.Get(tableNodes, indexID) }
`
	facts := parseFactsOrFail(t, "go", "agent/consul/state/schema.go", src)
	for _, name := range []string{"tableNodes", "tableACLs"} {
		if _, ok := facts.Consts[name]; !ok {
			t.Errorf("%s not published to the package; consts: %v", name, facts.Consts)
		}
	}
	for _, name := range []string{"errMsg", "indexURL"} {
		if v, ok := facts.Consts[name]; ok {
			t.Errorf("%s = %q published as a table name", name, v)
		}
	}
}

// A file with no store in it publishes nothing at all: the package table is
// paid for per file, and every Go file declares string constants.
func TestNonStoreFilePublishesNoConsts(t *testing.T) {
	src := `package agent

const defaultAddr = "127.0.0.1"

func f() string { return defaultAddr }
`
	facts := parseFactsOrFail(t, "go", "agent/agent.go", src)
	if len(facts.Consts) != 0 {
		t.Errorf("consts = %v, want none", facts.Consts)
	}
}

// A literal that opens with a SQL verb and is an English sentence is not
// database access. Consul's CLI test cases are named "update a role that does
// not exist", and 152 of those were counted as tables the indexer had missed.
func TestSQLStatementStart(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"INSERT INTO ORDERS (ID) VALUES ($1)", true},
		{"UPDATE ORDERS SET STATUS = $1 WHERE ID = $2", true},
		{"UPDATE ORDERS\n\tSET STATUS = $1", true},
		{"DELETE FROM ORDERS WHERE ID = $1", true},
		{"SELECT * FROM ORDERS", true},
		{"  (SELECT ID FROM ORDERS)", true},
		{"WITH RECENT AS (SELECT * FROM ORDERS) SELECT * FROM RECENT", true},
		{"UPDATE A ROLE THAT DOES NOT EXIST", false},
		{"UPDATE WITH POLICY BY NAME", false},
		{"DELETE WITH -ID", false},
		{"SELECT A VALUE", false},
		{"WITH APPEND TEMPLATED POLICIES", false},
		{"INSERT THE RECORD", false},
	}
	for _, tt := range tests {
		if got := sqlStatementStart(tt.in); got != tt.want {
			t.Errorf("sqlStatementStart(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// The narrowing is about what is counted, not about what is found: a statement
// the regexes can still name a table in keeps its edge either way.
func TestSQLSentenceIsNotADatabaseCandidate(t *testing.T) {
	src := `package update

func TestRoleUpdateCommand(t *testing.T) {
	t.Run("update a role that does not exist", func(t *testing.T) {})
	db.Exec("INSERT INTO orders (id) VALUES ($1)", id)
}
`
	facts := parseFactsOrFail(t, "go", "command/acl/role/update/role_update_test.go", src)
	want := storage.CoverageCounts{Candidates: 1, Edges: 1}
	if got := facts.Coverage[storage.ContractKindDB]; got != want {
		t.Errorf("db coverage = %+v, want %+v", got, want)
	}
}
