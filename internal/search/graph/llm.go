package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ragota/internal/store"
)

// extractJSON извлекает JSON из markdown-обёртки (```json ... ```).
// Если обёртки нет, возвращает исходную строку.
func extractJSON(llmRes string) string {
	extracted := llmRes
	if start := strings.Index(llmRes, "```"); start >= 0 {
		rest := llmRes[start+3:]
		if strings.HasPrefix(rest, "json") || strings.HasPrefix(rest, "JSON") {
			nl := strings.Index(rest, "\n")
			if nl >= 0 {
				rest = rest[nl+1:]
			}
		}
		end := strings.Index(rest, "```")
		if end > 0 {
			extracted = strings.TrimSpace(rest[:end])
		}
	}
	return extracted
}

// GetSymbolSummary собирает детерминированные данные и обогащает их через LLM.
func (s *Service) GetSymbolSummary(ctx context.Context, symbolID int) (*SymbolSummary, error) {
	u, err := s.st.GetASTUnit(ctx, symbolID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("symbol not found")
	}

	res := &SymbolSummary{
		Name:      u.Name,
		Signature: u.Signature,
	}

	// Deterministic part
	callers, _ := s.Callers(ctx, symbolID)
	for _, c := range callers {
		res.Callers = append(res.Callers, c.Qualified)
	}
	callees, _ := s.Callees(ctx, symbolID)
	for _, c := range callees {
		res.Calls = append(res.Calls, c.Qualified)
	}

	// Fetch parent info for prompt
	var parentName string
	if u.ParentID.Valid {
		parent, _ := s.st.GetASTUnit(ctx, int(u.ParentID.Int64))
		if parent != nil {
			parentName = parent.Name
		}
	}

	// Semantic part (LLM)
	sourceCode := s.sourceContent(*u)
	prompt := buildSymbolSummaryPrompt(u, parentName, sourceCode, res.Calls, res.Callers)

	llmRes, err := s.callOllama(ctx, prompt, s.cfg.Ollama.SymbolModel)
	if err != nil {
		res.LLMError = fmt.Sprintf("LLM model unavailable: %v. LLM fields (purpose, role, importance) are omitted.", err)
	} else {
		var sem struct {
			Purpose    string `json:"purpose"`
			Role       string `json:"role"`
			Importance string `json:"importance"`
		}
		errParse := json.Unmarshal([]byte(llmRes), &sem)
		if errParse != nil {
			errParse = json.Unmarshal([]byte(extractJSON(llmRes)), &sem)
		}
		if errParse != nil {
			res.LLMError = fmt.Sprintf("LLM response parse failed: %v. Raw response: %q", errParse, llmRes)
		} else {
			res.Purpose = sem.Purpose
			res.Role = sem.Role
			res.Importance = sem.Importance
		}
	}

	return res, nil
}

// GetFileIntent анализирует файл через Tree-sitter и LLM.
func (s *Service) GetFileIntent(ctx context.Context, path string) (*FileIntent, error) {
	units, err := s.st.ListASTUnitsByFile(ctx, path)
	if err != nil {
		return nil, err
	}

	res := &FileIntent{
		Symbols:          []string{},
		Imports:          []string{},
		Responsibilities: []string{},
	}
	importSet := make(map[string]struct{})
	language := ""
	for _, u := range units {
		if u.Kind != "file" && u.Kind != "module" && u.Kind != "package" {
			res.Symbols = append(res.Symbols, u.Name)
		}
		edges, _ := s.st.EdgesFrom(ctx, u.ID, EdgeImport)
		for _, e := range edges {
			importSet[e.DstName] = struct{}{}
		}
		if u.Language != "" {
			language = u.Language
		}
	}
	for imp := range importSet {
		res.Imports = append(res.Imports, imp)
	}

	srcCode, err := s.sourceContentFile(path)
	if err != nil {
		srcCode = "(source unavailable)"
	}

	prompt := buildFileIntentPrompt(language, path, res.Symbols, res.Imports, srcCode)

	llmRes, err := s.callOllama(ctx, prompt, s.cfg.Ollama.SymbolModel)
	if err != nil {
		res.LLMError = fmt.Sprintf("LLM model unavailable: %v. LLM fields (purpose, layer, responsibilities) are omitted.", err)
	} else {
		var sem struct {
			Purpose          string   `json:"purpose"`
			Layer            string   `json:"layer"`
			Responsibilities []string `json:"responsibilities"`
		}
		errParse := json.Unmarshal([]byte(llmRes), &sem)
		if errParse != nil {
			errParse = json.Unmarshal([]byte(extractJSON(llmRes)), &sem)
		}
		if errParse != nil {
			res.LLMError = fmt.Sprintf("LLM response parse failed: %v. Raw response: %q", errParse, llmRes)
		} else {
			res.Purpose = sem.Purpose
			res.Layer = sem.Layer
			res.Responsibilities = sem.Responsibilities
			res.Layer = s.validateFileLayer(path, units, res.Layer)
		}
	}

	return res, nil
}

// validateFileLayer корректирует layer, если LLM явно ошибся.
func (s *Service) validateFileLayer(path string, units []store.ASTUnit, claimedLayer string) string {
	if claimedLayer == "" {
		return claimedLayer
	}

	hasInterface := false
	hasImplementation := false
	isTest := false
	for _, u := range units {
		switch u.Kind {
		case "interface":
			hasInterface = true
		case "function", "method", "struct":
			hasImplementation = true
		}
		if strings.HasSuffix(u.Name, "_test") || strings.HasSuffix(path, "_test.go") {
			isTest = true
		}
	}

	layer := strings.ToLower(claimedLayer)

	if layer == "interface" && !hasInterface && hasImplementation {
		return "implementation"
	}
	if isTest && layer != "test" {
		return "test"
	}

	return claimedLayer
}

// GetSemanticNeighborhood выполняет детерминированное расширение и LLM-кластеризацию.
func (s *Service) GetSemanticNeighborhood(ctx context.Context, symbolID int) (*SemanticNeighborhood, error) {
	u, err := s.st.GetASTUnit(ctx, symbolID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("symbol not found")
	}

	res := &SemanticNeighborhood{
		Center: u.Name,
	}

	// Step 1: Deterministic
	callers, _ := s.Callers(ctx, symbolID)
	for _, c := range callers {
		res.Neighbors.Callers = append(res.Neighbors.Callers, c.Name)
	}
	callees, _ := s.Callees(ctx, symbolID)
	for _, c := range callees {
		res.Neighbors.DirectCalls = append(res.Neighbors.DirectCalls, c.Name)
	}
	outEdges, _ := s.st.EdgesFrom(ctx, symbolID, EdgeReference)
	for _, e := range outEdges {
		res.Neighbors.Types = append(res.Neighbors.Types, e.DstName)
	}

	// Step 2: LLM Compression
	prompt := buildSemanticNeighborhoodPrompt(u, res.Neighbors)

	llmRes, err := s.callOllama(ctx, prompt, s.cfg.Ollama.SymbolModel)
	if err != nil {
		res.LLMError = fmt.Sprintf("LLM model unavailable: %v. LLM fields (cluster, core, dependencies, boundary) are omitted.", err)
	} else {
		var sem struct {
			Cluster      string   `json:"cluster"`
			Core         []string `json:"core"`
			Dependencies []string `json:"dependencies"`
			Boundary     []string `json:"boundary"`
		}
		errParse := json.Unmarshal([]byte(llmRes), &sem)
		if errParse != nil {
			errParse = json.Unmarshal([]byte(extractJSON(llmRes)), &sem)
		}
		if errParse != nil {
			res.LLMError = fmt.Sprintf("LLM response parse failed: %v. Raw response: %q", errParse, llmRes)
		} else {
			res.Cluster = sem.Cluster
			res.Core = sem.Core
			res.Dependencies = sem.Dependencies
			res.Boundary = sem.Boundary
		}
	}

	return res, nil
}

func (s *Service) callOllama(ctx context.Context, prompt, model string) (string, error) {
	start := time.Now()
	url := strings.TrimRight(s.cfg.Ollama.URL, "/") + "/api/generate"
	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"format": "json",
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.recordOllamaLatency(model, start, err)
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(b))
		s.recordOllamaLatency(model, start, err)
		return "", err
	}

	var res struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		s.recordOllamaLatency(model, start, err)
		return "", err
	}
	s.recordOllamaLatency(model, start, nil)
	return res.Response, nil
}

func (s *Service) recordOllamaLatency(model string, start time.Time, err error) {
	if s.bus == nil {
		return
	}
	ms := float64(time.Since(start).Milliseconds())
	s.bus.SetOllamaLatency(model, ms, err != nil)
}

// sourceContent возвращает исходный код AST-юнита из файла.
func (s *Service) sourceContent(u store.ASTUnit) string {
	src, _ := u.ReadSource(store.SourceOptions{})
	return src
}

// sourceContentFile возвращает полный исходный код файла.
func (s *Service) sourceContentFile(path string) (string, error) {
	u := store.ASTUnit{FilePath: path}
	return u.ReadFullSource()
}
