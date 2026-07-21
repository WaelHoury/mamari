package mamari

import (
	"os"
	"path/filepath"
)

// writeFileAtomic replaces path only after a complete, synced write in the
// same directory. Long-running watch servers may be terminated at any point;
// writing the main index or a sidecar in place would otherwise leave a
// truncated file that the next process cannot load.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeStreamAtomic(path, mode, func(file *os.File) error {
		_, err := file.Write(data)
		return err
	})
}

func writeStreamAtomic(path string, mode os.FileMode, write func(*os.File) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = write(tmp); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
