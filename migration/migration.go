// Package migration содержит схему БД.
package migration

import _ "embed"

// InitUp — начальная схема. Идемпотентна: её можно применять при каждом старте.
//
//go:embed init.up.sql
var InitUp string
