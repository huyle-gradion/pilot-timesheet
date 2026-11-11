package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func (d actDispatcher) artifactPath(name string) (string, error) {
	dir, err := dayArtifactsDir(d.cfg.StatePath, d.day)
	return filepath.Join(dir, name), err
}

func (d actDispatcher) runStep(h *headlessSession) (string, error) {
	draft, _, err := collectDraft(h, d.day)
	if err != nil {
		return "", err
	}
	data, err := jsonBytes(draft)
	if err != nil {
		return "", err
	}
	path, err := d.artifactPath("draft.json")
	if err != nil {
		return "", err
	}
	if err := ensureDayArtifactsDir("next", filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("next: the draft file %s cannot be written: %w", path, err)
	}
	return draftNotice(draft), nil
}

func (d actDispatcher) recordStep(h *headlessSession) (string, error) {
	draftPath, err := d.artifactPath("draft.json")
	if err != nil {
		return "", err
	}
	descriptionsPath, err := d.artifactPath("descriptions.json")
	if err != nil {
		return "", err
	}
	reviewed, err := composeDescriptionsOnly(draftPath, descriptionsPath)
	if err != nil {
		return "", err
	}
	if reviewed.Date != d.day {
		return "", fmt.Errorf("next: the reviewed draft's date %s does not match the resolved day %s", reviewed.Date, d.day)
	}
	receipt, err := timesheet.RecordEntries(d.cfg.StatePath, timesheet.AppVersion, reviewed, false, time.Now())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d entry(s) written", receipt.Written), nil
}

func (d actDispatcher) stageStep(h *headlessSession) (string, error) {
	summary, err := openFinalizeDay(h, "next", d.day)
	if err != nil {
		return "", err
	}
	receipt, err := applyStage(h, d.cfg, "next", d.day, summary)
	if err != nil {
		return "", err
	}
	return stageNotice(receipt), nil
}

func (d actDispatcher) classifyStep(h *headlessSession) (string, error) {
	choicesPath, err := d.artifactPath("choices.json")
	if err != nil {
		return "", err
	}
	receipt, err := classifyFileDay(h, d.cfg, d.day, choicesPath)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("classified %d interval(s)", receipt.Selections), nil
}

func (d actDispatcher) classifyDefaultStep(h *headlessSession) (string, error) {
	receipt, err := classifyDefaultDay(h, d.day)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("classified %d interval(s) from the configured default", receipt.Selections), nil
}

func (d actDispatcher) saveStep(h *headlessSession) (string, error) {
	saved, err := saveDay(h, d.cfg, "next", d.day)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("saved revision %d: %d interval(s), %d minute(s)",
		saved.Revision, saved.Intervals, saved.TotalMinutes), nil
}

func (d actDispatcher) planStep(h *headlessSession) (string, error) {
	res, err := h.call("preview_plan", timesheet.Request{Day: d.day})
	if err != nil {
		return "", err
	}
	preview, ok := res.(timesheet.PlanPreview)
	if !ok {
		return "", fmt.Errorf("next: unexpected preview_plan payload")
	}
	if preview.State == timesheet.PlanBlocked {
		return "plan blocked: " + blockerCodes(preview.Blockers), nil
	}
	return fmt.Sprintf("plan %s: %d operation(s)", preview.State, len(preview.Operations)), nil
}

func draftNotice(draft timesheet.Draft) string {
	meetings, minutes := 0, 0
	for _, b := range draft.Blocks {
		if b.Kind == "meeting" {
			meetings++
		}
		start, startErr := time.Parse("15:04", b.Start)
		end, endErr := time.Parse("15:04", b.End)
		if startErr == nil && endErr == nil && end.After(start) {
			minutes += int(end.Sub(start).Minutes())
		}
	}
	return fmt.Sprintf("%d block(s), %d meeting(s), %d min", len(draft.Blocks), meetings, minutes)
}
