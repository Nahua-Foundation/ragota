package heuristic

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// BufPool — пул буферов для чтения файлов (4KB).
var BufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 4096)
		return &buf
	},
}

// BinaryExts — расширения бинарных файлов (auto-ignore без чтения).
var BinaryExts = map[string]bool{
	// Images
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true,
	".ico": true, ".svg": true, ".webp": true, ".tiff": true,
	// Fonts
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true,
	// Media
	".mp4": true, ".mp3": true, ".wav": true, ".avi": true, ".mov": true,
	".flac": true, ".ogg": true, ".webm": true,
	// Archives
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true,
	".7z": true, ".rar": true,
	// Executables and libraries
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".a": true,
	".o": true, ".obj": true,
	// Java binaries
	".jar": true, ".war": true, ".ear": true, ".class": true,
	// Databases
	".sqlite": true, ".db": true, ".mdb": true,
	// Protobuf binary data
	".pb": true, ".protodep": true,
	// Lock files (dependency locks)
	".lock": true,
	// Other binary
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true,
	// Backup and temp files
	".bak": true, ".tmp": true, ".swp": true, ".swo": true, ".old": true,
	// Vector DB and index files (Qdrant, BM25)
	".dat": true, ".mmap": true, ".zap": true, ".idx": true, ".index": true,
}

// IsBinaryExt возвращает true для известных бинарных расширений.
func IsBinaryExt(ext string) bool {
	return BinaryExts[strings.ToLower(ext)]
}

// IsBackupOrTempFile проверяет, является ли файл временным или бэкапом.
func IsBackupOrTempFile(name string) bool {
	// Файлы, заканчивающиеся на ~ (emacs/vim backup)
	if strings.HasSuffix(name, "~") {
		return true
	}
	// Файлы с расширением .bak, .tmp, .swp, .swo
	ext := strings.ToLower(filepath.Ext(name))
	return BinaryExts[ext]
}

// IsBinaryFile проверяет, является ли файл бинарным (не текстовым).
// Использует sync.Pool для буферов и проверку на regular file.
func IsBinaryFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	bufPtr := BufPool.Get().(*[]byte)
	buf := *bufPtr
	defer BufPool.Put(bufPtr)

	n, _ := f.Read(buf[:512])
	if n == 0 {
		return false
	}

	for _, b := range buf[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}
