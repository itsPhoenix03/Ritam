package domain

// TurnOutcome is how an agent turn ended.
type TurnOutcome string

const (
	// TurnRunning has not ended yet.
	TurnRunning TurnOutcome = ""
	// TurnCompleted ended without a failure.
	TurnCompleted TurnOutcome = "completed"
	// TurnFailed ended on a failure of some kind (see FailureKind).
	TurnFailed TurnOutcome = "failed"
	// TurnKilled was ended by the orchestrator before it finished: the run
	// stopped (a pause, a cancel, a completion, ritam exiting) while the
	// agent was still working, or the process died with it.
	TurnKilled TurnOutcome = "killed"
)
