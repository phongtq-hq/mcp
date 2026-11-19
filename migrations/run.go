package migrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
)

// NewProvider trả về goose provider trên các migration đã nhúng.
// db phải là kết nối có quyền DDL, không phải tài khoản của process ứng dụng.
func NewProvider(db *sql.DB) (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, db, FS)
	if err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return p, nil
}

// Up chạy mọi migration chưa áp dụng.
func Up(ctx context.Context, db *sql.DB) error {
	p, err := NewProvider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrations up: %w", err)
	}
	return nil
}
