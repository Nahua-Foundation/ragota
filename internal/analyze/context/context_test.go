package context

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectLanguages_Go(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module test"), 0o644))

	langs := DetectLanguages(tmp)
	assert.NotEmpty(t, langs)

	found := false
	for _, l := range langs {
		if l.Name == "go" {
			found = true
			assert.GreaterOrEqual(t, l.Confidence, 80)
			assert.Contains(t, l.Extensions, ".go")
		}
	}
	assert.True(t, found, "should detect Go")
}

func TestDetectLanguages_TypeScript(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "package.json"), []byte(`{"name":"test"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "tsconfig.json"), []byte(`{}`), 0o644))

	langs := DetectLanguages(tmp)
	found := false
	for _, l := range langs {
		if l.Name == "typescript" {
			found = true
			assert.Equal(t, 95, l.Confidence)
			assert.Contains(t, l.Extensions, ".ts")
			assert.Contains(t, l.Extensions, ".tsx")
		}
	}
	assert.True(t, found, "should detect TypeScript")
}

func TestDetectLanguages_Python(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte("[project]"), 0o644))

	langs := DetectLanguages(tmp)
	found := false
	for _, l := range langs {
		if l.Name == "python" {
			found = true
			assert.GreaterOrEqual(t, l.Confidence, 90)
		}
	}
	assert.True(t, found, "should detect Python")
}

func TestDetectLanguages_Empty(t *testing.T) {
	tmp := t.TempDir()
	langs := DetectLanguages(tmp)
	assert.Empty(t, langs, "empty project should detect nothing")
}

func TestDetectLanguages_MultiLang(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module test"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "package.json"), []byte(`{"name":"frontend"}`), 0o644))

	langs := DetectLanguages(tmp)
	assert.GreaterOrEqual(t, len(langs), 2, "should detect multiple languages")
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"UserProfile", []string{"user", "profile"}},
		{"payment_gateway", []string{"payment", "gateway"}},
		{"order-service", []string{"order", "service"}},
		{"HTTPClient", []string{"http", "client"}},
		{"simple", []string{"simple"}},
		{"a", nil},         // too short
		{"ab", nil},        // too short
		{"abc", []string{"abc"}},
		{"user.profile.page", []string{"user", "profile", "page"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := tokenize(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractTermsFromNames(t *testing.T) {
	tmp := t.TempDir()

	// Создаём структуру
	dirs := []string{
		"internal/payment",
		"internal/order",
		"internal/order/handler",
		"internal/user",
		"pkg/notification",
		"pkg/notification/email",
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(tmp, d), 0o755))
	}

	files := map[string]string{
		"internal/payment/service.go":             "",
		"internal/payment/repository.go":          "",
		"internal/payment/gateway.go":             "",
		"internal/order/handler.go":               "",
		"internal/order/service.go":               "",
		"internal/order/repository.go":            "",
		"internal/user/profile.go":                "",
		"internal/user/auth.go":                   "",
		"pkg/notification/sender.go":              "",
		"pkg/notification/email/smtp_client.go":   "",
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(tmp, name), []byte(content), 0o644))
	}

	terms := ExtractTermsFromNames(tmp, nil)

	// Проверяем, что доменные термины извлечены
	assert.NotNil(t, terms["payment"], "should extract 'payment'")
	assert.NotNil(t, terms["order"], "should extract 'order'")
	assert.NotNil(t, terms["user"], "should extract 'user'")
	assert.NotNil(t, terms["notification"], "should extract 'notification'")

	// Проверяем, что шумовые термины отфильтрованы
	assert.Nil(t, terms["internal"], "should filter 'internal' as noise")
}

func TestFilterSignificantTerms(t *testing.T) {
	terms := map[string]*Term{
		"payment": {Name: "payment", Freq: 5},
		"order":   {Name: "order", Freq: 10},
		"rare":    {Name: "rare", Freq: 1},
	}

	result := FilterSignificantTerms(terms, 3)
	assert.Len(t, result, 2)
	assert.Equal(t, "order", result[0].Name) // sorted by freq desc
	assert.Equal(t, "payment", result[1].Name)
}

func TestIsNoise(t *testing.T) {
	// Шумовые термины (технические, не доменные)
	assert.True(t, isNoise("test"))
	assert.True(t, isNoise("util"))
	assert.True(t, isNoise("ab")) // слишком короткий

	// Архитектурные паттерны НЕ являются шумом — могут быть доменными терминами
	// Например: ClaimsHandler (обработчик претензий), OrderService (сервис заказов)
	assert.False(t, isNoise("handler"))
	assert.False(t, isNoise("service"))
	assert.False(t, isNoise("controller"))

	// Доменные термины
	assert.False(t, isNoise("payment"))
	assert.False(t, isNoise("order"))
	assert.False(t, isNoise("user"))
}
