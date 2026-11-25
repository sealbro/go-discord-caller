package manager

// startRollback undoes a raid start or a guest join that failed part-way. Each
// step is set as the start reaches it, and run executes the ones that were, in
// the one order teardown is safe in — so no failure path can forget a step or
// run them differently from the others.
type startRollback struct {
	// audio closes the speakers: the setup's cleanup until the pipeline is
	// built, then the session's own, which stops the router first.
	audio func()
	// owner closes the owner bot's provider and receiver.
	owner      func()
	leaveOwner func()
	// pipeline releases what the pipeline build allocated outside the session.
	pipeline func()
	// relay detaches the guild from the ally session.
	relay     func()
	reconnect func()
	// view stops keeping the router's view of voice current.
	view func()
}

func (r *startRollback) run() {
	for _, step := range []func(){r.audio, r.owner, r.leaveOwner, r.pipeline, r.relay, r.reconnect, r.view} {
		if step != nil {
			step()
		}
	}
}
