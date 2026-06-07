package llm

import (
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"ragota/internal/analyze/heuristic"
	"ragota/internal/analyze/types"
)

// fileMetaCache кэширует FileMeta и chunks для избежания повторных чтений файлов.
var (
	fileMetaCache  sync.Map // path → types.FileMeta
	chunksCache    sync.Map // path → []types.Chunk
)

// SelectSamplesFromSubGroups выбирает 20% файлов из каждой подгруппы (листа дерева).
// Для каждого выбранного файла извлекает 3 чанка (начало/середина/конец по 10 строк).
func SelectSamplesFromSubGroups(subGroups []*types.SubGroup, root string) []*types.SubGroup {
	const samplePercentage = 20
	const minSamplesPerGroup = 1

	for _, sg := range subGroups {
		sampleCount := len(sg.Files) * samplePercentage / 100
		if sampleCount < minSamplesPerGroup {
			sampleCount = minSamplesPerGroup
		}
		if sampleCount > len(sg.Files) {
			sampleCount = len(sg.Files)
		}

		// Выбираем файлы равномерно из подгруппы
		selectedFiles := selectDiverseFiles(sg.Files, sampleCount, root)

		// Строим метаданные с чанками для выбранных файлов (с кэшированием)
		var samples []types.FileMeta
		for _, f := range selectedFiles {
			fullPath := filepath.Join(root, f)
			meta := getCachedFileMeta(fullPath)
			meta.SampleChunks = getCachedChunks(fullPath)
			samples = append(samples, meta)
		}

		sg.Samples = samples
	}

	return subGroups
}

// selectDiverseFiles выбирает разнообразные файлы из списка.
func selectDiverseFiles(files []string, n int, root string) []string {
	if len(files) <= n {
		return files
	}

	// Сортируем файлы по имени для детерминированности
	sortedFiles := make([]string, len(files))
	copy(sortedFiles, files)
	sort.Strings(sortedFiles)

	// Разбиваем на бакеты по размеру
	bySize := make(map[string][]string)
	for _, f := range sortedFiles {
		path := filepath.Join(root, f)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		var bucket string
		switch {
		case info.Size() < 1024:
			bucket = "small"
		case info.Size() < 10240:
			bucket = "medium"
		default:
			bucket = "large"
		}
		bySize[bucket] = append(bySize[bucket], f)
	}

	var selected []string

	// Берём по одному файлу из каждого бакета (small/medium/large)
	for _, bucket := range []string{"small", "medium", "large"} {
		files := bySize[bucket]
		if len(files) > 0 && len(selected) < n {
			selected = append(selected, files[0])
		}
	}

	// Добираем случайными файлами
	for len(selected) < n {
		idx := rand.Intn(len(sortedFiles))
		f := sortedFiles[idx]
		already := false
		for _, s := range selected {
			if s == f {
				already = true
				break
			}
		}
		if !already {
			selected = append(selected, f)
		}
	}

	return selected
}

// maxLineLen — максимальная длина одной строки в чанке.
const maxLineLen = 200

// maxChunkLen — максимальный размер всего чанка в символах.
const maxChunkLen = 1000

// ExtractThreeChunks извлекает 3 чанка из файла: начало, середина, конец (по 10 строк каждый).
// Длинные строки обрезаются до maxLineLen, весь чанк — до maxChunkLen.
func ExtractThreeChunks(path string) []types.Chunk {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	lines := splitLines(string(content))
	total := len(lines)
	if total == 0 {
		return nil
	}

	chunkSize := 10
	var chunks []types.Chunk

	// Чанк 1: Начало (строки 1-10)
	startEnd := min(chunkSize, total)
	if startEnd > 0 {
		chunks = append(chunks, types.Chunk{
			Content:   buildChunkContent(lines[0:startEnd]),
			StartLine: 1,
			EndLine:   startEnd,
			Position:  "start",
		})
	}

	// Чанк 2: Середина (строки total/2 - 5 ... total/2 + 5)
	if total > chunkSize*2 {
		mid := total / 2
		midStart := max(0, mid-chunkSize/2)
		midEnd := min(total, mid+chunkSize/2)
		if midEnd > midStart {
			chunks = append(chunks, types.Chunk{
				Content:   buildChunkContent(lines[midStart:midEnd]),
				StartLine: midStart + 1,
				EndLine:   midEnd,
				Position:  "middle",
			})
		}
	}

	// Чанк 3: Конец (строки total-10 ... total)
	if total > chunkSize {
		endStart := max(0, total-chunkSize)
		if endStart < total {
			chunks = append(chunks, types.Chunk{
				Content:   buildChunkContent(lines[endStart:total]),
				StartLine: endStart + 1,
				EndLine:   total,
				Position:  "end",
			})
		}
	}

	// Если файл маленький (< 30 строк), возвращаем только один чанк
	if total <= chunkSize*2 && len(chunks) > 1 {
		return chunks[:1]
	}

	return chunks
}

// buildChunkContent формирует контент чанка с ограничением длины строк и общего размера.
func buildChunkContent(lines []string) string {
	var trimmed []string
	totalLen := 0

	for _, line := range lines {
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "..."
		}
		if totalLen+len(line)+1 > maxChunkLen {
			break
		}
		trimmed = append(trimmed, line)
		totalLen += len(line) + 1
	}

	return strings.Join(trimmed, "\n")
}

// splitLines разбивает текст на строки.
func splitLines(content string) []string {
	return strings.Split(content, "\n")
}

// joinLines объединяет строки в текст.
func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}

// SelectDiverseSamples (старая версия, оставлена для совместимости).
// Использует новую логику с подгруппами.
func SelectDiverseSamples(files []string, n int, root string) []types.FileMeta {
	if len(files) <= n {
		var metas []types.FileMeta
		for _, f := range files {
			fullPath := filepath.Join(root, f)
			meta := getCachedFileMeta(fullPath)
			meta.SampleChunks = getCachedChunks(fullPath)
			metas = append(metas, meta)
		}
		return metas
	}

	selected := selectDiverseFiles(files, n, root)

	var metas []types.FileMeta
	for _, f := range selected {
		fullPath := filepath.Join(root, f)
		meta := getCachedFileMeta(fullPath)
		meta.SampleChunks = getCachedChunks(fullPath)
		metas = append(metas, meta)
	}
	return metas
}

// getCachedFileMeta возвращает кэшированные метаданные файла или строит и кэширует их.
func getCachedFileMeta(path string) types.FileMeta {
	if cached, ok := fileMetaCache.Load(path); ok {
		return cached.(types.FileMeta)
	}
	meta := heuristic.BuildFileMeta(path)
	fileMetaCache.Store(path, meta)
	return meta
}

// getCachedChunks возвращает кэшированные чанки файла или строит и кэширует их.
func getCachedChunks(path string) []types.Chunk {
	if cached, ok := chunksCache.Load(path); ok {
		return cached.([]types.Chunk)
	}
	chunks := ExtractThreeChunks(path)
	chunksCache.Store(path, chunks)
	return chunks
}
