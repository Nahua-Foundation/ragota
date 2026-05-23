// Package chunker разбивает исходный файл на чанки для векторного индекса.
// По умолчанию — построчное окно с перекрытием.
// Если для языка есть символы (function/class), они также добавляются как
// отдельные семантические чанки.
package chunker

import (
	"strings"

	"aitools/internal/parser"
)

const (
	// MaxChunkBytes — максимальный размер чанка в байтах.
	// Модели (nomic-embed-text) имеют лимит контекста 8192 токенов.
	// 2000 байт — безопасный порог, гарантированно вписывающийся в лимит
	// большинства моделей даже с учетом токенизации кода.
	MaxChunkBytes = 2000
)

// Chunk — кусок текста для эмбеддинга.
type Chunk struct {
	Path      string
	Text      string
	StartLine int
	EndLine   int
	Kind      string   // "window" | "symbol" | "tree"
	Symbol    string   // имя символа (для kind="symbol")
	Language  string   // язык файла
	Parent    string   // родительский класс/модуль
	Imports   []string // список импортов в файле
	// Comments — лидирующие doc-комментарии символа (если есть). Включаются
	// в combined text для embedding и попадают в payload как отдельное поле.
	Comments string
}

// Chunker конфигурирует параметры разбиения.
type Chunker struct {
	WindowLines  int
	OverlapLines int
}

// New создаёт чанкер с дефолтами при некорректных значениях.
func New(window, overlap int) *Chunker {
	if window <= 0 {
		window = 60
	}
	if overlap < 0 || overlap >= window {
		overlap = window / 6
	}
	return &Chunker{WindowLines: window, OverlapLines: overlap}
}

// Chunk разбивает source. symbols может быть nil.
func (c *Chunker) Chunk(path, lang string, source []byte, symbols []parser.Symbol) []Chunk {
	lines := strings.Split(string(source), "\n")
	var out []Chunk

	var imports []string
	if len(symbols) > 0 {
		imports = symbols[0].Imports
	}

	// Окна построчно с перекрытием
	step := c.WindowLines - c.OverlapLines
	if step <= 0 {
		step = c.WindowLines
	}
	for start := 0; start < len(lines); start += step {
		end := start + c.WindowLines
		if end > len(lines) {
			end = len(lines)
		}
		text := strings.Join(lines[start:end], "\n")
		if strings.TrimSpace(text) == "" {
			continue
		}

		// Разбиваем на под-чанки, если превышен лимит
		sub := splitText(path, lang, text, start+1, "window", "", "", imports, "")
		out = append(out, sub...)
	}

	// Символьные чанки — добавляем только для function/method/class/interface
	for _, s := range symbols {
		switch s.Kind {
		case "function", "method", "class", "interface", "type":
		default:
			continue
		}
		if s.StartLine < 1 || s.EndLine < s.StartLine || s.StartLine > len(lines) {
			continue
		}
		endLine := s.EndLine
		if endLine > len(lines) {
			endLine = len(lines)
		}
		text := strings.Join(lines[s.StartLine-1:endLine], "\n")
		if strings.TrimSpace(text) == "" {
			continue
		}

		// Разбиваем на под-чанки, если превышен лимит
		sub := splitText(path, lang, text, s.StartLine, "symbol", s.Name, s.Parent, s.Imports, s.Doc)
		out = append(out, sub...)
	}
	return out
}

// ChunkByTree превращает AST-чанки от парсера в Chunk.
// Используется для семантического разбиения всего файла.
func (c *Chunker) ChunkByTree(path, lang string, source []byte, treeChunks []parser.Symbol) []Chunk {
	var out []Chunk
	for _, tc := range treeChunks {
		if tc.StartByte < 0 || tc.EndByte > len(source) || tc.StartByte >= tc.EndByte {
			continue
		}
		text := string(source[tc.StartByte:tc.EndByte])
		if strings.TrimSpace(text) == "" {
			continue
		}
		// Даже AST-чанки прогоняем через splitText на случай гигантских листьев
		sub := splitText(path, lang, text, tc.StartLine, "tree", "", tc.Parent, tc.Imports, tc.Doc)
		out = append(out, sub...)
	}
	return out
}

// splitText разбивает текст на куски не более MaxChunkBytes.
// Старается не резать посередине строк, если это возможно.
func splitText(path, lang, text string, startLine int, kind, symbol, parent string, imports []string, comments string) []Chunk {
	if len(text) <= MaxChunkBytes {
		return []Chunk{{
			Path:      path,
			Text:      text,
			StartLine: startLine,
			EndLine:   startLine + strings.Count(text, "\n"),
			Kind:      kind,
			Symbol:    symbol,
			Language:  lang,
			Parent:    parent,
			Imports:   imports,
			Comments:  comments,
		}}
	}

	var chunks []Chunk
	lines := strings.Split(text, "\n")

	currentText := ""
	currentStart := startLine

	for i, line := range lines {
		separator := ""
		if currentText != "" {
			separator = "\n"
		}

		if len(currentText)+len(separator)+len(line) > MaxChunkBytes {
			// Сбрасываем текущий чанк
			if currentText != "" {
				chunks = append(chunks, Chunk{
					Path:      path,
					Text:      currentText,
					StartLine: currentStart,
					EndLine:   currentStart + strings.Count(currentText, "\n"),
					Kind:      kind,
					Symbol:    symbol,
					Language:  lang,
					Parent:    parent,
					Imports:   imports,
					Comments:  comments,
				})
				currentText = ""
			}

			// Если сама строка длиннее лимита — режем её по рунам
			if len(line) > MaxChunkBytes {
				runes := []rune(line)
				tmp := ""
				tmpStart := startLine + i
				for _, r := range runes {
					s := string(r)
					if len(tmp)+len(s) > MaxChunkBytes {
						chunks = append(chunks, Chunk{
							Path:      path,
							Text:      tmp,
							StartLine: tmpStart,
							EndLine:   tmpStart,
							Kind:      kind,
							Symbol:    symbol,
							Language:  lang,
							Parent:    parent,
							Imports:   imports,
							Comments:  comments,
						})
						tmp = ""
					}
					tmp += s
				}
				currentText = tmp
				currentStart = startLine + i
			} else {
				currentText = line
				currentStart = startLine + i
			}
		} else {
			currentText += separator + line
		}
	}

	if currentText != "" {
		chunks = append(chunks, Chunk{
			Path:      path,
			Text:      currentText,
			StartLine: currentStart,
			EndLine:   currentStart + strings.Count(currentText, "\n"),
			Kind:      kind,
			Symbol:    symbol,
			Language:  lang,
			Parent:    parent,
			Imports:   imports,
			Comments:  comments,
		})
	}

	return chunks
}
