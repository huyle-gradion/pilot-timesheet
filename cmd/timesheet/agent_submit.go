package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

type agentSubmitReview struct {
	Plan     timesheet.PlanPreview `json:"plan"`
	ReviewID string                `json:"review_id"`
}

// runAgentSubmit is an opt-in agent consent assertion, never a presence proof.
// With no confirmation inputs it only reviews; the agent owns the chat gate.
func runAgentSubmit(args []string) error {
	flags := flag.NewFlagSet("submit --chat", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var config, env, day, planID, reviewID string
	var confirmed bool
	flags.StringVar(&config, "config", "", "configuration path")
	flags.StringVar(&env, "env-file", "", "environment-file path")
	flags.StringVar(&day, "date", "", "reviewed day, YYYY-MM-DD (required)")
	flags.StringVar(&planID, "plan-id", "", "exact reviewed plan_id")
	flags.StringVar(&reviewID, "review-id", "", "exact review_id binding plan and context")
	flags.BoolVar(&confirmed, "confirm-chat", false, "assert the user explicitly confirmed this exact review in chat; not proof of human presence")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("submit --chat does not accept positional arguments")
	}
	if err := validateDate("submit --chat", day, dateSupplied(flags)); err != nil {
		return err
	}
	submit := confirmed || planID != "" || reviewID != ""
	if submit && (!confirmed || strings.TrimSpace(planID) == "" || strings.TrimSpace(reviewID) == "") {
		return fmt.Errorf("submit --chat requires --plan-id, --review-id and --confirm-chat together after explicit chat confirmation")
	}
	config, env, err := resolveStoreFlags("submit --chat", config, env)
	if err != nil {
		return err
	}
	h, err := openHeadless(config, env)
	if err != nil {
		return err
	}
	defer h.done()
	artifacts, err := timesheet.ReadDayArtifacts(h.cfg.StatePath, day)
	if err != nil {
		return err
	}
	snapshot, err := timesheet.ReadWorkflowSnapshot(h.cfg, h.store, day)
	if err != nil {
		return err
	}
	if (!artifacts.EntriesPresent && len(artifacts.CarryIn) == 0) || !snapshot.Finalized {
		return fmt.Errorf("submit --chat: no saved recorded day; finish logging before reviewing submission")
	}
	if err := timesheet.CheckCarryTimezone(artifacts.CarryIn, snapshot.Timezone); err != nil {
		return fmt.Errorf("submit --chat: %w", err)
	}
	if err := checkCarryIn(h.cfg.StatePath, day, artifacts.CarryIn); err != nil {
		return fmt.Errorf("submit --chat: %w", err)
	}
	entries := artifactStageEntries(artifacts)
	units, carryOut := deriveLedger(entries, day, snapshot.Timezone)
	current, legacy := ledgerDerivesSaved(units, legacyDerivations(entries, day, snapshot.Timezone), snapshot.Confirmed)
	if !current && !legacy {
		return fmt.Errorf("submit --chat: recorded ledger differs from saved intervals; review stage --revise and classify before requesting fresh confirmation")
	}
	if !carryOutCurrent(current, carryOut, artifacts.CarryOut) {
		return fmt.Errorf("submit --chat: the carry record into the next date does not match the saved derivation; review stage --revise --date %s before requesting fresh confirmation", day)
	}
	res, err := h.call("preview_plan", timesheet.Request{Day: day})
	if err != nil {
		return err
	}
	plan, ok := res.(timesheet.PlanPreview)
	if !ok {
		return fmt.Errorf("submit --chat: unexpected preview_plan payload")
	}
	context, err := h.session.ReviewContext()
	if err != nil {
		return err
	}
	store, err := filepath.Abs(h.cfg.StatePath)
	if err != nil {
		return err
	}
	binding, err := json.Marshal([]string{day, plan.PlanID, store, context})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(binding)
	review := agentSubmitReview{Plan: plan, ReviewID: "review-" + hex.EncodeToString(digest[:])}
	if !submit {
		return printJSON(review)
	}
	if plan.PlanID != planID || review.ReviewID != reviewID {
		return fmt.Errorf("submit --chat: stale review; review the current plan and context and obtain fresh chat confirmation")
	}
	if plan.State != timesheet.PlanReady {
		return fmt.Errorf("submit --chat: plan is blocked; review blockers before requesting confirmation")
	}
	res, err = h.call("approve_plan", timesheet.Request{Day: day, PlanID: planID})
	if err != nil {
		return err
	}
	approval, ok := res.(timesheet.ApprovalReceipt)
	if !ok {
		return fmt.Errorf("submit --chat: unexpected approve_plan payload")
	}
	res, err = h.call("submit_plan", timesheet.Request{Day: day, ApprovalID: approval.ApprovalID})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.SubmissionReceipt)
	if !ok {
		return fmt.Errorf("submit --chat: unexpected submit_plan payload")
	}
	return printJSON(receipt)
}
