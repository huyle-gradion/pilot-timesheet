package timesheet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWorkHoursConfig writes one minimal valid configuration with the given
// work_hours JSON object spliced in verbatim (an empty string omits the block).
func writeWorkHoursConfig(t *testing.T, workHours string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "timesheet.json")
	body := fmt.Sprintf(`{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": %q,
  "projects": [{"id": "WORK", "name": "Work"}]`, filepath.Join(dir, "state.json"))
	if workHours != "" {
		body += ",\n  \"work_hours\": " + workHours
	}
	body += "\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigWorkHoursValid(t *testing.T) {
	path := writeWorkHoursConfig(t, `{"days": ["mon", "tue", "wed", "thu", "fri"], "windows": [{"start": "09:00", "end": "12:00"}, {"start": "13:00", "end": "18:00"}]}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig = %v, want a valid work-hours policy", err)
	}
	if cfg.WorkHours == nil || len(cfg.WorkHours.Days) != 5 || len(cfg.WorkHours.Windows) != 2 {
		t.Fatalf("work_hours = %+v, want 5 days and 2 windows", cfg.WorkHours)
	}
	if cfg.WorkHours.Windows[1].Start != "13:00" || cfg.WorkHours.Windows[1].End != "18:00" {
		t.Fatalf("second window = %+v, want 13:00-18:00 verbatim", cfg.WorkHours.Windows[1])
	}
}

func TestLoadConfigWorkHoursRejected(t *testing.T) {
	valid := `{"start": "09:00", "end": "12:00"}`
	cases := []struct {
		name      string
		workHours string
		want      string
	}{
		{"empty days", `{"days": [], "windows": [` + valid + `]}`, "work_hours.days is required"},
		{"bad day token", `{"days": ["Mon"], "windows": [` + valid + `]}`, `"Mon"`},
		{"long day token", `{"days": ["monday"], "windows": [` + valid + `]}`, `"monday"`},
		{"empty windows", `{"days": ["mon"], "windows": []}`, "work_hours.windows is required"},
		{"too many windows", `{"days": ["mon"], "windows": [
			{"start": "06:00", "end": "07:00"},
			{"start": "08:00", "end": "09:00"},
			{"start": "10:00", "end": "11:00"},
			{"start": "12:00", "end": "13:00"},
			{"start": "14:00", "end": "15:00"}]}`, "at most 4"},
		{"bad start form", `{"days": ["mon"], "windows": [{"start": "9:00", "end": "12:00"}]}`, `"9:00"`},
		{"signed hour passes Atoi", `{"days": ["mon"], "windows": [{"start": "+1:00", "end": "12:00"}]}`, `"+1:00"`},
		{"signed minute passes Atoi", `{"days": ["mon"], "windows": [{"start": "09:+5", "end": "12:00"}]}`, `"09:+5"`},
		{"bad end range", `{"days": ["mon"], "windows": [{"start": "09:00", "end": "25:00"}]}`, `"25:00"`},
		{"start equals end", `{"days": ["mon"], "windows": [{"start": "09:00", "end": "09:00"}]}`, "must be before end"},
		{"start after end", `{"days": ["mon"], "windows": [{"start": "12:00", "end": "09:00"}]}`, "must be before end"},
		{"overlapping windows", `{"days": ["mon"], "windows": [
			{"start": "09:00", "end": "12:00"},
			{"start": "11:00", "end": "13:00"}]}`, "overlaps"},
		{"unknown work_hours key", `{"days": ["mon"], "windows": [` + valid + `], "max_block_minutes": 300}`, `"max_block_minutes"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeWorkHoursConfig(t, tc.workHours)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig accepted %s", tc.name)
			}
			app, ok := err.(*AppError)
			if !ok || app.Code != CodeConfigInvalid {
				t.Fatalf("error = %v, want code %s", err, CodeConfigInvalid)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
