package bolt

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Store implements store.Store using bbolt (embedded B+ tree).
type Store struct {
	db *bolt.DB
}

// Open creates or opens a bbolt database at the given path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("opening bolt db: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Get(bucket, key []byte) ([]byte, error) {
	var val []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v != nil {
			val = make([]byte, len(v))
			copy(val, v)
		}
		return nil
	})
	return val, err
}

func (s *Store) Set(bucket, key, value []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return fmt.Errorf("creating bucket: %w", err)
		}
		return b.Put(key, value)
	})
}

func (s *Store) Delete(bucket, key []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		return b.Delete(key)
	})
}

func (s *Store) ForEach(bucket []byte, fn func(key, value []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		return b.ForEach(fn)
	})
}

func (s *Store) Snapshot(bucket []byte) (map[string][]byte, error) {
	result := make(map[string][]byte)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			val := make([]byte, len(v))
			copy(val, v)
			result[string(k)] = val
			return nil
		})
	})
	return result, err
}

func (s *Store) Close() error {
	return s.db.Close()
}
