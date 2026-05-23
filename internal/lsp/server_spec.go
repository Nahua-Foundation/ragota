package lsp

import "regexp"

// ServerSpec описывает запускаемый LSP-сервер.
type ServerSpec struct {
	Language  string   // "go" | "typescript" | "python" | "java"
	Command   string   // например "gopls"
	Args      []string // аргументы
	LocalRoot string   // локальный корень проекта
}

// julHeaderRe матчит строку-заголовок java.util.logging вида:
//
//	"May 23, 2026 3:09:54 PM org.apache.aries.spifly.BaseActivator log"
//
// За такой строкой обычно идёт вторая с уровнем (INFO:/WARNING:/...).
var julHeaderRe = regexp.MustCompile(`^[A-Z][a-z]+ \d{1,2}, \d{4} \d{1,2}:\d{2}:\d{2} (AM|PM) \S+ (log|logp|logrb|info|warning|fine|finer|finest|config|severe)$`)

// DefaultServers — рекомендуемые LSP-серверы.
// Если бинаря нет в PATH, сервер для этого языка просто не стартует.
func DefaultServers() []ServerSpec {
	return []ServerSpec{
		{Language: "go", Command: "gopls"},
		{Language: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}},
		{Language: "python", Command: "pyright-langserver", Args: []string{"--stdio"}},
		{Language: "java", Command: "jdtls", Args: []string{
			"--jvm-arg", "-Xmx4G",
			"--jvm-arg", "--add-opens=java.base/sun.misc=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=java.base/java.lang.reflect=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=java.base/java.util=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.api=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.util=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.code=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.main=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.tree=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.model=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.comp=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.file=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.jvm=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.parser=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.processing=ALL-UNNAMED",
			"-data", ".ai-tools/jdtls-data",
		}},
	}
}
