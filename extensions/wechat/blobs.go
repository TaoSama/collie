package wechat

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// blobDir keeps byte content by its sha256 next to a JSON index that
// references it.
type blobDir struct{ dir string }

func (b blobDir) path(sum string) string { return filepath.Join(b.dir, sum) }

// put stores data and answers its sha256.
func (b blobDir) put(data []byte) (string, error) {
	h := sha256.Sum256(data)
	sum := hex.EncodeToString(h[:])
	if _, err := os.Stat(b.path(sum)); err == nil {
		return sum, nil
	}
	if err := os.MkdirAll(b.dir, privateDirMode); err != nil {
		return "", err
	}
	return sum, writePrivateFile(b.path(sum), data)
}

func (b blobDir) get(sum string) ([]byte, error) {
	data, err := os.ReadFile(b.path(sum))
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != sum {
		return nil, errors.New("stored attachment changed on disk")
	}
	return data, nil
}

// sweep removes every blob not in keep.
func (b blobDir) sweep(keep map[string]bool) error {
	entries, err := os.ReadDir(b.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(b.dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
