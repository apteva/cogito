package core

// Accessors on Thinker for sibling test packages.
//
// The scenario harness lives in package core and pokes Thinker's
// unexported fields directly — that's fine, same package. But scenarios
// in core/scenarios/ are a sibling package, and need read access to a
// few state fields to express their Wait/Verify conditions ("did the
// agent spawn a worker thread?", "is memory populated?", "what's the
// current iteration count?").
//
// Exposing these as methods (rather than upper-casing the field names)
// keeps the data model unchanged and the public API minimal.

// Threads returns the ThreadManager owning this Thinker's worker threads.
// Read-only intent; callers must not mutate the returned value's fields
// directly.
func (t *Thinker) Threads() *ThreadManager { return t.threads }

// Memory returns the MemoryStore this Thinker reads/writes against.
func (t *Thinker) Memory() *MemoryStore { return t.memory }

// Config returns the Config this Thinker was constructed from.
func (t *Thinker) Config() *Config { return t.config }

// Iteration returns the current think-loop iteration counter.
func (t *Thinker) Iteration() int { return t.iteration }

// Pool returns the underlying provider pool.
func (t *Thinker) Pool() *ProviderPool { return t.pool }

// Messages returns the current message slice. Returned slice shares
// backing storage with the Thinker — copy if you need to retain it.
func (t *Thinker) Messages() []Message { return t.messages }

// ResetConversation truncates the message history back to the initial
// system prompt. Used by long-running scenarios that need to clear
// context between phases without rebuilding the Thinker.
func (t *Thinker) ResetConversation() {
	if len(t.messages) > 0 {
		t.messages = t.messages[:1]
	}
}
