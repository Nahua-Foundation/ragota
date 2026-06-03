// Package detector обнаруживает потенциальные cross-repo вызовы
// (HTTP, gRPC, Kafka) через AST-patterns и эвристики.
//
// Работает без LLM — быстрый первичный отсев. Результат — список
// кандидатов для последующей LLM-классификации.
package detector

import (
	"context"

	"ragota/internal/store"
)

// CrossEdge — связь между репозиториями.
type CrossEdge struct {
	// Источник
	SrcRepo   string `json:"src_repo"`
	SrcFile   string `json:"src_file"`
	SrcLine   int    `json:"src_line"`
	SrcSymbol string `json:"src_symbol"`

	// Цель
	DstRepo    string `json:"dst_repo"`
	DstFile    string `json:"dst_file,omitempty"`
	DstSymbol  string `json:"dst_symbol,omitempty"`
	DstName    string `json:"dst_name"` // endpoint, topic, module path

	// Метаданные
	Protocol   string  `json:"protocol"`    // import, http, grpc, kafka, npm_package
	Confidence float64 `json:"confidence"`  // 0.0–1.0
	LLMReason  string  `json:"llm_reason"`  // объяснение от LLM (если был)
}

// Candidate — потенциальный cross-repo вызов.
type Candidate struct {
	Repo      string `json:"repo"`
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	Symbol    string `json:"symbol"`
	RawCode   string `json:"raw_code"`   // 5-10 строк кода вокруг вызова
	ProtoHint string `json:"proto_hint"` // "http", "grpc", "kafka" или ""
	Language  string `json:"language"`
}

// Detector обнаруживает кандидатов.
type Detector struct{}

// New создаёт detector.
func New() *Detector { return &Detector{} }

// DetectFromStore сканирует AST units из store и ищет вызовы,
// которые могут быть cross-repo (HTTP clients, gRPC stubs, Kafka producers).
func (d *Detector) DetectFromStore(ctx context.Context, st *store.SQLite) []Candidate {
	return d.detectUnresolvedCalls(ctx, st)
}

// detectUnresolvedCalls ищет функции с unresolved call edges
// и проверяет, являются ли они потенциально cross-repo вызовами.
func (d *Detector) detectUnresolvedCalls(ctx context.Context, st *store.SQLite) []Candidate {
	// Получаем все unresolved call edges
	allEdges, err := st.AllEdgesByKind(ctx, "call")
	if err != nil {
		return nil
	}

	var candidates []Candidate
	seen := make(map[string]bool) // file:line dedup

	for _, e := range allEdges {
		if e.DstID != 0 {
			continue // resolved — не cross-repo
		}

		srcUnit, _ := st.GetASTUnit(ctx, e.SrcID)
		if srcUnit == nil {
			continue
		}

		key := srcUnit.FilePath + ":" + string(rune(e.Line))
		if seen[key] {
			continue
		}
		seen[key] = true

		// Проверяем, выглядит ли dst_name как external вызов
		if !d.IsExternalCall(e.DstName, srcUnit.Language) {
			continue
		}

		// Извлекаем raw code
		rawCode := d.ExtractRawCode(srcUnit, e.Line)

		candidates = append(candidates, Candidate{
			Repo:     srcUnit.Repo,
			FilePath: srcUnit.FilePath,
			Line:     e.Line,
			Symbol:   srcUnit.Name,
			RawCode:  rawCode,
			Language: srcUnit.Language,
		})
	}

	return candidates
}

// IsExternalCall проверяет, выглядит ли имя вызова как external.
func (d *Detector) IsExternalCall(name, lang string) bool {
	// HTTP client patterns
	httpPatterns := []string{
		"fetch", "axios", "http.Get", "http.Post", "http.NewRequest",
		"requests.get", "requests.post", "urllib", "httpx",
		"RestTemplate", "WebClient", "HttpClient",
	}
	for _, p := range httpPatterns {
		if name == p || len(name) > len(p) && name[len(name)-len(p):] == p {
			return true
		}
	}

	// gRPC patterns
	grpcPatterns := []string{
		"grpc.Dial", "grpc.Invoke", "NewClient",
		"grpcClient", "grpc.NewClient",
	}
	for _, p := range grpcPatterns {
		if name == p || len(name) > len(p) && name[len(name)-len(p):] == p {
			return true
		}
	}

	// Kafka patterns
	kafkaPatterns := []string{
		"kafka.Producer", "kafka.Consumer", "Producer.Send",
		"consumer.Subscribe", "producer.Publish",
		"KafkaTemplate", "KafkaProducer", "KafkaConsumer",
	}
	for _, p := range kafkaPatterns {
		if name == p || len(name) > len(p) && name[len(name)-len(p):] == p {
			return true
		}
	}

	return false
}

// ExtractRawCode извлекает ~10 строк кода вокруг указанной линии.
func (d *Detector) ExtractRawCode(unit *store.ASTUnit, line int) string {
	src, err := unit.ReadSource(store.SourceOptions{})
	if err != nil {
		return ""
	}

	lines := splitLines(src)
	if len(lines) == 0 {
		return ""
	}

	start := line - 6 // 5 lines before
	if start < 0 {
		start = 0
	}
	end := line + 4 // 5 lines after
	if end > len(lines) {
		end = len(lines)
	}
	// Guard: line number beyond file length
	if start >= end {
		start = 0
	}
	if end <= start {
		end = len(lines)
	}

	return joinLines(lines[start:end])
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func joinLines(lines []string) string {
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n"
		}
		result += l
	}
	return result
}
