// Package stores provides metadata store registry management.
//
// The Service manages named metadata store instances (memory, BadgerDB,
// PostgreSQL) that are referenced by shares. It provides thread-safe
// registration, lookup, and lifecycle management of store instances.
package stores
