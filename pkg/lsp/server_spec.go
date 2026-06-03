package lsp

// ServerSpec описывает запускаемый LSP-сервер.
type ServerSpec struct {
	Language  string   // "go" | "typescript" | "python" | "java"
	Command   string   // например "gopls"
	Args      []string // аргументы
	LocalRoot string   // локальный корень проекта
	IsDocker  bool     // true если сервер запускается в Docker-контейнере
}

// DefaultServers — рекомендуемые LSP-серверы.
// Если бинаря нет в PATH, сервер для этого языка просто не стартует.
func DefaultServers() []ServerSpec {
	return []ServerSpec{
		{Language: "go", Command: "gopls"},
		{Language: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}},
		{Language: "python", Command: "pyright-langserver", Args: []string{"--stdio"}},
		// ВАЖНО: argparse в jdtls трактует значение, начинающееся с "--",
		// как следующий флаг ("argument --jvm-arg: expected one argument").
		// Поэтому используем форму --jvm-arg=VALUE (единый токен), а не
		// два отдельных аргумента "--jvm-arg", "--add-opens=...".
		{Language: "java", Command: "jdtls", Args: []string{
			"--jvm-arg=-Xmx4G",
			"--jvm-arg=--add-opens=java.base/sun.misc=ALL-UNNAMED",
			"--jvm-arg=--add-opens=java.base/java.lang.reflect=ALL-UNNAMED",
			"--jvm-arg=--add-opens=java.base/java.util=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.api=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.util=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.code=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.main=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.tree=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.model=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.comp=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.file=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.jvm=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.parser=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.processing=ALL-UNNAMED",
			"-data", ".ragota/jdtls-data",
		}},
	}
}
