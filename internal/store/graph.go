// Package store реализует SQLite-хранилище для индексатора: файлы,
// AST units, рёбра графа кода и метаданные эмбеддингов.
//
// Этот файл содержит доменные типы графа (ASTUnit, Edge). Реализация
// декомпозирована по доменам (все файлы — package store):
//
//   - graph.go       — доменные типы графа (ASTUnit, Edge) + docstring пакета.
//   - embed_meta.go  — EmbedMeta + Get/SetEmbedMeta.
//   - ast_units.go   — CRUD AST units: Replace/List/Get/Find/UpdateParents/
//     ChildrenOf, helpers scanASTUnit + astUnitColumns.
//   - edges.go       — CRUD рёбер + отложенный резолв: Replace/Resolve/
//     EdgesFrom/EdgesTo/EdgesByDstName, helpers scanEdges + edgeColumns.
//   - neighbors.go   — BFS-обход графа: ExpandNeighbors + edgesAround.
//   - sqlite.go      — открытие БД, миграции, общие операции (files etc).
package store

import "database/sql"

// ASTUnit — самостоятельная AST-единица: function/method/class/interface/module/...
// Используется для hybrid retrieval, parent-child навигации и graph expansion.
type ASTUnit struct {
	ID int64
	// Repo — имя репозитория, к которому относится юнит. В multi-repo
	// workspace используется для разделения графа: рёбра графа никогда
	// не пересекают границу репы (см. ResolvePendingEdges и
	// EdgesByDstNameForLangRepo).
	Repo      string
	FilePath  string
	Language  string
	Kind      string
	Name      string
	Qualified string
	ParentID  sql.NullInt64
	StartLine int
	EndLine   int
	StartByte int
	EndByte   int
	Signature string
	Doc       string
	Hash      string
}

// Edge — направленная связь между AST units.
//
// Kind:
//   - "call"        : src вызывает dst
//   - "import"      : src импортирует dst (модуль/файл)
//   - "implements"  : src реализует интерфейс dst
//   - "extends"     : src наследует от dst
//   - "reference"   : src ссылается на dst (поле/переменная/тип)
//   - "contains"    : src содержит dst (parent-child, дублирует ParentID, опционально)
type Edge struct {
	ID    int64 `json:"id"`
	SrcID int64 `json:"src_id"`
	DstID int64 `json:"dst_id"` // 0 если ещё не разрешено — тогда используется DstName
	// Repo — имя репозитория источника ребра. Резолв dst допускается
	// только внутри той же репы (см. ResolvePendingEdges).
	Repo     string `json:"repo"`
	Kind     string `json:"kind"`
	DstName  string `json:"dst_name"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
}

// TraverseResult — результат направленного обхода.
type TraverseResult struct {
	Nodes []ASTUnit `json:"nodes"`
	Edges []Edge    `json:"edges"`
}
