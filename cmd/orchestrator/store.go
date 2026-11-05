package main

import (
	"jellyfinner/internal/config"
	"jellyfinner/internal/database"
	"jellyfinner/internal/database/sqlite"
)

// openStore is the one place a command opens the database, so every command
// gets the same backend and the same retry and budget policy.
func openStore(cfg *config.Config) (database.Store, error) {
	return database.Open(sqlite.Driver, cfg.DBPath, database.Options{
		RetryDelay:     cfg.RetryDelay,
		BackfillBudget: cfg.BackfillDailyBytes,
	})
}
