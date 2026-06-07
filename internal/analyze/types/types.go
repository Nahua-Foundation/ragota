package types

// Chunk — чанк кода из файла (начало/середина/конец).
type Chunk struct {
	Content   string `json:"content"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Position  string `json:"position"` // "start" | "middle" | "end"
}

// Entry — запись о заблокированном файле/директории.
type Entry struct {
	Path       string
	Pattern    string
	Stage      string // "heuristic" | "llm" | "negation"
	Reason     string
	Confidence int // 0-100
}

// FileMeta — метаданные одного файла для LLM-оценки.
type FileMeta struct {
	Path         string   `json:"path"`
	Extension    string   `json:"extension"`
	Size         int64    `json:"size"`
	EstLines     int      `json:"est_lines"`
	Flags        []string `json:"flags"`
	Imports      []string `json:"imports,omitempty"`
	Signatures   []string `json:"signatures,omitempty"`
	SampleChunks []Chunk  `json:"sample_chunks"`
}

// SubGroup — подгруппа файлов внутри одной директории (для рекурсивной выборки).
type SubGroup struct {
	DirPath string     `json:"dir_path"` // "issuance/src/ndm/"
	Files   []string   `json:"files"`
	Samples []FileMeta `json:"samples"` // Выбранные файлы с метаданными
	Depth   int        `json:"depth"`   // Глубина в дереве
}

// FileGroup — группа файлов с одинаковым паттерном в определённой области.
type FileGroup struct {
	Pattern   string     // "*.yaml" или "k8s/*.yaml"
	Files     []string
	Dirs      []string
	SubGroups []*SubGroup `json:"sub_groups,omitempty"` // NEW: рекурсивные подгруппы
}

// PreScreenResult — результат pre-screen эвристики.
type PreScreenResult struct {
	AutoIgnored []Entry
	AutoKept    []string
	Remaining   []string
}

// GroupDecision — решение LLM по группе файлов.
type GroupDecision struct {
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`     // "keep" | "ignore"
	Confidence int    `json:"confidence"` // 0-100
}
