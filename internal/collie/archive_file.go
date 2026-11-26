package collie

import (
	"errors"
	"os"
	"path/filepath"
)

func replaceArchiveFile(path string, contents []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".collie-archive-*")
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return errors.Join(err, f.Close(), os.Remove(f.Name()))
	}
	if _, err := f.Write(contents); err != nil {
		return errors.Join(err, f.Close(), os.Remove(f.Name()))
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return errors.Join(err, os.Remove(f.Name()))
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return errors.Join(err, os.Remove(f.Name()))
	}
	return nil
}
