package watcher

import (
	"io/fs"
	"os"
)

// fileInfo возвращает информацию о файле.
// использует os.Lstat вместо os.Stat, чтобы не следовать за symlink'ами
// (иначе watcher может выйти за пределы root через symlink на внешнюю директорию).
func fileInfo(path string) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return info, nil
}
