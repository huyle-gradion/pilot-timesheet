package main

// No-replace publication and the present-path report. Each file is staged in a
// private 0600 temporary file inside the target directory, fsynced, closed, and
// then hard-linked to its final name. os.Link fails with EEXIST instead of
// clobbering an existing final path, so a concurrent setup loses cleanly and a
// crash cannot leave a half-written final file: the final path holds either
// fully-synced content or nothing. The temporary file is always removed after
// the attempt; no other path is ever deleted.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const setupTempMarker = ".tmp-"

// setupTargetExists reports whether a final path exists, without following a
// symlink. A symlink or non-file at a final path is still "present": setup
// never replaces it, and the report names what it is.
func setupTargetExists(path string) (bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("init: cannot inspect %s: %v", path, err)
	}
	return true, nil
}

// publishSetupFile writes data to a private temporary file in dir, fsyncs and
// closes it, then hard-links it to the final leaf with no replace. A false
// return with nil error means another writer won the final path (EEXIST),
// which is reported as present.
func publishSetupFile(dir, leaf string, data []byte) (bool, error) {
	tmp := filepath.Join(dir, fmt.Sprintf(".%s%s%d-%d", leaf, setupTempMarker, os.Getpid(), time.Now().UnixNano()))
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("init: cannot stage %s: %v", filepath.Join(dir, leaf), err)
	}
	discard := func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}
	if _, err := file.Write(data); err != nil {
		discard()
		return false, fmt.Errorf("init: cannot stage %s: %v", filepath.Join(dir, leaf), err)
	}
	if err := file.Sync(); err != nil {
		discard()
		return false, fmt.Errorf("init: cannot stage %s: %v", filepath.Join(dir, leaf), err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("init: cannot stage %s: %v", filepath.Join(dir, leaf), err)
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, filepath.Join(dir, leaf)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("init: cannot publish %s: %v", filepath.Join(dir, leaf), err)
	}
	return true, nil
}

func presentSetupFile(path string, isConfig bool) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("init: %s disappeared while reporting; nothing was replaced", path)
	}
	if err != nil {
		return "", fmt.Errorf("init: cannot inspect %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Sprintf("present %s (symlink; not followed; left untouched)", path), nil
	}
	if info.IsDir() {
		return fmt.Sprintf("present %s (a directory, not a file; not modified)", path), nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("present %s (not a regular file; not modified)", path), nil
	}
	mode := info.Mode().Perm()
	unsafe := ""
	if mode&0o077 != 0 {
		unsafe = "unsafe: group/other permissions; "
	}
	if !isConfig {
		return fmt.Sprintf("present %s (mode %04o; %spresent, not read; left byte-identical)", path, mode, unsafe), nil
	}
	if _, loadErr := timesheet.LoadConfig(path); loadErr == nil {
		return fmt.Sprintf("present %s (mode %04o; %sloads OK; left byte-identical)", path, mode, unsafe), nil
	} else {
		return fmt.Sprintf("present %s (mode %04o; %sexists but does not load: %q; left byte-identical; fix it yourself — setup never deletes or replaces it and never claims ownership of it)", path, mode, unsafe, loadErr.Error()), nil
	}
}
