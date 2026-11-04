// Package practiceassets embeds immutable exercise versions. Tests are server-only.
package practiceassets

import "embed"

//go:embed */v1/*
var Files embed.FS
