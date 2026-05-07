// Package testimages pins the container images used by integration tests
// across every service and pkg. One central set of tags keeps the whole
// repo on identical versions, so a Cassandra or Mongo schema/behaviour
// drift is caught everywhere at once rather than in whichever service
// happens to have the newest tag today.
//
// Versions here track docker-local/compose.yaml (the authoritative prod
// local-dev stack) where practical. One intentional divergence:
// Cassandra tests pin 4.1.3 because cassandra:5 with the
// testcontainers-go module's default MAX_HEAP_SIZE=1024M routinely OOMs
// on standard GitHub Actions runners.
//
// This package is only imported by integration tests (files gated by
// //go:build integration). Keep it dependency-free.
package testimages

const (
	// Cassandra is the image for every CQL-backed integration test.
	// See package doc for why this diverges from prod (cassandra:5).
	Cassandra = "cassandra:4.1.3"

	// Mongo is the image for every Mongo-backed integration test.
	Mongo = "mongo:8"

	// NATS is the image for every NATS-backed integration test
	// (core NATS + JetStream + WebSocket).
	NATS = "nats:2.11-alpine"

	// Node runs the TypeScript end-to-end clients in pkg/roomkeysender
	// and pkg/roomcrypto.
	Node = "node:20-alpine"

	// Elasticsearch is the search engine image for search-sync-worker.
	Elasticsearch = "elasticsearch:8.17.0"

	// Valkey is the Redis-compatible cache used by room-service and
	// pkg/roomkeystore.
	Valkey = "valkey/valkey:8"

	// MinIO is the image for every MinIO-backed integration test.
	MinIO = "minio/minio:RELEASE.2025-01-20T14-49-07Z"
)
