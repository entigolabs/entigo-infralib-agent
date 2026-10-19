package agentstream

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/entigolabs/entigo-infralib-agent/gen/wrapper/v1alpha1"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

const maxDecidedWaits = 100

// Approvals tracks steps waiting on manual approval and applies backend
// decisions to them. Each wait accepts one decision; later requests for the same
// wait are answered from the recorded decision.
type Approvals struct {
	pipelineIndex atomic.Int32

	mu      sync.Mutex
	waiting map[string]*waiter
	decided map[waitKey]bool
	order   []waitKey

	// stepLocks serialize decisions per step so concurrent requests for the same
	// wait see the first one's outcome instead of applying a second decision,
	// without a slow approver call blocking other steps.
	stepLocks sync.Map
}

type waiter struct {
	pipelineIndex int32
	approver      model.Approver
}

type waitKey struct {
	step          string
	pipelineIndex int32
}

func NewApprovals() *Approvals {
	return &Approvals{
		waiting: map[string]*waiter{},
		decided: map[waitKey]bool{},
	}
}

func (a *Approvals) SetPipelineIndex(index int) {
	a.pipelineIndex.Store(int32(index))
}

func (a *Approvals) Register(step string, approver model.Approver) model.ApprovalWait {
	w := &waiter{pipelineIndex: a.pipelineIndex.Load(), approver: approver}
	a.mu.Lock()
	a.waiting[step] = w
	a.mu.Unlock()
	return &approvalWait{approvals: a, step: step, waiter: w}
}

type approvalWait struct {
	approvals *Approvals
	step      string
	waiter    *waiter
}

func (w *approvalWait) Resolve(approved bool) {
	a := w.approvals
	unlock := a.lockStep(w.step)
	defer unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeWait(w.step, w.waiter)
	a.remember(waitKey{step: w.step, pipelineIndex: w.waiter.pipelineIndex}, approved)
}

func (w *approvalWait) Close() {
	a := w.approvals
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeWait(w.step, w.waiter)
}

func (a *Approvals) closeWait(step string, w *waiter) {
	if a.waiting[step] == w {
		delete(a.waiting, step)
	}
}

func (a *Approvals) lockStep(step string) func() {
	lock, _ := a.stepLocks.LoadOrStore(step, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (a *Approvals) Decide(req *v1alpha1.ApprovalRequest) *v1alpha1.ApprovalResult {
	result := &v1alpha1.ApprovalResult{ApprovalId: req.GetApprovalId()}
	var approve bool
	switch req.GetDecision() {
	case v1alpha1.ApprovalDecision_APPROVAL_DECISION_APPROVE:
		approve = true
	case v1alpha1.ApprovalDecision_APPROVAL_DECISION_REJECT:
	default:
		result.Outcome = v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_FAILED
		result.Error = fmt.Sprintf("unsupported approval decision %s", req.GetDecision())
		return result
	}

	key := waitKey{step: req.GetStep(), pipelineIndex: req.GetPipelineIndex()}
	unlock := a.lockStep(key.step)
	defer unlock()
	a.mu.Lock()
	w := a.waiting[key.step]
	approved, decided := a.decided[key]
	a.mu.Unlock()
	if w == nil || w.pipelineIndex != key.pipelineIndex {
		if decided {
			result.Outcome = decidedOutcome(approved, approve)
			return result
		}
		result.Outcome = v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_NOT_WAITING
		result.Error = fmt.Sprintf("step %s is not waiting for approval in pipeline %d", key.step, key.pipelineIndex)
		return result
	}
	if err := w.approver(approve); err != nil {
		result.Outcome = v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_FAILED
		result.Error = err.Error()
		return result
	}

	a.mu.Lock()
	a.closeWait(key.step, w)
	a.remember(key, approve)
	a.mu.Unlock()
	action := "Approved"
	if !approve {
		action = "Rejected"
	}
	log.Printf("%s step %s through the portal by %s\n", action, key.step, req.GetRequestedBy())
	result.Outcome = v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_SUCCESS
	return result
}

func decidedOutcome(approved, approve bool) v1alpha1.ApprovalOutcome {
	switch {
	case approved == approve:
		return v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_SUCCESS
	case approved:
		return v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_ALREADY_APPROVED
	default:
		return v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_ALREADY_REJECTED
	}
}

func (a *Approvals) remember(key waitKey, approved bool) {
	if _, ok := a.decided[key]; !ok {
		a.order = append(a.order, key)
	}
	a.decided[key] = approved
	if len(a.order) > maxDecidedWaits {
		delete(a.decided, a.order[0])
		a.order = a.order[1:]
	}
}
