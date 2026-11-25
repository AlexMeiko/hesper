package appsql

import _ "embed"

// InitSQL contains the bootstrap schema for the local SQLite app.db database.
//
//go:embed 001_init.sql
var InitSQL string
