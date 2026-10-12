package agent

func (ts *turnState) reportExecutionStage(stage string) {
	if ts.opts.ExecutionProgress != nil {
		ts.opts.ExecutionProgress(stage)
	}
}
