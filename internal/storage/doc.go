// Package storage provides implementations of the repository interfaces
// declared in internal/ports.
//
// OpenSQLite (sqlite*.go) is the default, persistent store: one embedded
// database file, opened through a pure-Go driver. NewInMemory (memory*.go)
// keeps everything in process memory and is used by tests and by
// storage.backend: memory. Both are held to the same semantics by
// conformance_test.go, and callers only ever see the ports.* interfaces.
//
// No database server is used or assumed anywhere in this package.
package storage
