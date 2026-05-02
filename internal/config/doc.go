// Package config loads and validates the typed TOML configuration. Path
// values are validated against an allowlist of prefixes; subprocess argv
// lists are built from typed values rather than via shell (§8).
package config
