# Personal pilot timesheet logger

Turns evidence of daily activity into user-confirmed working intervals and explicitly approved Gradion submissions. Activity alone does not establish time worked.

## Language

**Tracked Repository**:
A repository explicitly selected as a source of the user's local commit and remote pull request activity.
_Avoid_: Watched folder, workspace root

**Evidence**:
A source record supporting a possible activity, such as a calendar event, commit, manual declaration, or agent session event. Evidence does not by itself confirm attendance or working duration.
_Avoid_: Confirmed work, timesheet entry

**Topic Candidate**:
A collection of evidence concerning a possible work topic, preferably identified by repository and pull request. A topic may contain multiple separated Activity Groups.
_Avoid_: Approved task, billable topic

**Activity Group**:
Temporally close evidence within one Topic Candidate. Its timestamp span may suggest a duration but does not establish actual time worked.
_Avoid_: Working interval, confirmed duration

**Time Block**:
A bounded working interval with a project and description, either proposed or confirmed. Only user confirmation establishes it as recorded work.
_Avoid_: Timeslot, chunk

**Proposed Time Block**:
A suggested working interval awaiting confirmation of its duration, attendance where applicable, project, and description.
_Avoid_: Confirmed work, approved entry

**Confirmed Time Block**:
A working interval whose time, project, description, and applicable attendance the user has confirmed. Confirmation does not authorize remote submission.
_Avoid_: Submitted entry, submission approval

**Collision**:
An overlap between proposed or confirmed working intervals, or calendar and activity evidence requiring review. Multiple evidence records can describe one interval without adding working time.
_Avoid_: Double-counted work, automatic resolution

**Timesheet Entry**:
A work record prepared for or stored in Gradion, with a date, bounded interval, project or classification, and description. Its local status distinguishes reviewed, approved, and remotely submitted work.
_Avoid_: Evidence item, topic candidate

**Carried Segment**:
The part of a long overnight entry that the source day cannot hold within the remote four-hour policy, derived at stage and staged on the next date beside that day's own entries. It is recorded apart from both ledgers and never changes a saved day without the user's explicit revision.
_Avoid_: Moved entry, split entry, overflow

**Submission Plan**:
The exact proposed creates and edits against a known remote state for a specific daily record revision.
_Avoid_: Automatic sync, general write permission

**Approval**:
User authorization to execute one specific Submission Plan. It does not authorize changed plans or future days.
_Avoid_: Attendance confirmation, standing permission

**Remote Mapping**:
The association between a local Timesheet Entry and its stable Gradion entry identity and last observed remote state.
_Avoid_: Time-slot identity, inferred match

**Timesheet Reconciliation**:
Comparison of local records, Remote Mappings, and current Gradion entries to determine safe creates or edits. Uncertain writes and disagreements remain unresolved rather than becoming blind retries.
_Avoid_: Overwriting, replacement submission

**Collector Plugin**:
An explicitly enabled evidence source that declares its configuration needs and contributes Evidence without confirming work or authorizing submission.
_Avoid_: Submission plugin, approval agent

**Date Window**:
The selected local date or date range for collecting and reviewing activity. Each day has its own reviewed record and approval.
_Avoid_: Combined multi-day approval

**Agent Session Evidence**:
Recorded agent start, pause, resume, or finish activity suggesting that the user may have been working. Agent execution time is not the user's confirmed working time.
_Avoid_: Billable agent time, continuous user work

**Classification**:
A Gradion identifier categorizing submitted work. Its relationship to a billing project depends on the verified Gradion contract.
_Avoid_: Inferred project, AI-assigned billing code

**Harness-Agnostic Skill**:
An agent-facing workflow usable without requiring a particular agent vendor or model.
_Avoid_: Claude-only workflow, vendor-bound skill

**Editorial Usage**:
Tokens consumed to propose titles and descriptions, including retries and reruns. This is distinct from agent orchestration usage.
_Avoid_: Whole-session budget, free retries
