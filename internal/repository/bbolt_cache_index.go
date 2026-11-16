package repository

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// CacheIndexEntry is one remote-cache index record.
type CacheIndexEntry struct {
	Digest     string    `json:"digest"`
	Root       string    `json:"root,omitempty"`
	BundleKey  string    `json:"bundle_key"`
	Size       int64     `json:"size"`
	CreatedAt  time.Time `json:"created_at"`
	LastAccess time.Time `json:"last_access"`
}

// BboltCacheIndex is the rebuildable local remote-cache lookup index. It is
// partitioned by cache namespace (org id or "_shared").
type BboltCacheIndex struct {
	db *bolt.DB
}

// OpenBboltCacheIndex opens (creating if needed) the index at path.
func OpenBboltCacheIndex(path string) (*BboltCacheIndex, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create cache index dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second, NoSync: true})
	if err != nil {
		return nil, fmt.Errorf("open cache index: %w", err)
	}
	return &BboltCacheIndex{db: db}, nil
}

// Close closes the underlying database.
func (i *BboltCacheIndex) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	return i.db.Close()
}

func (i *BboltCacheIndex) bucket(tx *bolt.Tx, namespace string) (*bolt.Bucket, error) {
	return tx.CreateBucketIfNotExists([]byte(namespace))
}

// Put stores an entry in namespace.
func (i *BboltCacheIndex) Put(namespace string, e *CacheIndexEntry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.LastAccess.IsZero() {
		e.LastAccess = e.CreatedAt
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal cache index entry: %w", err)
	}
	return i.db.Update(func(tx *bolt.Tx) error {
		b, err := i.bucket(tx, namespace)
		if err != nil {
			return err
		}
		return b.Put([]byte(e.Digest), data)
	})
}

// Get returns an entry from namespace.
func (i *BboltCacheIndex) Get(namespace, digest string) (*CacheIndexEntry, bool, error) {
	var out *CacheIndexEntry
	err := i.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespace))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(digest))
		if raw == nil {
			return nil
		}
		var e CacheIndexEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("decode cache index entry: %w", err)
		}
		out = &e
		return nil
	})
	return out, out != nil, err
}

// Touch updates LastAccess for an entry.
func (i *BboltCacheIndex) Touch(namespace, digest string, at time.Time) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespace))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(digest))
		if raw == nil {
			return nil
		}
		var e CacheIndexEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		e.LastAccess = at
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put([]byte(digest), data)
	})
}

// Delete removes an entry.
func (i *BboltCacheIndex) Delete(namespace, digest string) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespace))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(digest))
	})
}

// DeleteNamespace removes every entry in namespace.
func (i *BboltCacheIndex) DeleteNamespace(namespace string) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(namespace)) == nil {
			return nil
		}
		return tx.DeleteBucket([]byte(namespace))
	})
}

// Stats returns the entry count and total bytes in namespace.
func (i *BboltCacheIndex) Stats(namespace string) (count int, bytes int64, err error) {
	err = i.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespace))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e CacheIndexEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return nil
			}
			count++
			bytes += e.Size
			return nil
		})
	})
	return count, bytes, err
}

// GC evicts entries older than ttl or beyond maxBytes (oldest first). It
// returns the evicted entries so callers can delete their S3 objects.
func (i *BboltCacheIndex) GC(namespace string, ttl time.Duration, maxBytes int64, now time.Time) ([]CacheIndexEntry, error) {
	var entries []CacheIndexEntry
	if err := i.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespace))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e CacheIndexEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return nil
			}
			entries = append(entries, e)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].LastAccess.Before(entries[b].LastAccess) })
	var evicted []CacheIndexEntry
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	for _, e := range entries {
		expired := ttl > 0 && now.Sub(e.LastAccess) > ttl
		over := maxBytes > 0 && total > maxBytes
		if !expired && !over {
			continue
		}
		evicted = append(evicted, e)
		total -= e.Size
	}
	for _, e := range evicted {
		if err := i.Delete(namespace, e.Digest); err != nil {
			return evicted, err
		}
	}
	return evicted, nil
}
