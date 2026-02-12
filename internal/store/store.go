package store

// Store is an abstract key-value storage interface backed by buckets.
// The initial implementation uses bbolt; the interface allows swapping
// to Badger, Pebble, SQLite, etc. without touching the rest of the codebase.
type Store interface {
	Get(bucket, key []byte) ([]byte, error)
	Set(bucket, key, value []byte) error
	Delete(bucket, key []byte) error
	ForEach(bucket []byte, fn func(key, value []byte) error) error
	Snapshot(bucket []byte) (map[string][]byte, error)
	Close() error
}
