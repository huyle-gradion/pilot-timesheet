package main

import (
	"fmt"
	"maps"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func runClassifyDefault(f *finalizeFlags) error {
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	receipt, err := classifyDefaultDay(h, f.date, f.project)
	if err != nil {
		return err
	}
	return printJSON(receipt)
}

// classifyDefaultDay is classify --configured-default's whole flow over one
// held session: open the day, refuse a finalized one, and fill unselected
// unsaved intervals with the configured cached default. The tui verb's
// classify act is the same flow over its own held session.
func classifyDefaultDay(h *headlessSession, date, project string) (ClassifyReceipt, error) {
	res, err := h.call("open_day", timesheet.Request{Day: date})
	if err != nil {
		return ClassifyReceipt{}, err
	}
	summary, ok := res.(timesheet.DaySummary)
	if !ok {
		return ClassifyReceipt{}, fmt.Errorf("classify: unexpected open_day payload")
	}
	if finalizedSummary(summary) {
		return ClassifyReceipt{}, fmt.Errorf("classify --configured-default refuses finalized/saved days; use a reviewed classify --file decision to change saved classifications")
	}
	project, err = resolveProject("classify", h.cfg, project)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	return classifyDefaultCore(h, h.cfg, date, project, summary)
}

// classifyDefaultCore is classify --configured-default's core, shared by the
// verb and the tui verb's classify act: unselected intervals take the
// configured cached default, existing explicit selections are preserved, and
// the sidecar is rewritten with every selection content-bound.
func classifyDefaultCore(h *headlessSession, cfg *timesheet.Config, date, project string, summary timesheet.DaySummary) (ClassifyReceipt, error) {
	choice, err := h.session.ConfiguredDefault()
	if err != nil {
		return ClassifyReceipt{}, err
	}
	artifacts, err := timesheet.ReadDayArtifacts(cfg.StatePath, date)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	if artifacts.Entries == nil && len(artifacts.CarryIn) == 0 {
		return ClassifyReceipt{}, fmt.Errorf("classify: no recorded entries; run record first")
	}
	units := ledgerUnits(artifactStageEntries(artifacts), date, summary.Timezone)
	if !sidecarBinds(date, artifacts.Choices, stagedBlocksFrom(units, artifacts.StageMap)) {
		return ClassifyReceipt{}, fmt.Errorf("classify: stale classification sidecar; review the current staged content and repair with classify --file")
	}
	staged, err := applyStage(h, cfg, "classify", date, project, summary)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	blocks, err := stagedBlocks(cfg, date)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	if !sidecarBinds(date, artifacts.Choices, blocks) {
		return ClassifyReceipt{}, fmt.Errorf("classify: stale classification sidecar after staging; review and repair with classify --file")
	}
	selections := maps.Clone(artifacts.Choices)
	if selections == nil {
		selections = make(map[string]timesheet.ClassificationChoiceEntry)
	}
	for id, block := range blocks {
		if _, selected := selections[id]; selected {
			continue
		}
		bound := choice
		bound.ContentDigest = timesheet.BlockContentDigest(date, block)
		bound.Source = timesheet.SelectionSourceDefault
		selections[id] = bound
	}
	applied, err := applySelections(h, date, summary.Revision, selections)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	if err := timesheet.WriteClassificationChoices(cfg.StatePath, timesheet.AppVersion, date, selections, time.Now()); err != nil {
		return ClassifyReceipt{}, err
	}
	return ClassifyReceipt{
		Date: date, SidecarFile: timesheet.ChoicesPath(cfg.StatePath, date), Applied: applied,
		Selections: len(applied), Revision: summary.Revision, StagedIntervals: staged.StagedIntervals,
	}, nil
}
