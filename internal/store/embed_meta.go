package store

// Файл реализует CRUD метаданных эмбеддингов коллекции (таблица embed_meta).
// Используется для отслеживания смены модели/размерности эмбеддингов
// и автоматического пересоздания Qdrant-коллекций.

import (
	"context"
	"database/sql"
	"time"
)

// EmbedMeta — метаданные коллекции эмбеддингов (модель + размерность).
type EmbedMeta struct {
	Collection string
	Model      string
	Dim        int
	UpdatedAt  time.Time
}

// GetEmbedMeta возвращает текущие метаданные коллекции, либо nil если запись отсутствует.
func (s *SQLite) GetEmbedMeta(ctx context.Context, collection string) (*EmbedMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT collection, model, dim, updated_at FROM embed_meta WHERE collection = ?`, collection)
	var m EmbedMeta
	var ts int64
	if err := row.Scan(&m.Collection, &m.Model, &m.Dim, &ts); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	m.UpdatedAt = time.Unix(ts, 0)
	return &m, nil
}

// SetEmbedMeta сохраняет/обновляет метаданные эмбеддингов коллекции.
func (s *SQLite) SetEmbedMeta(ctx context.Context, m EmbedMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO embed_meta(collection, model, dim, updated_at)
		 VALUES(?,?,?,?)
		 ON CONFLICT(collection) DO UPDATE SET
		   model=excluded.model,
		   dim=excluded.dim,
		   updated_at=excluded.updated_at`,
		m.Collection, m.Model, m.Dim, time.Now().Unix())
	return err
}
