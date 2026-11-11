package main

// The derivation half of `next`: the pure predicate switch over an observed
// episode, its argv construction, and the source-gate mirror. Everything here
// is a function of nextObservation alone — no filesystem, store, clock or
// environment read.

import (
	"path/filepath"
	"sort"
	"strconv"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// deriveDayNext is the pure switch. It must not read the filesystem, the store,
// the clock or the environment.
func deriveDayNext(o nextObservation) nextAction {
	return applyRepairPolicy(o, derivePipeline(o))
}

// derivePipeline is the predicate order itself; deriveDayNext adds the
// repair-once answer policy on top.
func derivePipeline(o nextObservation) nextAction {
	n := nextAction{Day: o.Day}

	if o.Workflow.Finalized {
		return deriveFinalizedDay(o, n)
	}

	if o.Artifacts.EntriesPresent && len(o.Artifacts.Entries.Entries) > 0 {
		return deriveRecordedDay(o, n)
	}
	// A carry-only day (ADR 0011): segments were carried in, and the day's own
	// logging has run and has nothing to record (an empty ledger, or a
	// collected draft with no blocks). Its carried segments stage alone. Before
	// that, a carry never short-circuits the day's own collect/describe/record.
	if len(o.Artifacts.CarryIn) > 0 && (o.Artifacts.EntriesPresent || (o.DraftPresent && o.DraftValid && o.DraftBlocks == 0)) {
		return deriveRecordedDay(o, n)
	}

	if !o.DraftPresent {
		return n.withAction("run", o.argv("run"),
			"no draft artifact exists at "+o.DraftPath+" for this day")
	}
	if !o.DraftValid {
		// The bytes are not the draft type record consumes, so judging or
		// recording against them would repeat a failing action forever. Running
		// re-collects and rewrites the artifact.
		return n.withAction("run", o.argv("run"),
			"the draft artifact at "+o.DraftPath+" is not a valid draft JSON document, so record could never consume it; run re-collects the day")
	}
	if o.DraftBlocks == 0 {
		// record requires at least one block, so an empty draft has no path
		// through the pipeline; asking for descriptions would only feed a
		// record that must fail.
		return n.withStop("the draft artifact at " + o.DraftPath + " carries no blocks, so record would reject the day " +
			"(it requires at least one block). The day has no collected evidence: check the enabled sources in " +
			"configuration, or record the day's blocks with the documented reviewed-file recovery input")
	}
	if o.DescriptionsBinding != o.DraftDigest {
		// A present-but-unbound answer (an older draft, a hand-written file, or
		// no file at all) is stale: the judgment is asked for again so record is
		// never handed descriptions that belong to different blocks.
		why := "no descriptions answer is bound to the current draft"
		if o.DescriptionsPresent {
			why = "the descriptions answer at " + o.DescriptionsPath + " is not bound to the current draft (its draft_digest is stale)"
		}
		return n.withJudgment("review", nextJudgment{
			Kind:        judgmentBlockDescriptions,
			Day:         o.Day,
			AnswerFile:  o.DescriptionsPath,
			DraftDigest: o.DraftDigest,
			Draft:       o.Draft,
			Protocol: "write {\"draft_digest\":\"" + o.DraftDigest +
				"\",\"notes\":\"<string>\",\"descriptions\":{\"<block_id>\":\"<prose>\"}} to " +
				o.DescriptionsPath + "; copy draft_digest verbatim from this report (an answer bound to a different draft is " +
				"stale and will be asked for again); the blocks to describe are judgment.draft (the same document on disk at " +
				o.DraftPath + "); record validates every block's description and appends any prs_inline list — " + why,
		})
	}
	return n.withAction("record", o.recordArgv(),
		"the descriptions answer is bound to the current draft and no entries ledger is recorded")
}

// deriveFinalizedDay is the post-save tail. A finalized day is never restaged
// or re-recorded: a changed ledger is a human decision (`stage --revise`), and
// the only automatic step is the plan/reconcile/resolve loop, which terminates
// on durable timestamps.
func deriveFinalizedDay(o nextObservation, n nextAction) nextAction {
	if !o.Artifacts.EntriesPresent && len(o.Artifacts.CarryIn) == 0 {
		return n.withStop("the day is finalized, but its entries ledger is missing; restore the reviewed ledger before next can prove the saved content is current")
	}
	revise := "the human decides and runs `stage --revise --date " + o.Day + "`; next will not execute it"
	current, legacy := ledgerDerivesSaved(o.Units, o.LegacyUnits, o.Workflow.Confirmed)
	if !current && !legacy {
		ledger := ledgerContentSpans(o.Units)
		confirmed := confirmedContentSpans(o.Workflow.Confirmed)
		if len(o.Artifacts.CarryIn) > 0 {
			// A carry is never applied to a saved (or submitted) day silently
			// (ADR 0011): the human decides whether this day takes it in.
			in := o.Artifacts.CarryIn[0]
			return n.withStop("the day is finalized, but its entries ledger and the " + strconv.Itoa(len(o.Artifacts.CarryIn)) +
				" segment(s) carried from " + in.SourceDate + " (first: " + in.Start + "-" + in.End + ", " + strconv.Itoa(in.Minutes) +
				" min) no longer derive the saved intervals (" + strconv.Itoa(len(ledger)) + " span(s) vs " + strconv.Itoa(len(confirmed)) +
				" saved). A carry is never applied to a saved day silently; if this day should take it in, " + revise)
		}
		return n.withStop("the day is finalized, but the entries ledger no longer derives the saved intervals (" +
			strconv.Itoa(len(ledger)) + " ledger span(s) vs " + strconv.Itoa(len(confirmed)) + " saved). If that change is intentional, " + revise)
	}
	// The day's own carry record must be what its saved derivation carries: a
	// day saved under today's rule carries exactly CarryDerived, and a day saved
	// under a legacy rule carries nothing (ADR 0011).
	wantCarry := o.CarryDerived
	if !current {
		wantCarry = nil
	}
	if !timesheet.SameCarries(wantCarry, o.Artifacts.CarryOut) {
		return n.withStop("the day is finalized, but its carry record into the next date (" + strconv.Itoa(len(o.Artifacts.CarryOut)) +
			" segment(s)) does not match what its saved derivation carries (" + strconv.Itoa(len(wantCarry)) + "); " + revise)
	}
	if o.Workflow.PlanAmbiguous {
		return n.withStop("two plan records for " + o.Day + " share the newest created_at, so which one is current " +
			"cannot be derived; run `plan --date " + o.Day + "` for a fresh immutable record")
	}
	plan := o.Workflow.Plan
	if plan == nil {
		return o.planAction(n, "no plan record exists for this finalized day")
	}
	if !o.Workflow.PlanCurrent {
		return o.planAction(n, "the plan's source revisions no longer match the saved contributions; a fresh preview is required")
	}
	switch plan.State {
	case timesheet.PlanReady:
		return n.withHuman(nextHumanGates{
			ApproveCommand:   shellJoin(o.argv("submit")),
			SubmitCommand:    shellJoin(append(o.argv("submit"), "--approval-id", "<approval_id>")),
			StatusCommand:    shellJoin(append([]string{o.Exe, "submit", "status"}, o.argv("submit")[2:]...)),
			Unresolved:       o.Workflow.UnresolvedWrites,
			NativeUnresolved: o.Workflow.NativeUnresolved,
			Carried:          carriedView(o.Artifacts.CarryIn, o.Artifacts.CarryOut),
		})
	case timesheet.PlanBlocked:
		return deriveFinalizedBlockedPlan(o, n, plan)
	default:
		return n.withStop("the plan record " + plan.PlanID + " has the unknown state " + quote(plan.State) +
			"; run `plan --date " + o.Day + "` for a fresh record")
	}
}

// deriveBlockedPlan is the reconcile -> resolve -> fresh-plan loop, derived
// only from durable timestamps: the date's observation and the newest
// resolution must be strictly newer than the newest plan record to license a
// re-preview. Equal timestamps are underivable and stop.
func deriveBlockedPlan(o nextObservation, n nextAction, plan *timesheet.PlanSnapshot) nextAction {
	reconcile := shellJoin(o.argv("reconcile"))
	blockers := blockerCodes(plan.Blockers)

	observationAfter := false
	equalSeen := false
	if obs := o.Workflow.Observation; obs != nil {
		after, err := timestampAfter(obs.ObservedAt, plan.CreatedAt)
		if err != nil {
			return n.withStop("cannot compare the observation timestamp with the plan: " + err.Error())
		}
		observationAfter = after
		equalSeen = sameInstant(obs.ObservedAt, plan.CreatedAt)
	}
	resolutionAfter := false
	if res := o.Workflow.Resolution; res != nil {
		after, err := timestampAfter(res.At, plan.CreatedAt)
		if err != nil {
			return n.withStop("cannot compare the resolution timestamp with the plan: " + err.Error())
		}
		resolutionAfter = after
	}

	if observationAfter || resolutionAfter {
		return o.planAction(n, "the newest observation or resolution is strictly newer than the blocked plan "+
			plan.PlanID+", so the plan identity and its preview are stale")
	}
	head := "the newest plan " + plan.PlanID + " is blocked"
	if blockers != "" {
		head += " (blockers: " + blockers + ")"
	}
	head += "; next will not decide an identity or reuse a stale plan_id"
	if equalSeen {
		head += ". The newest observation has the same timestamp as the plan, so nothing is provably newer — reconcile again for a fresh observation: " + reconcile
	} else {
		head += ". Re-read the date for a fresh observation: " + reconcile
	}
	q := o.resolveAsk(plan, head)
	answer, ask := o.ask(n, q)
	if ask != nil {
		return *ask
	}
	return n.withAction("resolve", append(o.argv("resolve"), "--file", q.AnswerFile),
		"the human answered "+answer.Answer+" for the blocked plan "+plan.PlanID+" (via "+answer.Surface+")")
}

// deriveRecordedDay is the recorded-to-saved prefix. Every freshness decision
// here is a CONTENT proof, never a timestamp: staging is current when the stage
// map matches the ledger's own projection, and classification is current when
// the sidecar's per-block digests bind to that projection.
func deriveRecordedDay(o nextObservation, n nextAction) nextAction {
	if o.ProjectBlocked {
		// Several projects and none chosen: the human picks one (or passes
		// --project); next never guesses.
		q := projectAsk(o.Day, o.artifactDir(), o.ProjectIDs)
		q.LastError = o.ProjectAskError
		return n.withAsk(q)
	}

	if len(o.Units) == 0 {
		return n.withStop("nothing in the entries ledger has both a start and an end, so stage skipped every block " +
			"(times are never invented); fix the ledger entries and record again")
	}
	// The stored stage map is current exactly when it matches the content
	// projection a fresh stage would derive. Timestamps are not consulted: they
	// are second-resolution, so a same-second ledger edit would otherwise look
	// current and send save into a fail-closed replay loop.
	if !stageMapMatchesUnits(o.Artifacts.StageMap, o.Units) {
		return n.withAction("stage", o.projectArgv("stage"),
			"the stage map does not match the ledger's current derivation ("+strconv.Itoa(len(o.Units))+
				" stageable unit(s)), so the staged interval ids are not current")
	}
	if !timesheet.SameCarries(o.CarryDerived, o.Artifacts.CarryOut) {
		return n.withAction("stage", o.projectArgv("stage"),
			"the carry record into the next date ("+strconv.Itoa(len(o.Artifacts.CarryOut))+
				" segment(s)) does not match the ledger's current derivation ("+strconv.Itoa(len(o.CarryDerived))+"); stage rewrites it")
	}
	blocks := stagedBlocksFrom(o.Units, o.Artifacts.StageMap)
	digest := timesheet.StagedLedgerDigest(o.Day, blocks)
	// A sidecar that is absent means classification never ran; an explicitly
	// empty one ({"selections":{}}) is the documented no-op. A present sidecar
	// whose selections no longer bind to the current content is STALE: save
	// would reject it in applyPersistedSelections, so deriving save would only
	// repeat a failing action.
	binds := o.Artifacts.ChoicesPresent && sidecarBinds(o.Day, o.Artifacts.Choices, blocks)
	if o.GradionEnabled && o.Workflow.ConfiguredDefaultPresent {
		if o.Artifacts.ChoicesPresent && !binds {
			return n.withStop("the classification sidecar is stale; review the current staged content and repair with classify --file; configured defaults never overwrite prior selections")
		}
		if len(o.Artifacts.Choices) < len(blocks) {
			if o.Workflow.ConfiguredDefaultError != "" {
				return n.withStop(o.Workflow.ConfiguredDefaultError)
			}
			return n.withAction("classify", append(o.projectArgv("classify"), "--configured-default"), "unselected intervals require the configured cached default; existing explicit selections are preserved")
		}
	}
	if o.GradionEnabled && !binds {
		// The answer file is only handed to classify when it carries THIS
		// ledger's digest, so a stale choices file (an earlier generation, or
		// one whose blocks changed) can never be re-applied to shifted ids.
		if o.ChoicesBinding == digest {
			return n.withAction("classify", o.classifyArgv(),
				"the classification answer at "+o.ChoicesPath+" is bound to the current staged ledger")
		}
		why := "no classification answer is bound to the current staged ledger"
		if o.ChoicesPresent {
			why = "the classification answer at " + o.ChoicesPath + " is stale (its ledger_digest does not match the current staged ledger)"
		}
		// Approved history is applied first (no configured default reaches
		// here); only the intervals it cannot decide are judged.
		historyBound := o.History != nil && o.History.LedgerDigest == digest
		if o.Workflow.SubmittedHistory > 0 && !historyBound {
			return n.withAction("classify", append(o.projectArgv("classify"), "--from-history"),
				"confirmed submissions exist on other days; classify intervals whose repository or title has one consistent prior classification before the judgment")
		}
		judged := blocks
		kept := ""
		if historyBound && len(o.History.Selections) > 0 {
			judged = make(map[string]timesheet.EntryRecord, len(blocks))
			for id, e := range blocks {
				if _, picked := o.History.Selections[id]; !picked {
					judged[id] = e
				}
			}
			kept = "; " + strconv.Itoa(len(blocks)-len(judged)) + " interval(s) were classified from history, are not listed, and are kept"
		}
		// The catalog is read by a deterministic step first, so the judgment
		// is a pick from closed lists bound to this same ledger.
		if o.Candidates == nil || o.Candidates.LedgerDigest != digest {
			return n.withAction("candidates", append(o.argv("classifications"), "--candidates"),
				"no catalog candidates are bound to the current staged ledger; read the catalog once per distinct topic before the classification judgment")
		}
		jb := judgmentBlocks(judged)
		carriedFrom := map[string]string{}
		for _, u := range o.Units {
			if u.CarriedFrom != "" {
				carriedFrom[u.BlockID] = u.CarriedFrom
			}
		}
		for i := range jb {
			jb[i].CarriedFrom = carriedFrom[jb[i].BlockID]
		}
		return n.withJudgment("classify", nextJudgment{
			Kind:           judgmentClassifications,
			Day:            o.Day,
			AnswerFile:     o.ChoicesPath,
			LedgerDigest:   digest,
			Candidates:     judgmentCandidates(o.Candidates, jb),
			CandidatesNote: o.Candidates.Note,
			Blocks:         jb,
			Protocol: "write {\"ledger_digest\":\"" + digest +
				"\",\"selections\":{\"<interval_id>\":{\"catalog_id\":\"...\",\"classification_id\":\"...\"}}} to " +
				o.ChoicesPath + "; copy ledger_digest verbatim from this report (an answer bound to different content is stale " +
				"and will be asked for again); interval_ids and the recorded descriptions are listed in blocks — no draft or " +
				"descriptions artifact is needed; for each interval pick at most one option from the candidates set its block " +
				"names, copying that set's catalog_id and the option's classification_id verbatim; omit an interval with no " +
				"fitting candidate; do not query the catalog; {\"selections\":{}} is the documented no-op" + kept + " — " + why,
		})
	}
	if !o.GradionEnabled && o.Artifacts.ChoicesPresent && !binds {
		// No derivable classification step, and next never clears or drops
		// persisted selections on its own: name the two supported remedies.
		clearFile := filepath.Join(o.artifactDir(), "clear-classifications.json")
		clearCmd := shellJoin(o.classifyFileArgv(clearFile))
		return n.withStop("the persisted classification sidecar (" + o.SidecarPath +
			") no longer binds to the current staged content, and this configuration disables the remote catalog " +
			"boundary (gradion.enabled), so next will not route classify. Either re-enable the remote and run next " +
			"again for a fresh classification judgment, or make the explicit decision to clear the selections by " +
			"writing {\"selections\":{}} to " + clearFile + " and running: " + clearCmd +
			" — next will never clear or drop persisted selections itself")
	}

	if stop := o.sourceGateStop(n); stop != nil {
		return *stop
	}
	return n.withAction("save", o.projectArgv("save"),
		"the day is staged and within the source gate; save runs the existing confirm+save pair")
}

// judgmentBlocks lists the staged intervals' current content in interval-id
// order, so the classification answer can be authored from this report alone.
func judgmentBlocks(blocks map[string]timesheet.EntryRecord) []nextBlock {
	ids := make([]string, 0, len(blocks))
	for id := range blocks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]nextBlock, 0, len(ids))
	for _, id := range ids {
		e := blocks[id]
		out = append(out, nextBlock{
			IntervalID: id, BlockID: e.BlockID, Start: e.Start, End: e.End, Description: e.Description,
		})
	}
	return out
}

// sourceGateStop mirrors checkSourceGate's order: an unattempted enabled source
// needs collection (an action, never a stop), and an incomplete/failed attempt
// needs the human-authorized acknowledgment, whose exact argv is named with the
// collection_id observed in the store. It returns nil when the gate is open.
func (o nextObservation) sourceGateStop(n nextAction) *nextAction {
	for _, src := range o.Workflow.Sources {
		if !src.Attempted {
			stop := n.withAction("run", o.argv("run"),
				"the enabled source "+src.Source+" has no collection attempt for this day")
			return &stop
		}
	}
	// Acknowledgment is a human decision: each unacknowledged source is asked
	// in turn, and only the human's bound answer licenses the acknowledge act.
	for _, src := range o.Workflow.Sources {
		if src.Complete || src.Acknowledged {
			continue
		}
		answer, ask := o.ask(n, o.sourceAckAsk(src))
		if ask != nil {
			return ask
		}
		act := n.withAction("acknowledge",
			append(o.argv("acknowledge"), "--source", src.Source, "--collection-id", src.CollectionID),
			"the human answered acknowledge for the "+src.Source+" collection "+src.CollectionID+" (via "+answer.Surface+")")
		return &act
	}
	return nil
}

// --- argv construction -------------------------------------------------------

// argv starts every derived command with the absolute executable, the resolved
// config and env paths, and the explicit resolved day, so the command is
// correct from any cwd and never depends on a later "today".
func (o nextObservation) argv(verb string) []string {
	cmd := []string{o.Exe, verb, "--config", o.ConfigPath}
	if o.EnvFile != "" {
		cmd = append(cmd, "--env-file", o.EnvFile)
	}
	return append(cmd, "--date", o.Day)
}

func (o nextObservation) projectArgv(verb string) []string {
	return append(o.argv(verb), "--project", o.Project)
}

func (o nextObservation) recordArgv() []string {
	return append(o.argv("record"), "--draft", o.DraftPath, "--descriptions", o.DescriptionsPath)
}

func (o nextObservation) classifyArgv() []string {
	return o.classifyFileArgv(o.ChoicesPath)
}

// classifyFileArgv is the classify form for one choices file. The reason text
// reuses it for the explicit offline clear, so the named remedy is the same
// supported command.
func (o nextObservation) classifyFileArgv(file string) []string {
	return append(o.argv("classify"), "--file", file, "--project", o.Project)
}

func (o nextObservation) artifactDir() string {
	return filepath.Dir(o.DraftPath)
}

func (o nextObservation) planAction(n nextAction, why string) nextAction {
	if !o.GradionEnabled {
		return n.withStop("planning a submission preview requires the remote boundary, which this configuration does not enable " +
			"(gradion.enabled); the day is saved, so enable the remote and run `plan --date " + o.Day + "`")
	}
	return n.withAction("plan", o.argv("plan"), why)
}
