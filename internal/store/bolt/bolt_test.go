package bolt

import (
	"os"
	"path/filepath"
	"testing"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var testBucket = []byte("test-bucket")

func TestOpenClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// File should exist
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file should exist: %v", err)
	}
}

func TestOpenInvalidPath(t *testing.T) {
	_, err := Open("/nonexistent/dir/test.db")
	if err == nil {
		t.Fatal("opening db in nonexistent dir should fail")
	}
}

func TestSetAndGet(t *testing.T) {
	s := tempStore(t)

	if err := s.Set(testBucket, []byte("key1"), []byte("val1")); err != nil {
		t.Fatal(err)
	}

	val, err := s.Get(testBucket, []byte("key1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "val1" {
		t.Fatalf("expected val1, got %q", val)
	}
}

func TestGetNonexistentBucket(t *testing.T) {
	s := tempStore(t)
	val, err := s.Get([]byte("no-bucket"), []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if val != nil {
		t.Fatalf("expected nil for nonexistent bucket, got %q", val)
	}
}

func TestGetNonexistentKey(t *testing.T) {
	s := tempStore(t)
	if err := s.Set(testBucket, []byte("other"), []byte("val")); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get(testBucket, []byte("missing"))
	if err != nil {
		t.Fatal(err)
	}
	if val != nil {
		t.Fatalf("expected nil for missing key, got %q", val)
	}
}

func TestSetOverwrite(t *testing.T) {
	s := tempStore(t)
	if err := s.Set(testBucket, []byte("k"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(testBucket, []byte("k"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get(testBucket, []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "v2" {
		t.Fatalf("expected v2 after overwrite, got %q", val)
	}
}

func TestDelete(t *testing.T) {
	s := tempStore(t)
	if err := s.Set(testBucket, []byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(testBucket, []byte("k")); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get(testBucket, []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if val != nil {
		t.Fatalf("expected nil after delete, got %q", val)
	}
}

func TestDeleteNonexistentBucket(t *testing.T) {
	s := tempStore(t)
	// Should not error when bucket doesn't exist
	if err := s.Delete([]byte("no-bucket"), []byte("key")); err != nil {
		t.Fatal(err)
	}
}

func TestForEach(t *testing.T) {
	s := tempStore(t)
	keys := []string{"a", "b", "c"}
	for _, k := range keys {
		if err := s.Set(testBucket, []byte(k), []byte("val-"+k)); err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]string)
	err := s.ForEach(testBucket, func(k, v []byte) error {
		seen[string(k)] = string(v)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(seen))
	}
	for _, k := range keys {
		if seen[k] != "val-"+k {
			t.Fatalf("expected val-%s, got %q", k, seen[k])
		}
	}
}

func TestForEachNonexistentBucket(t *testing.T) {
	s := tempStore(t)
	count := 0
	err := s.ForEach([]byte("no-bucket"), func(k, v []byte) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("iterating empty/nonexistent bucket should yield 0 entries")
	}
}

func TestSnapshot(t *testing.T) {
	s := tempStore(t)
	if err := s.Set(testBucket, []byte("x"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(testBucket, []byte("y"), []byte("2")); err != nil {
		t.Fatal(err)
	}

	snap, err := s.Snapshot(testBucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	if string(snap["x"]) != "1" || string(snap["y"]) != "2" {
		t.Fatalf("unexpected snapshot content: %v", snap)
	}
}

func TestSnapshotNonexistentBucket(t *testing.T) {
	s := tempStore(t)
	snap, err := s.Snapshot([]byte("no-bucket"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot, got %d entries", len(snap))
	}
}

func TestSnapshotReturnsCopy(t *testing.T) {
	s := tempStore(t)
	if err := s.Set(testBucket, []byte("k"), []byte("original")); err != nil {
		t.Fatal(err)
	}

	snap, _ := s.Snapshot(testBucket)
	// Mutating the snapshot should not affect the store
	snap["k"][0] = 'X'

	val, _ := s.Get(testBucket, []byte("k"))
	if string(val) != "original" {
		t.Fatal("snapshot mutation should not affect store")
	}
}

func TestMultipleBuckets(t *testing.T) {
	s := tempStore(t)
	b1 := []byte("bucket1")
	b2 := []byte("bucket2")

	if err := s.Set(b1, []byte("k"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(b2, []byte("k"), []byte("v2")); err != nil {
		t.Fatal(err)
	}

	v1, _ := s.Get(b1, []byte("k"))
	v2, _ := s.Get(b2, []byte("k"))
	if string(v1) != "v1" || string(v2) != "v2" {
		t.Fatal("buckets should be isolated")
	}
}
