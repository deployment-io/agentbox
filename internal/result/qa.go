package result

// The QA half of the result.json schema — see docs/CONTRACT.md. The wire
// values are shared with deployment-runner-kit's tasks.QAResultDtoV1.
//
// Like the review half, every list here is owned by agentbox after
// extraction (internal/qa.Lift): the criteria are agentbox's own, in spec
// order, with the agent's verdict attached; findings are capped and carry a
// stage agentbox stamped.

// QA run statuses.
const (
	QAStatusCompleted  = "completed"
	QAStatusSkipped    = "skipped"
	QAStatusCouldntRun = "couldnt_run"
)

// QA criterion states.
const (
	QAStatePassed     = "passed"
	QAStateFailed     = "failed"
	QAStateNotChecked = "not_checked"
)

// QAResult is one QA run's output.
type QAResult struct {
	// Status is QAStatusCompleted, QAStatusSkipped or QAStatusCouldntRun.
	// couldnt_run means the previews or the browser could not be used — it
	// says nothing about the code.
	Status       string `json:"status"`
	StatusReason string `json:"status_reason,omitempty"`

	// NoCriteria is true when the spec carried no acceptance criteria and the
	// run did a smoke check instead. Criteria is then empty.
	NoCriteria bool `json:"no_criteria"`

	Criteria []QACriterion `json:"criteria"`

	// Findings use Review's finding shape, with Stage "qa" and Pass
	// "criteria".
	Findings []ReviewFinding `json:"findings"`

	// Observations are things QA noticed that no criterion covers (a console
	// error, a failed request). They are not findings.
	Observations []string `json:"observations"`
}

// QACriterion is one acceptance criterion and what QA concluded about it.
// Criterion is agentbox's own text, never the agent's.
type QACriterion struct {
	Criterion string `json:"criterion"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// WriteQAFailure is WriteFailure for a QA run that never reached the agent:
// the outcome carries a qa_result with status couldnt_run and the reason, so
// the consumer records "QA couldn't run, because X" rather than finding no QA
// at all.
func WriteQAFailure(err error, reason string) error {
	return Write(Outcome{
		Status:   StatusFailure,
		Error:    err.Error(),
		ExitCode: ExitExecutionFailure,
		QAResult: &QAResult{
			Status:       QAStatusCouldntRun,
			StatusReason: reason,
			Criteria:     []QACriterion{},
			Findings:     []ReviewFinding{},
			Observations: []string{},
		},
	})
}
