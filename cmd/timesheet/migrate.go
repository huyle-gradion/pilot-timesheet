package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// maxMigrateRangeDays caps how far one FROM..TO value expands before the
// migration's own date bound judges the selection, so a typo cannot build a
// huge list.
const maxMigrateRangeDays = 366

func runMigrate(args []string) error {
	if len(args) == 0 || (args[0] != "preview" && args[0] != "apply") {
		return fmt.Errorf("migrate takes a subcommand: `timesheet migrate preview --date <day>` or `timesheet migrate apply --date <day> --migration-id <id>`")
	}
	verb, args := args[0], args[1:]
	name := "migrate " + verb
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var configPath, envFile, migrationID string
	var dates dateList
	flags.StringVar(&configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.Var(&dates, "date", "legacy day YYYY-MM-DD or inclusive range FROM..TO (required; repeatable)")
	if verb == "apply" {
		flags.StringVar(&migrationID, "migration-id", "", "migration_id copied verbatim from migrate preview for the same dates (required)")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s does not accept positional arguments", name)
	}
	if len(dates) == 0 {
		return fmt.Errorf("%s requires --date", name)
	}
	if verb == "apply" && strings.TrimSpace(migrationID) == "" {
		return fmt.Errorf("migrate apply requires --migration-id from `timesheet migrate preview` for the same dates")
	}
	configPath, envFile, err := resolveStoreFlags(name, configPath, envFile)
	if err != nil {
		return err
	}
	h, err := openHeadless(configPath, envFile)
	if err != nil {
		return err
	}
	defer h.done()
	var res any
	if verb == "preview" {
		res, err = h.call("migration_preview", timesheet.Request{Dates: dates})
	} else {
		res, err = h.call("migration_apply", timesheet.Request{Dates: dates, MigrationID: migrationID})
	}
	if err != nil {
		return err
	}
	return printJSON(res)
}

type dateList []string

func (d *dateList) String() string { return strings.Join(*d, ",") }

func (d *dateList) Set(v string) error {
	from, to, isRange := strings.Cut(v, "..")
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return fmt.Errorf("--date must be YYYY-MM-DD or FROM..TO")
	}
	if !isRange {
		*d = append(*d, from)
		return nil
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil || end.Before(start) {
		return fmt.Errorf("--date range must be FROM..TO with TO on or after FROM")
	}
	if end.Sub(start) > maxMigrateRangeDays*24*time.Hour {
		return fmt.Errorf("--date range spans more than %d days", maxMigrateRangeDays)
	}
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		*d = append(*d, day.Format("2006-01-02"))
	}
	return nil
}
