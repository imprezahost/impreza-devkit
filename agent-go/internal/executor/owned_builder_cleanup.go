package executor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Only acknowledged, fully receipted work is eligible. Inspect every path before
// deleting anything; never follow a cache symlink or recursively remove a root.
func (d *Docker) forgetOwnedPreparation(w *PreparationWork, id, dir string) error {
	result, err := d.preparationWorkResult(w, id)
	if err != nil {
		return err
	}
	if !result.Completed {
		return errors.New("owned preparation has no final worker receipt")
	}
	return withOwnedBuilderLock(dir, func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		var cachePaths []string
		for _, entry := range entries {
			if entry.Name() == "buildx" {
				cache := filepath.Join(dir, "buildx")
				if err = realWorkDirectory(cache); err != nil {
					return err
				}
				var total int64
				err = filepath.WalkDir(cache, func(path string, entry fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					rel, err := filepath.Rel(cache, path)
					if err != nil || strings.HasPrefix(rel, "..") || strings.Count(rel, string(os.PathSeparator)) > 8 {
						return errors.New("invalid builder cache path")
					}
					if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
						return errors.New("unsafe builder cache entry")
					}
					total += info.Size()
					if total > 1024*1024 || len(cachePaths) >= 128 {
						return errors.New("builder cache exceeds cleanup limits")
					}
					cachePaths = append(cachePaths, path)
					return nil
				})
				if err != nil {
					return err
				}
			} else if !slices.Contains([]string{"request.json", "started", "result.json", "builder.json", "builder.lock", "builder.apparmor", "builder.cid"}, entry.Name()) || !entry.Type().IsRegular() {
				return errors.New("unexpected owned preparation file; cleanup refused")
			}
		}
		for i := len(cachePaths) - 1; i >= 0; i-- {
			if err = os.Remove(cachePaths[i]); err != nil {
				return err
			}
		}
		for _, entry := range entries {
			if entry.Name() != "buildx" {
				if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					return err
				}
			}
		}
		if err = os.Remove(dir); err != nil {
			return err
		}
		return syncRecoveryDirectory(filepath.Dir(dir))
	})
}
