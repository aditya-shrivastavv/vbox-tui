package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// A batch applies one lifecycle action to several explicitly selected
// machines, sequentially and in list order. Planning and execution are split
// so the plan shown to the user comes from the same pure rule the executor
// later applies: what a preview promises, what runs, and what the summary
// reports cannot drift apart.

// BatchTarget is the plan for one machine.
type BatchTarget struct {
	VM      vbox.Machine
	Verb    vbox.Action // the verb actually sent, empty means nothing to send
	Skip    string      // non-empty: this machine will be left alone, why
	Attempt bool        // false for skipped entries and already-there entries
}

// PlanBatch decides, for freshly-read machines, what the action means for
// each: the verb to send, or the reason a machine is skipped. It reads
// nothing and mutates nothing; callers pass machines they just listed.
//
// Every target is resolved and vetted here, before anything mutates: a
// machine whose fresh state refuses the action never enters the execution
// set, so a request VirtualBox would refuse is never issued by an operator
// who confirmed a plan that listed it.
func PlanBatch(action vbox.Action, machines []vbox.Machine) []BatchTarget {
	plan := make([]BatchTarget, 0, len(machines))
	for _, vm := range machines {
		t := BatchTarget{VM: vm}
		verb, why := DecideVerb(action, vm.State)
		switch verb {
		case VerbRefused:
			t.Skip = why
		case "":
			// Already at the start of the action: honest skip, nothing sent.
			t.Skip = fmt.Sprintf("it is already %s", vm.State.Word())
		default:
			t.Verb = verb
			t.Attempt = true
		}
		plan = append(plan, t)
	}
	return plan
}

// BatchResult is the end of one machine's turn.
type BatchResult struct {
	VM   vbox.Machine
	Verb vbox.Action
	Err  error
	// Report carries the restart verdict when the verb was a graceful
	// restart; other verbs leave it nil.
	Report *RestartReport
}

// BatchSummary is the aggregate verdict of one batch run.
type BatchSummary struct {
	Action    vbox.Action
	Succeeded []BatchResult
	Failed    []BatchResult
	Skipped   []BatchTarget // refused or already-there before anything ran
	Untried   []BatchTarget // never reached: the run stopped early
	Cancelled bool          // the user left mid-run
}

// ExitCode reports 0 only when every named target was attempted and
// succeeded: a skipped, failed, untried or cancelled batch is an incomplete
// batch, and a script driving vbx must be able to see that.
func (s BatchSummary) ExitCode() int {
	if len(s.Failed) == 0 && len(s.Skipped) == 0 && len(s.Untried) == 0 && !s.Cancelled {
		return 0
	}
	return 1
}

// AnyAttempt reports whether a state-changing request may have reached
// VirtualBox during the run — which decides the honest cancellation wording.
func (s BatchSummary) AnyAttempt() bool {
	return len(s.Succeeded) > 0 || len(s.Failed) > 0
}

// batchLine is one machine's progress line: the verb and the name.
func batchLine(t BatchTarget) string {
	return fmt.Sprintf("%s %s", t.Verb.Label(), quoteName(nameOf(t.VM)))
}

// SummaryLine is the single honest result sentence for a whole batch.
func (s BatchSummary) SummaryLine() string {
	var parts []string
	if n := len(s.Succeeded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d succeeded", n))
	}
	if n := len(s.Failed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d failed (%s)", n, targetNames(s.failedNames())))
	}
	if n := len(s.Skipped); n > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (%s)", n, targetNames(s.skippedNames())))
	}
	if n := len(s.Untried); n > 0 {
		parts = append(parts, fmt.Sprintf("%d not attempted (%s)", n, targetNames(namesOfTargets(s.Untried))))
	}
	lead := fmt.Sprintf("%s", strings.Title(string(s.Action))) //nolint:staticcheck // verbs are plain ASCII words
	if s.Cancelled {
		lead = "Cancelled"
	}
	if len(parts) == 0 {
		return lead + ": nothing to do."
	}
	return lead + ": " + strings.Join(parts, ", ") + "."
}

func (s BatchSummary) failedNames() []string {
	out := make([]string, 0, len(s.Failed))
	for _, r := range s.Failed {
		out = append(out, nameOf(r.VM))
	}
	return out
}

func (s BatchSummary) skippedNames() []string {
	return namesOfTargets(s.Skipped)
}

func namesOfTargets(ts []BatchTarget) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, nameOf(t.VM))
	}
	return out
}

func targetNames(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, quoteName(n))
	}
	return strings.Join(quoted, ", ")
}

// RunBatch executes a plan sequentially, in order, stopping at the first
// error: a failed machine usually means something systemic (a lock, a wedged
// hypervisor), and ploughing through more machines after it buries the
// signal under more noise. The stop is honest in the summary: what was not
// attempted is named.
//
// Each round trips through the backend's live list, so a machine that moved
// between preview and execution answers to its state at its own moment, not
// at plan time. A machine that vanished is reported as failed — the user
// asked for it by name and it did not happen.
//
// onResult is called after each machine (nil allowed) and drives the UI's
// per-VM progress lines.
func RunBatch(
	ctx context.Context,
	backend vbox.Manager,
	action vbox.Action,
	plan []BatchTarget,
	routes RestartRoutes,
	onResult func(BatchResult),
) BatchSummary {
	sum := BatchSummary{Action: action}
	for i, t := range plan {
		if !t.Attempt {
			sum.Skipped = append(sum.Skipped, t)
			continue
		}
		if err := ctx.Err(); err != nil {
			sum.Cancelled = true
			sum.Untried = append(sum.Untried, rest(plan, i)...)
			return sum
		}
		res := BatchResult{VM: t.VM, Verb: t.Verb}
		// Re-read this machine's state at its own moment. A transition since
		// the plan (the GUI paused it, a shutdown finished) must be honoured,
		// and a refusal now means the request is never issued.
		fresh, ok, err := rebind(ctx, backend, t.VM.UUID)
		switch {
		case err != nil:
			res.Err = err
		case !ok:
			res.Err = fmt.Errorf("no longer registered with VirtualBox")
		case t.Verb == vbox.ActionRestart:
			// A graceful restart goes through its own door: the mechanism is
			// chosen per VM. A machine whose route is blocked or whose guest
			// refused is reported for that machine and the batch continues —
			// one VM missing Guest Additions is not a systemic failure, and
			// nothing that failed here was reset or power-cycled.
			verb, why := DecideVerb(t.Verb, fresh.State)
			if verb == VerbRefused {
				res.Err = fmt.Errorf("state changed before its turn: %s", why)
				break
			}
			report := RequestRestart(ctx, backend, fresh, routes)
			res.Report = &report
			switch {
			case report.Outcome == RestartBlocked:
				t.Skip = report.Why
				t.Attempt = false
				sum.Skipped = append(sum.Skipped, t)
				if onResult != nil {
					onResult(res)
				}
				continue
			case !report.OK():
				res.Err = errors.New(report.Why)
			}
		default:
			verb, why := DecideVerb(t.Verb, fresh.State)
			if verb == VerbRefused {
				res.Err = fmt.Errorf("state changed before its turn: %s", why)
			} else if verb == "" {
				// Settled by itself into where the action starts; nothing to
				// send for this machine.
				res.Verb = ""
			} else {
				res.Err = Perform(ctx, backend, verb, t.VM.UUID)
			}
		}
		if res.Err != nil {
			sum.Failed = append(sum.Failed, res)
			if onResult != nil {
				onResult(res)
			}
			sum.Untried = append(sum.Untried, rest(plan, i+1)...)
			return sum
		}
		sum.Succeeded = append(sum.Succeeded, res)
		if onResult != nil {
			onResult(res)
		}
	}
	return sum
}

func rest(plan []BatchTarget, from int) []BatchTarget {
	if from >= len(plan) {
		return nil
	}
	return append([]BatchTarget(nil), plan[from:]...)
}

// rebind reads the latest view of one machine. Errors are returned so a
// transient hypervisor problem is reported as a failure for that machine
// rather than silently skipped.
func rebind(ctx context.Context, backend vbox.Manager, uuid string) (vbox.Machine, bool, error) {
	inv, err := backend.List(ctx)
	if err != nil {
		return vbox.Machine{}, false, err
	}
	vm, ok := inv.Find(uuid)
	return vm, ok, nil
}

// ---------- interactive glue ----------

// Batch flow messages. The plan is built before anything mutates, confirmed
// by the user, and then executed with per-machine progress lines appearing as
// each machine's turn finishes.

type batchPlanMsg struct {
	gen    uint64
	action vbox.Action
	plan   []BatchTarget
	// lost names selected entries that vanished between selection and the
	// fresh read. They are reported, never silently dropped: a person who
	// selected a machine must hear what became of it.
	lost []string
	err  error
}

type batchProgressMsg struct {
	gen  uint64
	line string
	err  string
}

type batchDoneMsg struct {
	gen uint64
	sum BatchSummary
}

// batchTickMsg drives the progress poll while a batch runs in its command
// goroutine: results land in m.batchPending under m.batchMu, and each tick
// moves them onto the visible lines.
type batchTickMsg struct{ gen uint64 }

// requestBatchPlan is the Enter-with-a-selection path: read the current
// inventory, resolve every selected name-or-UUID, and put the verdict of the
// verb rule in front of the user before anything runs.
func (m *Model) requestBatchPlan(action vbox.Action) tea.Cmd {
	m.mode = modeBusy
	m.phase = "Checking the selected machines…"
	m.genBatch++
	gen := m.genBatch
	backend := m.backend
	wanted := m.selectionSnapshot() // canonical UUIDs in inventory order
	names := m.selectionNames()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		inv, err := backend.List(ctx)
		if err != nil {
			return batchPlanMsg{gen: gen, action: action, err: err}
		}
		var targets []vbox.Machine
		var lost []string
		for _, uuid := range wanted {
			if vm, ok := inv.Find(uuid); ok {
				targets = append(targets, vm)
				continue
			}
			lost = append(lost, uuid)
		}
		for i, n := range lost {
			// Keep the name the user selected, and keep the order after the
			// resolvable machines so the plan reads predictably.
			_ = i
			if nm, isName := names[n]; isName {
				lost[i] = nm
			}
		}
		plan := PlanBatch(action, targets)
		for _, n := range lost {
			plan = append(plan, BatchTarget{
				VM:   vbox.Machine{Name: n},
				Skip: "it is no longer registered with VirtualBox",
			})
		}
		return batchPlanMsg{gen: gen, action: action, plan: plan, lost: lost}
	}
}

func (m *Model) applyBatchPlan(msg batchPlanMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genBatch {
		return m, nil
	}
	if msg.err != nil {
		return m, m.finish(OutcomeFailure, "Could not check the selected machines: "+oneError(msg.err, 400))
	}
	attempts := 0
	for _, t := range msg.plan {
		if t.Attempt {
			attempts++
		}
	}
	if attempts == 0 {
		// Every named machine refuses the action right now. Saying "0
		// succeeded, N skipped" honestly matters more than running nothing
		// silently.
		sum := BatchSummary{Action: msg.action, Skipped: msg.plan}
		return m, m.finish(OutcomeFailure, sum.SummaryLine()+" Nothing was sent to VirtualBox.")
	}
	m.batchAction = msg.action
	m.batchPlan = msg.plan
	m.mode = modeConfirm
	return m, nil
}

// batchConfirmLines renders the plan the user is asked to sign off: one line
// per machine, verb or skip reason, nothing sent before the yes.
func (m *Model) batchConfirmLines() []string {
	out := []string{m.question(fmt.Sprintf("Run %s on these %d machines?",
		m.batchAction.Label(), len(m.batchPlan)))}
	for _, t := range m.batchPlan {
		if t.Attempt {
			out = append(out, m.styles.Muted.Render("·  "+batchLine(t)))
			continue
		}
		out = append(out, m.styles.Muted.Render(
			fmt.Sprintf("·  skip %s: %s", quoteName(nameOf(t.VM)), t.Skip)))
	}
	out = append(out, m.styles.Muted.Render("y   run in order, one machine at a time · n / esc  cancel (nothing is sent to VirtualBox)"))
	return out
}

func (m *Model) handleBatchConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		return m.startBatch()
	case "n", "N", "esc", "ctrl+c":
		m.batchAction = ""
		m.batchPlan = nil
		m.mode = modePickVM
		m.reconcileCursor()
		return m, nil
	}
	return m, nil
}

// startBatch runs the plan in the command channel: sequentially, in order,
// with progress on each machine's completion. The cancellation stops between
// machines only — a request already handed to VirtualBox cannot be recalled,
// and the summary says which machines were not attempted.
func (m *Model) startBatch() (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.batchCancel = cancel
	m.batchDone = nil
	m.mode = modeBatchRun
	m.phase = fmt.Sprintf("%s: working through %d machines…", m.batchAction.Label(), len(m.batchPlan))
	m.genBatch++
	gen := m.genBatch
	backend := m.backend
	action := m.batchAction
	plan := append([]BatchTarget(nil), m.batchPlan...)
	routes := m.routes()
	run := func() tea.Msg {
		defer cancel()
		sum := RunBatch(ctx, backend, action, plan, routes, func(res BatchResult) {
			line := SuccessLine(res.Verb, quoteName(nameOf(res.VM)))
			if res.Verb == "" {
				line = fmt.Sprintf("%s settled by itself: nothing was sent.", quoteName(nameOf(res.VM)))
			}
			if res.Report != nil {
				line = res.Report.Summary(quoteName(nameOf(res.VM)))
			}
			errLine := ""
			if res.Err != nil {
				errLine = oneError(res.Err, 200)
			}
			m.batchMu.Lock()
			m.batchPending = append(m.batchPending, batchProgressMsg{gen: gen, line: line, err: errLine})
			m.batchMu.Unlock()
		})
		return batchDoneMsg{gen: gen, sum: sum}
	}
	tick := func() tea.Msg {
		time.Sleep(m.batchTick())
		return batchTickMsg{gen: gen}
	}
	return m, tea.Batch(run, tick, m.spinner.Tick)
}

func (m *Model) batchTick() time.Duration {
	if d := m.batchPollDelay; d > 0 {
		return d
	}
	return 60 * time.Millisecond
}

func (m *Model) applyBatchDone(msg batchDoneMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genBatch {
		return m, nil
	}
	m.drainBatchPending()
	if m.batchCancel != nil {
		m.batchCancel()
		m.batchCancel = nil
	}
	sum := msg.sum
	note := ""
	if sum.Cancelled || sum.AnyAttempt() {
		note = " Machines already requested keep whatever VirtualBox has already applied; cancelling cannot recall those."
	}
	line := sum.SummaryLine() + note
	if sum.ExitCode() != 0 && !sum.AnyAttempt() {
		line = sum.SummaryLine() + " Nothing was sent to VirtualBox."
	}
	outcome := OutcomeSuccess
	if sum.ExitCode() != 0 {
		outcome = OutcomeFailure
	}
	m.batchAction = ""
	m.batchPlan = nil
	return m, m.finish(outcome, line)
}

// drainBatchPending moves finished-machine results from the mutex-protected
// pending buffer onto visible lines. Called from Update only.
func (m *Model) drainBatchPending() {
	m.batchMu.Lock()
	pending := m.batchPending
	m.batchPending = nil
	m.batchMu.Unlock()
	for _, p := range pending {
		if p.err != "" {
			m.batchDone = append(m.batchDone, "! "+p.line+" — "+p.err)
			continue
		}
		m.batchDone = append(m.batchDone, "+ "+p.line)
	}
}

// applyBatchTick keeps the per-VM lines moving while the run command is
// still busy on a machine.
func (m *Model) applyBatchTick(msg batchTickMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genBatch || m.mode != modeBatchRun {
		return m, nil
	}
	m.drainBatchPending()
	gen := m.genBatch
	delay := m.batchTick()
	return m, func() tea.Msg {
		time.Sleep(delay)
		return batchTickMsg{gen: gen}
	}
}

// batchRunningKeys answers esc during a run: cancellation is only honest
// between machines, so esc stops the sequence and the wait continues
// politely until the in-flight machine answers.
func (m *Model) handleBatchRunKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "q":
		if m.batchCancel == nil {
			return m, nil
		}
		m.batchCancelReq = true
		m.batchCancel()
		m.note = "Cancel requested: no further machine will be started. The machine in flight finishes its request first — VirtualBox cannot recall one already made."
		return m, nil
	}
	return m, nil
}

// batchLines renders progress: one line per finished machine, then the
// remaining count while the run continues.
func (m *Model) batchLines() []string {
	out := append([]string(nil), m.batchDone...)
	if m.mode == modeBatchRun {
		done := len(m.batchDone)
		total := len(m.batchPlan)
		if left := total - done; left > 0 {
			out = append(out, m.styles.Muted.Render(
				fmt.Sprintf("%d of %d done, %d to go", clamp(done, 0, total), total, max(left, 0))))
		}
	}
	return out
}

// RunBatchDirect is the program-free door the CLI uses when the command line
// itself named every target (`vbx start A B`): resolve, plan, run, report.
// The caller resolved all names before calling, so a name that does not mean
// one machine already failed the whole command — the same rule single-target
// commands follow. onLine (nil allowed) receives one progress line per
// machine as it finishes; the return value carries the verdict and exit code.
func RunBatchDirect(
	ctx context.Context,
	backend vbox.Manager,
	action vbox.Action,
	machines []vbox.Machine,
	routes RestartRoutes,
	onLine func(string),
) BatchSummary {
	plan := PlanBatch(action, machines)
	return RunBatch(ctx, backend, action, plan, routes, func(res BatchResult) {
		if onLine == nil {
			return
		}
		line := SuccessLine(res.Verb, quoteName(nameOf(res.VM)))
		if res.Verb == "" {
			line = fmt.Sprintf("%s settled by itself: nothing was sent.", quoteName(nameOf(res.VM)))
		}
		if res.Report != nil {
			line = res.Report.Summary(quoteName(nameOf(res.VM)))
		}
		if res.Err != nil {
			line += " — " + oneError(res.Err, 200)
		}
		onLine(line)
	})
}
