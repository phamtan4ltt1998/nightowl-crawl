// Package db manages the MySQL connection pool for the fetcher service.
package db

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"
)

var pool *sql.DB

// Init opens and pings the MySQL connection pool.
func Init() error {
	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		env("DB_USER", "nightowl"),
		env("DB_PASSWORD", "nightowl"),
		env("DB_HOST", "localhost"),
		env("DB_PORT", "3306"),
		env("DB_NAME", "nightowl"),
	)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(3)

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping mysql: %w", err)
	}
	pool = db
	return nil
}

// DB returns the shared connection pool. Panics if Init has not been called.
func DB() *sql.DB {
	if pool == nil {
		panic("db.Init() not called")
	}
	return pool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
