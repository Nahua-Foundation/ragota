package classify

import (
	"strings"
)

// FileCategory represents the architectural role of a file.
type FileCategory string

const (
	CategoryModel          FileCategory = "model"          // data structures, DTOs, entities
	CategoryLogic          FileCategory = "logic"          // business logic, handlers, services
	CategoryInterface      FileCategory = "interface"      // API endpoints, gRPC, HTTP handlers
	CategoryInfrastructure FileCategory = "infrastructure" // DB access, external clients, config
	CategoryTest           FileCategory = "test"           // test files
	CategoryConfig         FileCategory = "config"         // configuration files
	CategoryDocumentation  FileCategory = "documentation"  // docs, README, CHANGELOG
	CategoryUnknown        FileCategory = "unknown"        // cannot determine
)

// ClassificationResult holds the classification outcome with confidence.
type ClassificationResult struct {
	Category   FileCategory
	Confidence int    // 0-100
	Reason     string // explanation
}

// Classifier determines the architectural role of a file based on its content and path.
type Classifier struct {
	// Language-specific patterns can be added here
}

// NewClassifier creates a new file classifier.
func NewClassifier() *Classifier {
	return &Classifier{}
}

// Classify analyzes file content and path to determine its architectural role.
//
// Parameters:
//   - path: relative file path
//   - content: first N lines of the file
//   - imports: extracted import statements
//   - signatures: extracted function/type signatures
func (c *Classifier) Classify(path, content string, imports, signatures []string) ClassificationResult {
	lower := strings.ToLower(path)

	// Fast path: test files
	if c.isTestPath(lower) {
		return ClassificationResult{
			Category:   CategoryTest,
			Confidence: 95,
			Reason:     "test file path",
		}
	}

	// Fast path: third-party proto dependencies → skip indexing
	if c.isThirdPartyProto(lower) {
		return ClassificationResult{
			Category:   CategoryInfrastructure,
			Confidence: 95,
			Reason:     "third-party proto dependency (not business logic)",
		}
	}

	// Fast path: config files
	if c.isConfigPath(lower) {
		return ClassificationResult{
			Category:   CategoryConfig,
			Confidence: 90,
			Reason:     "configuration file",
		}
	}

	// Fast path: documentation
	if c.isDocumentationPath(lower) {
		return ClassificationResult{
			Category:   CategoryDocumentation,
			Confidence: 95,
			Reason:     "documentation file",
		}
	}

	// Content-based classification
	contentLower := strings.ToLower(content)
	importsJoined := strings.ToLower(strings.Join(imports, " "))
	sigJoined := strings.ToLower(strings.Join(signatures, " "))

	// Interface detection
	if c.isInterface(contentLower, importsJoined, sigJoined, lower) {
		return ClassificationResult{
			Category:   CategoryInterface,
			Confidence: 85,
			Reason:     "API/endpoint definitions",
		}
	}

	// Infrastructure detection
	if c.isInfrastructure(contentLower, importsJoined, lower) {
		return ClassificationResult{
			Category:   CategoryInfrastructure,
			Confidence: 80,
			Reason:     "infrastructure code",
		}
	}

	// Model detection
	if c.isModel(contentLower, sigJoined, lower) {
		return ClassificationResult{
			Category:   CategoryModel,
			Confidence: 75,
			Reason:     "data structure definitions",
		}
	}

	// Logic detection (default for code files)
	if c.isLogic(contentLower, sigJoined) {
		return ClassificationResult{
			Category:   CategoryLogic,
			Confidence: 70,
			Reason:     "business logic",
		}
	}

	return ClassificationResult{
		Category:   CategoryUnknown,
		Confidence: 50,
		Reason:     "cannot determine role",
	}
}

func (c *Classifier) isTestPath(path string) bool {
	testPatterns := []string{
		"_test.", ".test.", ".spec.", "/test/", "/tests/",
		"test_", "/__tests__/", "/spec/",
	}
	for _, p := range testPatterns {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

func (c *Classifier) isConfigPath(path string) bool {
	configPatterns := []string{
		".yaml", ".yml", ".toml", ".ini", ".conf", ".cfg",
		".env", ".json", "config/", "configs/", "conf/",
		"settings/", ".config/",
	}
	for _, p := range configPatterns {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

func (c *Classifier) isDocumentationPath(path string) bool {
	docPatterns := []string{
		"readme", "changelog", "license", "contributing",
		".md", ".rst", ".txt", "/docs/", "/documentation/",
	}
	for _, p := range docPatterns {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

func (c *Classifier) isInterface(content, imports, sigs, path string) bool {
	// HTTP/gRPC patterns
	httpPatterns := []string{
		"router", "handler", "controller", "endpoint", "route",
		"http.handle", "http.listen", "app.get(", "app.post(",
		"mux.handle", "gin.engine", "echo.", "fastapi",
		"@getmapping", "@postmapping", "@putmapping", "@deletemapping",
	}

	grpcPatterns := []string{
		"grpc.server", "grpc.client", "protoc", ".proto",
		"service ", "rpc ", "register server",
	}

	// Check path hints
	interfacePathHints := []string{
		"/handler/", "/handlers/", "/controller/", "/controllers/",
		"/api/", "/endpoint/", "/endpoints/", "/route/", "/routes/",
		"/server/", "/grpc/", "/rest/", "/http/",
	}

	for _, p := range interfacePathHints {
		if strings.Contains(path, p) {
			return true
		}
	}

	for _, p := range httpPatterns {
		if strings.Contains(sigs, p) || strings.Contains(imports, p) {
			return true
		}
	}

	for _, p := range grpcPatterns {
		if strings.Contains(content, p) || strings.Contains(imports, p) {
			return true
		}
	}

	return false
}

func (c *Classifier) isInfrastructure(content, imports, path string) bool {
	infraPathHints := []string{
		"/repository/", "/repositories/", "/dao/", "/dal/",
		"/db/", "/database/", "/migration/", "/migrations/",
		"/client/", "/clients/", "/adapter/", "/adapters/",
		"/middleware/", "/infra/", "/infrastructure/",
		"/cache/", "/queue/", "/pubsub/", "/storage/",
	}

	for _, p := range infraPathHints {
		if strings.Contains(path, p) {
			return true
		}
	}

	infraPatterns := []string{
		"database", "mongodb", "postgres", "mysql", "redis",
		"sql.open", "sql.db", "gorm", "ent.", "sequelize",
		"mongodb", "mongoose", "typeorm", "prisma",
		"rabbitmq", "kafka", "nats", "amqp",
		"aws.", "s3.", "dynamodb", "sqs.", "sns.",
		"kubernetes", "docker", "helm", "terraform",
	}

	for _, p := range infraPatterns {
		if strings.Contains(content, p) || strings.Contains(imports, p) {
			return true
		}
	}

	return false
}

func (c *Classifier) isModel(content, sigs, path string) bool {
	modelPathHints := []string{
		"/model/", "/models/", "/entity/", "/entities/",
		"/dto/", "/types/", "/schema/", "/schemas/",
		"/domain/", "/core/",
	}

	for _, p := range modelPathHints {
		if strings.Contains(path, p) {
			return true
		}
	}

	// Look for struct/class definitions without much logic
	modelPatterns := []string{
		"type ", "struct ", "class ", "interface ",
		"message ", "enum ", "typedef ",
	}

	structCount := 0
	for _, p := range modelPatterns {
		structCount += strings.Count(sigs, p)
	}

	// If we have many type definitions and few functions, it's likely a model file
	funcPatterns := []string{"func ", "function ", "def ", "fn ", "public ", "private "}
	funcCount := 0
	for _, p := range funcPatterns {
		funcCount += strings.Count(sigs, p)
	}

	// Heuristic: if structs > funcs, it's a model file
	if structCount > funcCount && structCount > 0 {
		return true
	}

	return false
}

func (c *Classifier) isLogic(content, sigs string) bool {
	logicPatterns := []string{
		"func ", "function ", "def ", "fn ",
		"if ", "for ", "while ", "switch ",
		"return ", "error", "result",
		"service", "handler", "processor", "worker",
	}

	logicCount := 0
	for _, p := range logicPatterns {
		if strings.Contains(content, p) || strings.Contains(sigs, p) {
			logicCount++
		}
	}

	return logicCount >= 3
}

// isThirdPartyProto проверяет, является ли .proto файл сторонней зависимостью.
// Универсальное правило: third_party/, protodeps/, google/ — это внешние зависимости.
func (c *Classifier) isThirdPartyProto(path string) bool {
	if !strings.HasSuffix(path, ".proto") {
		return false
	}
	// Сторонние proto зависимости
	thirdPartyPatterns := []string{
		"/third_party/", "/protodeps/", "/vendor/",
		"/google/", "/protobuf/", "/protovalidate/",
	}
	for _, p := range thirdPartyPatterns {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}
