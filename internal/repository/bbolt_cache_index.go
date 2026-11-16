package repository

import (
	"encoding/json"
	"fmt"
	"io"
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
	BlobKey    string    `json:"blob_key,omitempty"`
	PackDigest string    `json:"pack_digest,omitempty"`
	PackKey    string    `json:"pack_key,omitempty"`
	PackOffset int64     `json:"pack_offset,omitempty"`
	Aliases    []string  `json:"aliases,omitempty"`
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

// ExportSnapshot writes a consistent copy of the database to w.
func (i *BboltCacheIndex) ExportSnapshot(w io.Writer) error {
	return i.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(w)
		return err
	})
}

// ImportSnapshot merges a serialized bbolt snapshot into the live index.
func (i *BboltCacheIndex) ImportSnapshot(_ string, data []byte) error {
	tmp, err := os.CreateTemp("", "remotecache-snapshot-*.db")
	if err != nil {
		return fmt.Errorf("create cache index snapshot temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write cache index snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close cache index snapshot: %w", err)
	}
	src, err := bolt.Open(tmpPath, 0o600, &bolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open cache index snapshot: %w", err)
	}
	defer func() { _ = src.Close() }()
	return src.View(func(stx *bolt.Tx) error {
		return i.db.Update(func(dtx *bolt.Tx) error {
			return stx.ForEach(func(name []byte, sb *bolt.Bucket) error {
				db, err := dtx.CreateBucketIfNotExists(name)
				if err != nil {
					return err
				}
				return sb.ForEach(func(k, v []byte) error {
					return db.Put(k, v)
				})
			})
		})
	})
}

// Namespaces returns every namespace bucket in the index.
func (i *BboltCacheIndex) Namespaces() ([]string, error) {
	var out []string
	err := i.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			out = append(out, string(name))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
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
	for i := range entries {
		total += entries[i].Size
	}
	for i := range entries {
		e := &entries[i]
		expired := ttl > 0 && now.Sub(e.LastAccess) > ttl
		over := maxBytes > 0 && total > maxBytes
		if !expired && !over {
			continue
		}
		evicted = append(evicted, *e)
		total -= e.Size
	}
	for idx := range evicted {
		if err := i.Delete(namespace, evicted[idx].Digest); err != nil {
			return evicted, err
		}
	}
	return evicted, nil
}
