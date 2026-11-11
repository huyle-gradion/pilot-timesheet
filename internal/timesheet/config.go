package timesheet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxConfigBytes = 1 << 20

// LoadConfig reads the non-secret JSON configuration. It never reads
// credentials, never expands environment variables, and never prints values.
// Unknown fields are rejected so typos fail closed.
func LoadConfig(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, newError(CodeConfigInvalid, "config path is required")
	}
	data, err := readSmallFile(path, maxConfigBytes)
	if err != nil {
		return nil, newError(CodeConfigInvalid, "config file cannot be read")
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		if field, ok := unknownJSONField(err); ok {
			return nil, newError(CodeConfigInvalid, fmt.Sprintf("config: unknown field %q", field))
		}
		return nil, newError(CodeConfigInvalid, "config file is not valid JSON for the supported schema")
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, newError(CodeConfigInvalid, "config file has trailing data")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// A valid configuration that still lists a retired key (ADR 0012) is
	// reported to the command layer, which removes the key or says it is
	// ignored. The keys are never read either way.
	if RetiredConfigHook != nil && (cfg.RetiredProjects != nil || (cfg.Migration != nil && cfg.Migration.RetiredProjectMap != nil)) {
		RetiredConfigHook(path)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	c.Timezone = strings.TrimSpace(c.Timezone)
	if c.Timezone == "" {
		return newError(CodeConfigInvalid, "config: timezone is required")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return newError(CodeConfigInvalid, "config: timezone is not a recognised IANA timezone")
	}
	c.StatePath = strings.TrimSpace(c.StatePath)
	if c.StatePath == "" {
		return newError(CodeConfigInvalid, "config: state_path is required")
	}
	c.StatePath = filepath.Clean(c.StatePath)
	envSeen := map[string]bool{}
	for _, name := range c.RequiredEnv {
		if !envNameRE.MatchString(name) {
			return newError(CodeConfigInvalid, "config: required_env contains an invalid variable name")
		}
		if envSeen[name] {
			return newError(CodeConfigInvalid, "config: required_env contains a duplicate variable name")
		}
		envSeen[name] = true
	}
	if err := c.validateSources(); err != nil {
		return err
	}
	if err := c.validateWorkHours(); err != nil {
		return err
	}
	if err := c.validateJudge(); err != nil {
		return err
	}
	if err := c.validateDaily(); err != nil {
		return err
	}
	return c.validateR3()
}

// validWorkDayTokens is the exact accepted weekday vocabulary for work_hours.
// Nothing is normalized: "Mon", "monday" and "MON" are rejected by name.
var validWorkDayTokens = map[string]bool{
	"mon": true, "tue": true, "wed": true, "thu": true,
	"fri": true, "sat": true, "sun": true,
}

// maxWorkWindows bounds the day's main-work windows. Two or three windows cover
// a split day; more is a configuration error rather than a policy shape.
const maxWorkWindows = 4

// validateWorkHours checks the optional, non-blocking main-work-time policy.
// It is presentation-only: a valid policy annotates the draft and a nil one
// changes nothing. Every failure names the offending value.
func (c *Config) validateWorkHours() error {
	w := c.WorkHours
	if w == nil {
		return nil
	}
	if len(w.Days) == 0 {
		return newError(CodeConfigInvalid, "config: work_hours.days is required")
	}
	for _, day := range w.Days {
		if !validWorkDayTokens[day] {
			return newError(CodeConfigInvalid, fmt.Sprintf("config: work_hours.days contains an invalid day %q (use mon tue wed thu fri sat sun)", day))
		}
	}
	if len(w.Windows) == 0 {
		return newError(CodeConfigInvalid, "config: work_hours.windows is required")
	}
	if len(w.Windows) > maxWorkWindows {
		return newError(CodeConfigInvalid, fmt.Sprintf("config: work_hours.windows has %d windows; at most %d are allowed", len(w.Windows), maxWorkWindows))
	}
	starts := make([]int, len(w.Windows))
	ends := make([]int, len(w.Windows))
	for i, win := range w.Windows {
		startMin, err := parseClock(win.Start)
		if err != nil {
			return newError(CodeConfigInvalid, fmt.Sprintf("config: work_hours.windows[%d].start %q is not HH:MM", i, win.Start))
		}
		endMin, err := parseClock(win.End)
		if err != nil {
			return newError(CodeConfigInvalid, fmt.Sprintf("config: work_hours.windows[%d].end %q is not HH:MM", i, win.End))
		}
		if startMin >= endMin {
			return newError(CodeConfigInvalid, fmt.Sprintf("config: work_hours.windows[%d] start %q must be before end %q", i, win.Start, win.End))
		}
		starts[i], ends[i] = startMin, endMin
	}
	for i := range w.Windows {
		for j := i + 1; j < len(w.Windows); j++ {
			if starts[i] < ends[j] && starts[j] < ends[i] {
				return newError(CodeConfigInvalid, fmt.Sprintf("config: work_hours.windows[%d] %s-%s overlaps windows[%d] %s-%s",
					i, w.Windows[i].Start, w.Windows[i].End, j, w.Windows[j].Start, w.Windows[j].End))
			}
		}
	}
	return nil
}

// validateR3 checks the disabled-by-default R3 provisions: explicit legacy
// migration and the read-only Gradion boundary. Both are optional; omitting them
// reproduces exact R2 behavior. Endpoint and credential values are never echoed
// in a diagnostic.
func (c *Config) validateR3() error {
	if c.Gradion != nil && c.Gradion.DefaultClassification != nil {
		if err := c.Gradion.DefaultClassification.normalize(); err != nil {
			return err
		}
	}
	if c.Migration != nil {
		m := c.Migration
		if m.Enabled {
			m.LegacyRoot = strings.TrimSpace(m.LegacyRoot)
			if m.LegacyRoot == "" {
				return newError(CodeConfigInvalid, "config: migration.legacy_root is required when migration is enabled")
			}
			m.LegacyRoot = filepath.Clean(m.LegacyRoot)
			m.LegacyTimezone = strings.TrimSpace(m.LegacyTimezone)
			if m.LegacyTimezone == "" {
				return newError(CodeConfigInvalid, "config: migration.legacy_timezone is required when migration is enabled")
			}
			if _, err := time.LoadLocation(m.LegacyTimezone); err != nil {
				return newError(CodeConfigInvalid, "config: migration.legacy_timezone is not a recognised IANA timezone")
			}
		}
		for from, to := range m.ClassificationMap {
			if strings.TrimSpace(from) == "" || !identifierRE.MatchString(to) {
				return newError(CodeConfigInvalid, "config: migration.classification_map is invalid")
			}
		}
	}
	if c.Gradion != nil && c.Gradion.Enabled {
		c.Gradion.Endpoint = strings.TrimSpace(c.Gradion.Endpoint)
		u, err := url.Parse(c.Gradion.Endpoint)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return newError(CodeConfigInvalid, "config: gradion.endpoint must be an absolute http(s) URL")
		}
		c.Gradion.TokenEnv = strings.TrimSpace(c.Gradion.TokenEnv)
		if c.Gradion.TokenEnv == "" {
			c.Gradion.TokenEnv = DefaultGradionTokenEnv
		}
		if !envNameRE.MatchString(c.Gradion.TokenEnv) {
			return newError(CodeConfigInvalid, "config: gradion.token_env must be a variable name")
		}
	}
	return nil
}

// validateSources checks the non-secret evidence source configuration. It
// rejects unknown shapes, a missing GitHub identity or repository list, and
// malformed api_base values. Credential values never appear in configuration.
func (c *Config) validateSources() error {
	if c.Sources == nil {
		return nil
	}
	if c.Sources.Github == nil && c.Sources.Calendar == nil && !c.Sources.Enabled {
		return newError(CodeConfigInvalid, "config: sources is present but empty")
	}
	if c.Sources.Github != nil {
		g := c.Sources.Github
		if g.Enabled {
			if !githubSourceUserRE.MatchString(strings.TrimSpace(g.User)) {
				return newError(CodeConfigInvalid, "config: sources.github.user is invalid")
			}
			if len(g.Repositories) == 0 || len(g.Repositories) > 50 {
				return newError(CodeConfigInvalid, "config: sources.github.repositories must name between 1 and 50 repositories")
			}
			for _, repo := range g.Repositories {
				if !validRepositoryID(repo) {
					return newError(CodeConfigInvalid, "config: sources.github.repositories contains an invalid owner/name entry")
				}
			}
			if _, err := normalizeGithubBase(g.APIBase); err != nil {
				return err
			}
		}
	}
	return nil
}

func validRepositoryID(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}
	return githubRepoOwnerRE.MatchString(owner) && githubRepoNameRE.MatchString(name)
}

func normalizeGithubBase(base string) (*url.URL, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return url.Parse("https://api.github.com/")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, newError(CodeConfigInvalid, "config: sources.github.api_base must be an absolute http(s) URL")
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u, nil
}

// LoadEnvFile parses the operator-supplied environment file as data. Supported
// syntax is documented in docs/agents/timesheet-interface.md:
//
//   - blank lines and lines whose first non-space character is '#' are ignored
//   - KEY=VALUE assignments; keys match [A-Za-z_][A-Za-z0-9_]*
//   - unquoted values are trimmed and end at an inline " #" comment
//   - single-quoted values are literal
//   - double-quoted values support \\ \" \n \t and no other escapes
//
// Command substitution, variable expansion, sourcing, and undocumented dotenv
// features are not supported and are rejected. Errors report a line number only.
func LoadEnvFile(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, newError(CodeConfigInvalid, "env file path is required")
	}
	data, err := readSmallFile(path, maxConfigBytes)
	if err != nil {
		return nil, newError(CodeDotenvInvalid, "environment file cannot be read")
	}
	values := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimLeft(raw, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			return nil, dotenvLineError(lineNo, "'export' prefix is not supported")
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, dotenvLineError(lineNo, "expected KEY=VALUE assignment")
		}
		key := strings.TrimSpace(line[:eq])
		if !envNameRE.MatchString(key) {
			return nil, dotenvLineError(lineNo, "invalid variable name")
		}
		value, err := parseDotenvValue(strings.TrimSpace(line[eq+1:]))
		if err != nil {
			return nil, dotenvLineError(lineNo, err.Error())
		}
		values[key] = value
	}
	return values, nil
}

func dotenvLineError(line int, detail string) *AppError {
	return newError(CodeDotenvInvalid, fmt.Sprintf("environment file line %d: %s", line, detail))
}

func parseDotenvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '"':
		return parseDoubleQuoted(raw)
	case '\'':
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return "", errors.New("unterminated single-quoted value")
		}
		inner := raw[1 : len(raw)-1]
		if strings.ContainsRune(inner, '\'') {
			return "", errors.New("single-quoted value must not contain a quote")
		}
		if strings.ContainsAny(inner, "\r\n") {
			return "", errors.New("quoted value must be on one line")
		}
		return inner, nil
	default:
		for i := 0; i < len(raw); i++ {
			if raw[i] == '#' && i > 0 && (raw[i-1] == ' ' || raw[i-1] == '\t') {
				raw = raw[:i]
				break
			}
		}
		if strings.ContainsAny(raw, "\r\n") {
			return "", errors.New("value must be on one line")
		}
		return strings.TrimSpace(raw), nil
	}
}

func parseDoubleQuoted(raw string) (string, error) {
	if len(raw) < 2 || raw[len(raw)-1] != '"' {
		return "", errors.New("unterminated double-quoted value")
	}
	inner := raw[1 : len(raw)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if ch == '"' {
			return "", errors.New("unexpected quote in double-quoted value")
		}
		if ch != '\\' {
			b.WriteByte(ch)
			continue
		}
		i++
		if i >= len(inner) {
			return "", errors.New("invalid escape in double-quoted value")
		}
		switch inner[i] {
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		default:
			return "", errors.New("invalid escape in double-quoted value")
		}
	}
	return b.String(), nil
}

// MergeEnv applies inherited process values over file values, including
// explicitly empty values. Only keys from the file are copied here; required
// names absent from the file are read lazily by MissingRequiredEnv.
func MergeEnv(fileValues map[string]string) map[string]string {
	merged := make(map[string]string, len(fileValues))
	for k, v := range fileValues {
		if inherited, ok := os.LookupEnv(k); ok {
			merged[k] = inherited
			continue
		}
		merged[k] = v
	}
	return merged
}

// resolveEnv resolves one credential NAME from the merged environment with
// the same precedence R1 established: inherited process values override file
// values, including explicitly empty inherited overrides. Only the boolean
// presence decision is ever exported to callers; values are never logged.
func resolveEnv(merged map[string]string, name string) (string, bool) {
	if v, ok := merged[name]; ok {
		return v, true
	}
	return os.LookupEnv(name)
}

// MissingRequiredEnv returns required variable NAMES that are absent or empty
// after precedence has been applied. It never returns values.
func MissingRequiredEnv(cfg *Config, merged map[string]string) (missing, present []string) {
	for _, name := range cfg.RequiredEnv {
		value, ok := merged[name]
		if !ok {
			value, ok = os.LookupEnv(name)
		}
		if !ok || value == "" {
			missing = append(missing, name)
			continue
		}
		present = append(present, name)
	}
	return missing, present
}

func readSmallFile(path string, max int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, errors.New("not a readable file")
	}
	if info.Size() > max {
		return nil, errors.New("file is too large")
	}
	return os.ReadFile(path)
}

// unknownJSONField extracts the field name from encoding/json's unknown-field
// diagnostic, which DisallowUnknownFields emits as `json: unknown field "x"`.
// Only that diagnostic is surfaced; every other decoder error stays generic so
// no file content is echoed.
func unknownJSONField(err error) (string, bool) {
	const prefix = `json: unknown field `
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) {
		return "", false
	}
	field := strings.TrimSpace(strings.TrimPrefix(msg, prefix))
	field = strings.Trim(field, `"`)
	if field == "" {
		return "", false
	}
	return field, true
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("trailing value")
	}
	return err
}
