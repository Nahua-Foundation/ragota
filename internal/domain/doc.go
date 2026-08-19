// Package domain holds the entity types every layer speaks: repositories,
// AST units, edges, index jobs and the query shapes over them.
//
// It is deliberately the only package with this vocabulary, and the only
// package that owns nothing else. The rule that keeps it that way: domain
// imports nothing from this module. It is data, not behavior — every struct
// here is a plain value with JSON tags, and every function that acts on them
// lives in the package that owns the action. If a type needs an import, it
// does not belong here.
package domain
