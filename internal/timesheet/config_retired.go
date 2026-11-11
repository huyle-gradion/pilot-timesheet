package timesheet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
)

// RetiredConfigHook, when set, is called by LoadConfig with the path of a
// configuration that still lists a retired key (ADR 0012). The command layer
// sets it to remove the keys from the file, or to say they are ignored. It is
// nil in the library, so loading never writes by itself.
var RetiredConfigHook func(path string)

// retiredConfigKeys are the retired keys, by the object that holds them.
var retiredConfigKeys = map[string][]string{
	"":          {"projects"},
	"migration": {"project_map"},
}

// RemoveRetiredConfigKeys deletes the retired keys from the configuration
// file at path by a token-level edit: every other key keeps its order, values
// and formatting. The original bytes are kept once in path+".bak" (0600, never
// overwritten), then the new bytes replace the file atomically (a temporary
// file in the same directory, the original mode, fsync, rename). It refuses a
// symlink and a file it cannot write, and changes nothing then. removed lists
// the dotted names it deleted; an empty list means there was nothing to do.
func RemoveRetiredConfigKeys(path string) (removed []string, backup string, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", path)
	}
	// 2 is W_OK: the file and its directory must both be writable.
	if err := syscall.Access(path, 2); err != nil {
		return nil, "", fmt.Errorf("%s is not writable", path)
	}
	dir := filepath.Dir(path)
	if err := syscall.Access(dir, 2); err != nil {
		return nil, "", fmt.Errorf("%s is not writable", dir)
	}
	data, err := readSmallFile(path, maxConfigBytes)
	if err != nil {
		return nil, "", err
	}
	edited, removed, err := withoutRetiredKeys(data)
	if err != nil || len(removed) == 0 {
		return nil, "", err
	}
	backup = path + ".bak"
	if err := writeOnce(backup, data); err != nil {
		return nil, "", err
	}
	if err := replaceFile(path, edited, info.Mode().Perm()); err != nil {
		return nil, "", err
	}
	return removed, backup, nil
}

// withoutRetiredKeys returns data with the retired members cut out, and
// proves that nothing else changed: the edited document decodes to the
// original minus exactly those members.
func withoutRetiredKeys(data []byte) ([]byte, []string, error) {
	spans, removed, err := memberSpans(data, 0, retiredConfigKeys[""], "")
	if err != nil {
		return nil, nil, err
	}
	if start, raw, ok := migrationValue(data); ok && len(raw) > 0 && raw[0] == '{' {
		nested, names, err := memberSpans(raw, start, retiredConfigKeys["migration"], "migration.")
		if err != nil {
			return nil, nil, err
		}
		spans = append(spans, nested...)
		removed = append(removed, names...)
	}
	if len(spans) == 0 {
		return data, nil, nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] > spans[j][0] })
	out := append([]byte(nil), data...)
	for _, s := range spans {
		out = append(out[:s[0]], out[s[1]:]...)
	}
	if err := sameExceptRetired(data, out); err != nil {
		return nil, nil, err
	}
	return out, removed, nil
}

// migrationValue is the top-level "migration" value and its byte offset.
func migrationValue(data []byte) (int, []byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, nil, false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, nil, false
		}
		if key == "migration" {
			return int(dec.InputOffset()) - len(raw), raw, true
		}
	}
	return 0, nil, false
}

// memberSpans finds, in the JSON object obj (which starts at base in the whole
// document), the byte span of each member named in targets. A span that is
// not the object's first member starts right after the previous value, so it
// takes its own leading comma. A first member's span starts at its key and
// runs through the following comma and whitespace, so the next member moves
// into its place.
func memberSpans(obj []byte, base int, targets []string, prefix string) ([][2]int, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, errors.New("configuration is not a JSON object")
	}
	var spans [][2]int
	var names []string
	first := true
	for dec.More() {
		before := int(dec.InputOffset())
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		end := int(dec.InputOffset())
		for _, target := range targets {
			if key != target {
				continue
			}
			start := before
			if first {
				start = skipSpace(obj, before)
				end = skipSpace(obj, end)
				if end < len(obj) && obj[end] == ',' {
					end = skipSpace(obj, end+1)
				}
			}
			spans = append(spans, [2]int{base + start, base + end})
			names = append(names, prefix+key)
		}
		first = false
	}
	return spans, names, nil
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// sameExceptRetired proves the edit removed only the retired members.
func sameExceptRetired(before, after []byte) error {
	var a, b map[string]any
	if err := json.Unmarshal(before, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(after, &b); err != nil {
		return errors.New("the edited configuration is not valid JSON")
	}
	for _, key := range retiredConfigKeys[""] {
		delete(a, key)
	}
	if m, ok := a["migration"].(map[string]any); ok {
		for _, key := range retiredConfigKeys["migration"] {
			delete(m, key)
		}
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("the edit would change more than the retired keys")
	}
	return nil
}

// writeOnce creates path with data at mode 0600 unless it already exists,
// in which case the existing file is kept as it is.
func writeOnce(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// replaceFile atomically replaces path with data at mode perm.
func replaceFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".timesheet-config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
