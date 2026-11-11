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
	"strconv"
	"strings"
	"syscall"
)

// RetiredConfigHook, when set, is called by LoadConfig with the path of a
// configuration that still lists a retired key (ADR 0012). The command layer
// sets it to remove the keys from the file, or to say they are ignored. It is
// nil in the library, so loading never writes by itself.
var RetiredConfigHook func(path string)

// retiredConfigKeys are the retired keys, by the object that holds them. Like
// the configuration loader, matching ignores case.
var retiredConfigKeys = map[string][]string{
	"":          {"projects"},
	"migration": {"project_map"},
}

func isRetiredKey(object, key string) bool {
	for _, k := range retiredConfigKeys[object] {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// RemoveRetiredConfigKeys deletes the retired keys from the configuration
// file at path by a token-level edit: every other key keeps its order, values
// and formatting. The original bytes are first kept as a complete backup
// (path+".bak", or the next free .bak.N when that name holds anything else;
// see keepBackup), then the new bytes replace the file atomically (a
// temporary file in the same directory, the original mode, fsync, rename). It
// refuses a symlink, a hard-linked file and a file it cannot write, and
// changes nothing then. removed lists the names it deleted; an empty list
// means there was nothing it could find to remove.
func RemoveRetiredConfigKeys(path string) (removed []string, backup string, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return nil, "", fmt.Errorf("%s has other hard links", path)
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
	backup, err = keepBackup(path+".bak", data)
	if err != nil {
		return nil, "", err
	}
	if err := replaceFile(path, edited, info.Mode().Perm()); err != nil {
		return nil, "", err
	}
	return removed, backup, nil
}

// member is one object member's byte positions in its object: where its key
// starts and where its value ends.
type member struct {
	keyStart, valueEnd int
	key                string
}

// objectMembers lists the members of the JSON object obj, plus the offsets
// just after its '{' and of its '}'.
func objectMembers(obj []byte) ([]member, int, int, error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, 0, 0, errors.New("configuration is not a JSON object")
	}
	open := int(dec.InputOffset())
	var members []member
	for dec.More() {
		start := skipSpace(obj, int(dec.InputOffset()))
		if start < len(obj) && obj[start] == ',' {
			start = skipSpace(obj, start+1)
		}
		keyTok, err := dec.Token()
		if err != nil {
			return nil, 0, 0, err
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, 0, 0, err
		}
		members = append(members, member{keyStart: start, valueEnd: int(dec.InputOffset()), key: key})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, 0, 0, errors.New("configuration object is not closed")
	}
	return members, open, int(dec.InputOffset()) - 1, nil
}

// cutSpans returns the byte spans of obj (offset by base) that remove the
// members drop selects, with their separators. A run of removed members after
// a kept one is cut from that member's value end, taking the leading commas;
// a run at the start is cut up to the next kept key; removing every member
// cuts from after '{' to before '}', so no stray indentation is left.
func cutSpans(members []member, open, end, base int, drop func(member) bool) [][2]int {
	var spans [][2]int
	lastKept := -1
	for i := 0; i < len(members); {
		if !drop(members[i]) {
			lastKept = i
			i++
			continue
		}
		j := i
		for j < len(members) && drop(members[j]) {
			j++
		}
		switch {
		case lastKept >= 0:
			spans = append(spans, [2]int{base + members[lastKept].valueEnd, base + members[j-1].valueEnd})
		case j < len(members):
			spans = append(spans, [2]int{base + members[i].keyStart, base + members[j].keyStart})
		default:
			spans = append(spans, [2]int{base + open, base + end})
		}
		i = j
	}
	return spans
}

// withoutRetiredKeys returns data with the retired members cut out, and
// proves that nothing else changed: the edited document decodes to the
// original minus exactly those members.
func withoutRetiredKeys(data []byte) ([]byte, []string, error) {
	members, open, end, err := objectMembers(data)
	if err != nil {
		return nil, nil, err
	}
	var removed []string
	spans := cutSpans(members, open, end, 0, func(m member) bool { return isRetiredKey("", m.key) })
	for _, m := range members {
		if isRetiredKey("", m.key) {
			removed = append(removed, m.key)
			continue
		}
		if !strings.EqualFold(m.key, "migration") {
			continue
		}
		start, raw, ok := memberValue(data, m)
		if !ok || len(raw) == 0 || raw[0] != '{' {
			continue
		}
		nested, nOpen, nClose, err := objectMembers(raw)
		if err != nil {
			return nil, nil, err
		}
		spans = append(spans, cutSpans(nested, nOpen, nClose, start, func(n member) bool { return isRetiredKey("migration", n.key) })...)
		for _, n := range nested {
			if isRetiredKey("migration", n.key) {
				removed = append(removed, m.key+"."+n.key)
			}
		}
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

func memberValue(data []byte, m member) (int, []byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(data[m.keyStart:m.valueEnd]))
	var key string
	if err := dec.Decode(&key); err != nil {
		return 0, nil, false
	}
	i := skipSpace(data, m.keyStart+int(dec.InputOffset()))
	if i >= m.valueEnd || data[i] != ':' {
		return 0, nil, false
	}
	start := skipSpace(data, i+1)
	return start, data[start:m.valueEnd], true
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

func sameExceptRetired(before, after []byte) error {
	var a, b map[string]any
	if err := json.Unmarshal(before, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(after, &b); err != nil {
		return errors.New("the edited configuration is not valid JSON")
	}
	for key, value := range a {
		if isRetiredKey("", key) {
			delete(a, key)
			continue
		}
		if m, ok := value.(map[string]any); ok && strings.EqualFold(key, "migration") {
			for nested := range m {
				if isRetiredKey("migration", nested) {
					delete(m, nested)
				}
			}
		}
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("the edit would change more than the retired keys")
	}
	return nil
}

// keepBackup makes sure a complete copy of data exists at name, or at the
// first free name.N after it, and returns the name it used. The copy is
// written to a temporary file, synced and then hard-linked into place, so a
// backup name is either absent or complete. An existing name counts as the
// backup only when it is a regular file holding exactly data; anything else
// (an unrelated file, a stale backup, a dangling symlink) is left alone and
// the next name is tried.
func keepBackup(name string, data []byte) (string, error) {
	dir := filepath.Dir(name)
	tmp, err := os.CreateTemp(dir, ".timesheet-backup-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	for i := 0; i < 100; i++ {
		candidate := name
		if i > 0 {
			candidate = name + "." + strconv.Itoa(i)
		}
		err := os.Link(tmpName, candidate)
		if err == nil {
			syncDir(dir)
			return candidate, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if info, lerr := os.Lstat(candidate); lerr == nil && info.Mode().IsRegular() {
			if existing, rerr := os.ReadFile(candidate); rerr == nil && bytes.Equal(existing, data) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("no free backup name next to %s", name)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
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
	syncDir(filepath.Dir(path))
	return nil
}
