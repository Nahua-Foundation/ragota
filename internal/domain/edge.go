package domain

// Edge represents a relationship between AST units.
type Edge struct {
	ID         string  `json:"id"`
	RepoID     string  `json:"repo_id"`
	SrcID      string  `json:"src_id"`             // Source ASTUnit ID
	DstID      string  `json:"dst_id,omitempty"`   // Destination ASTUnit ID (empty if unresolved)
	Kind       string  `json:"kind"`               // import, call, reference, implements, inherits
	DstName    string  `json:"dst_name,omitempty"` // Destination name (if unresolved)
	FilePath   string  `json:"file_path"`
	Line       int     `json:"line"`
	DstRepoID  string  `json:"dst_repo_id,omitempty"` // For cross-repo edges
	Confidence float32 `json:"confidence"`            // 0-1, for ML-based edges
	Meta       string  `json:"meta,omitempty"`        // JSON: call args, wire field mappings, topic, path
}
