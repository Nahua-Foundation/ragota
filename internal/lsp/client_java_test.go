package lsp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestHandleLanguageStatusClosesJavaReady — быстрый unit-тест: проверяем, что
// notification "language/status" с type:"ServiceReady" закрывает канал
// c.javaReady, и что повторное получение не паникует (атомарность через
// c.javaReadyClosed).
func TestHandleLanguageStatusClosesJavaReady(t *testing.T) {
	cases := []struct {
		name      string
		statusTyp string
		wantReady bool
	}{
		{"ServiceReady closes", "ServiceReady", true},
		{"Started closes", "Started", true},
		{"Ready closes", "Ready", true},
		{"Starting does not close", "Starting", false},
		{"Error does not close", "Error", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{
				Language:         "java",
				diagnosticsReady: make(chan string, 1),
				javaReady:        make(chan struct{}),
			}
			params, _ := json.Marshal(map[string]any{
				"type":    tc.statusTyp,
				"message": "test",
			})
			c.handleServerNotification("language/status", params)

			select {
			case <-c.javaReady:
				if !tc.wantReady {
					t.Fatalf("javaReady closed for status %q, expected NOT closed", tc.statusTyp)
				}
			default:
				if tc.wantReady {
					t.Fatalf("javaReady NOT closed for status %q, expected closed", tc.statusTyp)
				}
			}
		})
	}
}

// TestHandleLanguageStatusIdempotent — повторное получение готовности не должно
// паниковать close-ом уже закрытого канала.
func TestHandleLanguageStatusIdempotent(t *testing.T) {
	c := &Client{
		Language:         "java",
		diagnosticsReady: make(chan string, 1),
		javaReady:        make(chan struct{}),
	}
	params, _ := json.Marshal(map[string]any{"type": "ServiceReady"})
	c.handleServerNotification("language/status", params)
	c.handleServerNotification("language/status", params) // не должно паниковать
	c.handleServerNotification("language/status", params)
	if !c.javaReadyClosed.Load() {
		t.Fatal("javaReadyClosed flag must be true after ServiceReady")
	}
}

// TestJDTLSStart_Integration — реальный запуск jdtls. Скипается, если:
//   - не задана env JDTLS_INTEGRATION=1 (тяжёлый тест, ~30-120с)
//   - в PATH нет jdtls или java
//
// Проверяет, что после Start() клиент инициализируется, дожидается language/status,
// и отвечает на простой Hover без RPC-ошибки. Пустой результат Hover допустим
// (invisible-project режим), главное — что сервер жив и канал javaReady закрылся.
func TestJDTLSStart_Integration(t *testing.T) {
	if os.Getenv("JDTLS_INTEGRATION") != "1" {
		t.Skip("set JDTLS_INTEGRATION=1 to run jdtls integration test (~30-120s)")
	}
	if _, err := exec.LookPath("jdtls"); err != nil {
		t.Skipf("jdtls not in PATH: %v", err)
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skipf("java not in PATH: %v", err)
	}

	root := t.TempDir()
	javaFile := filepath.Join(root, "Hello.java")
	content := `public class Hello {
    public static void main(String[] args) {
        String greeting = "hi";
        System.out.println(greeting);
    }
}
`
	if err := os.WriteFile(javaFile, []byte(content), 0644); err != nil {
		t.Fatalf("write java file: %v", err)
	}

	// Сокращаем таймаут для теста до 90с — больше чем достаточно для invisible-project.
	t.Setenv("JDTLS_READY_TIMEOUT", "90")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	spec := ServerSpec{
		Language: "java",
		Command:  "jdtls",
		Args: []string{
			"--jvm-arg", "-Xmx1G",
			"-data", filepath.Join(root, ".jdtls-data"),
		},
	}
	client, err := Start(ctx, spec, root)
	if err != nil {
		t.Fatalf("Start jdtls: %v", err)
	}
	defer client.Close()

	// Start уже выполнил initialize синхронно (включая ожидание language/status).
	if !client.initialized.Load() {
		t.Fatal("client not initialized after Start")
	}

	if !client.IsAlive() {
		t.Fatalf("jdtls process is not alive after initialize")
	}

	// Канал готовности должен быть закрыт.
	select {
	case <-client.javaReady:
		// ok
	default:
		t.Fatal("javaReady was not closed after initialize — jdtls did not send language/status")
	}

	// Откроем файл и выполним Hover. Допускаем пустой ответ, но не RPC-ошибку.
	if err := client.DidOpen(javaFile, "java", content); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	// Дадим jdtls немного времени на анализ открытого файла.
	time.Sleep(2 * time.Second)

	hctx, hcancel := context.WithTimeout(ctx, 30*time.Second)
	defer hcancel()
	// Позиция (line=2, character=15) — внутри слова "greeting".
	if _, err := client.Hover(hctx, javaFile, 2, 15); err != nil {
		t.Fatalf("Hover RPC error: %v", err)
	}
}
