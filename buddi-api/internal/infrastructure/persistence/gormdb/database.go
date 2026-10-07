// Package gormdb provides the GORM backed implementation of the persistence
// contracts: a pooled database handle, a transaction manager, and a generic
// base repository that concrete repositories embed.
package gormdb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// Config tunes the connection pool and GORM behaviour.
type Config struct {
	DSN string

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration

	LogLevel         gormlogger.LogLevel
	LogSlowThreshold time.Duration
	PrepareStmt      bool
}

// DefaultConfig returns a Config with sensible pool defaults. DSN is required.
func DefaultConfig(dsn string) Config {
	return Config{
		DSN:              dsn,
		MaxOpenConns:     20,
		MaxIdleConns:     5,
		ConnMaxLifetime:  time.Hour,
		ConnMaxIdleTime:  30 * time.Minute,
		LogLevel:         gormlogger.Warn,
		LogSlowThreshold: 200 * time.Millisecond,
		PrepareStmt:      false,
	}
}

type Database struct {
	config Config
	db     *gorm.DB
}

var _ persistence.Database = (*Database)(nil)

// New returns an unconnected Database.
func New(config Config) *Database {
	return &Database{config: config}
}

// Connect opens the pool and verifies connectivity.
func (d *Database) Connect(ctx context.Context) error {
	if d.db != nil {
		return nil
	}

	if d.config.DSN == "" {
		return errors.New("gormdb: dsn is required")
	}

	gormConfig := &gorm.Config{
		Logger:                 gormlogger.New(log.New(os.Stderr, "", log.LstdFlags), gormlogger.Config{SlowThreshold: d.config.LogSlowThreshold, LogLevel: d.config.LogLevel}),
		TranslateError:         true,
		NowFunc:                func() time.Time { return time.Now().UTC() },
		PrepareStmt:            d.config.PrepareStmt,
		SkipDefaultTransaction: true,
	}

	db, err := gorm.Open(postgres.Open(d.config.DSN), gormConfig)
	if err != nil {
		return fmt.Errorf("gormdb: open: %w", err)
	}

	pool, err := db.DB()
	if err != nil {
		return fmt.Errorf("gormdb: resolve pool: %w", err)
	}

	pool.SetMaxOpenConns(d.config.MaxOpenConns)
	pool.SetMaxIdleConns(d.config.MaxIdleConns)
	pool.SetConnMaxLifetime(d.config.ConnMaxLifetime)
	pool.SetConnMaxIdleTime(d.config.ConnMaxIdleTime)

	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return fmt.Errorf("gormdb: ping: %w", err)
	}

	d.db = db
	return nil
}

// Close releases the pool.
func (d *Database) Close() error {
	if d.db == nil {
		return nil
	}

	pool, err := d.db.DB()
	if err != nil {
		return fmt.Errorf("gormdb: resolve pool: %w", err)
	}

	d.db = nil
	return pool.Close()
}

// Ping verifies the pool can reach the database.
func (d *Database) Ping(ctx context.Context) error {
	if d.db == nil {
		return errors.New("gormdb: not connected")
	}

	pool, err := d.db.DB()
	if err != nil {
		return fmt.Errorf("gormdb: resolve pool: %w", err)
	}

	return pool.PingContext(ctx)
}

// Conn returns the root handle. Prefer Repository.Conn, which also honours a
// transaction carried on the context.
func (d *Database) Conn() *gorm.DB {
	return d.db
}
