package ast

import (
	"regexp"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// SQLParser parses SQL migration files into db_table / db_column units.
// It understands CREATE TABLE and ALTER TABLE ... ADD COLUMN statements —
// enough to model the schema that application code writes into.
type SQLParser struct{}

// NewSQLParser creates a SQL migrations parser.
func NewSQLParser() *SQLParser { return &SQLParser{} }

// Language returns the language name.
func (p *SQLParser) Language() string { return "sql" }

var (
	reCreateTable = regexp.MustCompile(`(?i)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["'` + "`" + `]?([A-Za-z_][A-Za-z0-9_.]*)`)
	reAlterAdd    = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\s+["'` + "`" + `]?([A-Za-z_][A-Za-z0-9_.]*)["'` + "`" + `]?\s+ADD\s+(?:COLUMN\s+)?["'` + "`" + `]?([A-Za-z_][A-Za-z0-9_]*)["'` + "`" + `]?\s*(\S*)`)
)

// sqlConstraintKeywords start lines inside CREATE TABLE that are not columns.
var sqlConstraintKeywords = map[string]bool{
	"PRIMARY": true, "FOREIGN": true, "UNIQUE": true, "CONSTRAINT": true,
	"KEY": true, "INDEX": true, "CHECK": true, "LIKE": true,
}

// Parse extracts schema units from a SQL file.
func (p *SQLParser) Parse(filePath, content string) ([]*storage.ASTUnit, []*storage.Edge, error) {
	var units []*storage.ASTUnit

	addUnit := func(lineNo int, kind, name, qualified, signature string) {
		units = append(units, &storage.ASTUnit{
			Kind:      kind,
			Name:      name,
			Qualified: qualified,
			Signature: signature,
			StartLine: lineNo + 1,
			EndLine:   lineNo + 1,
			Hash:      hashString(qualified + signature),
		})
	}

	lines := strings.Split(content, "\n")
	currentTable := ""
	tableStart := 0
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}

		if m := reCreateTable.FindStringSubmatch(line); m != nil {
			currentTable = sqlTableName(m[1])
			tableStart = i
			addUnit(i, storage.KindDBTable, currentTable, "db:"+currentTable, "")
			continue
		}
		if m := reAlterAdd.FindStringSubmatch(line); m != nil {
			table := sqlTableName(m[1])
			col := strings.ToLower(m[2])
			addUnit(i, storage.KindDBColumn, col, "db:"+table+"."+col, m[3])
			continue
		}

		if currentTable == "" {
			continue
		}
		if strings.HasPrefix(line, ")") {
			// close CREATE TABLE block; extend the table unit's span
			for _, u := range units {
				if u.Kind == storage.KindDBTable && u.Name == currentTable && u.StartLine == tableStart+1 {
					u.EndLine = i + 1
				}
			}
			currentTable = ""
			continue
		}
		// Column line: `user_id TEXT NOT NULL,`
		fields := strings.Fields(strings.TrimSuffix(line, ","))
		if len(fields) < 1 {
			continue
		}
		name := strings.Trim(fields[0], "\"'`")
		if sqlConstraintKeywords[strings.ToUpper(name)] || strings.HasPrefix(name, "(") {
			continue
		}
		if !isIdentLike(name) {
			continue
		}
		colType := ""
		if len(fields) > 1 {
			colType = fields[1]
		}
		addUnit(i, storage.KindDBColumn, strings.ToLower(name), "db:"+currentTable+"."+strings.ToLower(name), colType)
	}

	return units, nil, nil
}

// --- Detection of SQL statements inside application code ---

var (
	reInsert = regexp.MustCompile(`(?i)INSERT\s+INTO\s+["'` + "`" + `]?([A-Za-z_][A-Za-z0-9_.]*)["'` + "`" + `]?\s*(?:\(([^)]*)\))?`)
	reUpdate = regexp.MustCompile(`(?i)UPDATE\s+["'` + "`" + `]?([A-Za-z_][A-Za-z0-9_.]*)["'` + "`" + `]?\s+SET\s+([^;]*)`)
	reDelete = regexp.MustCompile(`(?i)DELETE\s+FROM\s+["'` + "`" + `]?([A-Za-z_][A-Za-z0-9_.]*)`)
	reSelect = regexp.MustCompile(`(?i)(?:FROM|JOIN)\s+["'` + "`" + `]?([A-Za-z_][A-Za-z0-9_.]*)`)
)

// sqlEdgesFromArgs inspects string-literal call arguments for SQL statements
// and emits writes_to / reads_from edges. Column names of INSERT/UPDATE go
// into Meta.Fields so the tracer can name the sink column.
func sqlEdgesFromArgs(fc *fileCtx, src int, line int, args []string) bool {
	if src < 0 {
		return false
	}
	emitted := false
	candidate := false
	// A statement whose table cannot be parsed is still a database access the
	// coverage counters must see, so the candidate is recorded once per call
	// site regardless of how the parse goes.
	defer func() {
		if candidate || emitted {
			fc.contractSite(storage.ContractKindDB, emitted)
		}
	}()
	for _, arg := range args {
		lit, ok := unquote(arg)
		if !ok {
			continue
		}
		upper := strings.ToUpper(lit)
		if !strings.Contains(upper, "INSERT") && !strings.Contains(upper, "UPDATE") &&
			!strings.Contains(upper, "DELETE") && !strings.Contains(upper, "SELECT") {
			continue
		}
		// The scan above is a cheap pre-filter and matches ordinary words
		// ("indices.deletes"). Only a literal that opens with a statement verb
		// counts as a database access the coverage report should account for;
		// an edge, wherever in the literal it was found, always does.
		candidate = candidate || sqlStatementStart(upper)

		if m := reInsert.FindStringSubmatch(lit); m != nil {
			table := sqlTableName(m[1])
			fields := map[string]string{}
			for _, col := range strings.Split(m[2], ",") {
				col = strings.ToLower(strings.TrimSpace(strings.Trim(col, "\"'`")))
				if col != "" && isIdentLike(col) {
					fields[col] = col
				}
			}
			if len(fields) == 0 {
				fields = nil
			}
			fc.addEdge(src, storage.EdgeWritesTo, contract.DB(table), line, contract.ConfHigh,
				&storage.EdgeMeta{Args: args, Fields: fields})
			emitted = true
			continue
		}
		if m := reUpdate.FindStringSubmatch(lit); m != nil {
			table := sqlTableName(m[1])
			fields := map[string]string{}
			for _, assign := range strings.Split(m[2], ",") {
				if i := strings.Index(assign, "="); i > 0 {
					col := strings.ToLower(strings.TrimSpace(strings.Trim(assign[:i], "\"'`")))
					if isIdentLike(col) {
						fields[col] = col
					}
				}
			}
			if len(fields) == 0 {
				fields = nil
			}
			fc.addEdge(src, storage.EdgeWritesTo, contract.DB(table), line, contract.ConfHigh,
				&storage.EdgeMeta{Args: args, Fields: fields})
			emitted = true
			continue
		}
		if m := reDelete.FindStringSubmatch(lit); m != nil {
			table := sqlTableName(m[1])
			fc.addEdge(src, storage.EdgeWritesTo, contract.DB(table), line, contract.ConfHigh, &storage.EdgeMeta{Args: args})
			emitted = true
			continue
		}
		if strings.Contains(upper, "SELECT") {
			for _, m := range reSelect.FindAllStringSubmatch(lit, -1) {
				table := sqlTableName(m[1])
				fc.addEdge(src, storage.EdgeReadsFrom, contract.DB(table), line, contract.ConfHigh, &storage.EdgeMeta{Args: args})
				emitted = true
			}
		}
	}
	return emitted
}

// sqlStatements pairs each statement verb with the keyword that has to follow
// it for the literal to be a statement at all. The verb alone is also how an
// English sentence starts: Consul names its CLI test cases "update a role that
// does not exist" and "delete with -id", and 152 of those were counted as
// database access the indexer had failed to resolve.
var sqlStatements = []struct{ verb, needs string }{
	{"INSERT ", " INTO "},
	{"UPDATE ", " SET "},
	{"DELETE ", " FROM "},
	{"SELECT ", " FROM "},
	{"WITH ", " AS ("},
}

// sqlStatementStart reports whether an upper-cased literal is a SQL statement:
// it opens with a statement verb, allowing for a leading CTE paren, and carries
// that verb's obligatory keyword. Whitespace is normalized first, because a
// statement long enough to be worth writing is written across lines.
func sqlStatementStart(upper string) bool {
	s := strings.Join(strings.Fields(strings.TrimLeft(upper, " \t\r\n(")), " ")
	for _, stmt := range sqlStatements {
		if strings.HasPrefix(s, stmt.verb) {
			return strings.Contains(s, stmt.needs)
		}
	}
	return false
}

// sqlTableName normalizes a table reference for a db: key. The schema
// qualifier is kept: two services owning a "users" table in different schemas
// are different tables, and collapsing them makes the linker attribute writes
// to an arbitrary one.
func sqlTableName(raw string) string {
	parts := strings.Split(raw, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(strings.Trim(p, "\"'`[]")))
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ".")
}
