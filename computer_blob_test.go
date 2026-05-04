package core

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
)

// stubComputer is the minimum that executeComputerAction needs:
// Execute() returns a deterministic byte slice we can identify in
// the BlobStore round-trip.
type stubComputer struct {
	bytes []byte
}

func (s *stubComputer) Execute(action computer.Action) ([]byte, error) {
	return s.bytes, nil
}
func (s *stubComputer) Screenshot() ([]byte, error)   { return s.bytes, nil }
func (s *stubComputer) DisplaySize() computer.DisplaySize {
	return computer.DisplaySize{Width: 100, Height: 100}
}
func (s *stubComputer) Close() error { return nil }

// Pins the screenshot → BlobStore wiring. Three properties matter:
//
//   1. The image bytes still get attached to ToolResult.Image so
//      vision input is unaffected (this is what lets the agent
//      "see" the page on its next thought).
//   2. The same bytes are stashed in the BlobStore under a fresh
//      blobref handle — so the agent can forward them to other tools.
//   3. The handle is surfaced in the ToolResult.Content text so the
//      agent has a string it can paste into a follow-up tool arg
//      (e.g. files_upload(content_base64=blobref://...)).
//
// If anyone removes the BlobStore Put or stops appending the ref to
// Content, the agent loses the ability to forward screenshots to
// storage / image-studio / etc — this test fails so the regression
// is caught before it ships.
func TestExecuteComputerAction_PublishesScreenshotToBlobStore(t *testing.T) {
	// Distinctive payload: PNG magic bytes + tail. The thinker
	// detects mime by leading bytes, so this exercises the
	// "mime=image/png" branch as well.
	pngMagic := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 'a', 'b', 'c'}

	bus := NewEventBus()
	blobs := NewBlobStore(DefaultBlobMaxTotal, DefaultBlobTTL)
	defer blobs.Close()

	t1 := &Thinker{
		threadID: "test",
		bus:      bus,
		blobs:    blobs,
		computer: &stubComputer{bytes: pngMagic},
	}

	// Subscribe before dispatching so we don't miss the event.
	obs := bus.SubscribeAll("test-observer", 8)

	var trMu sync.Mutex
	var got *ToolResult
	doneCh := make(chan struct{})
	go func() {
		for ev := range obs.C {
			if ev.Type == EventInbox && ev.ToolResult != nil {
				trMu.Lock()
				got = ev.ToolResult
				trMu.Unlock()
				close(doneCh)
				return
			}
		}
	}()

	t1.executeComputerAction(NativeToolCall{
		ID:   "computer_use:0",
		Name: "computer_use",
		Args: map[string]string{"action": "screenshot"},
	})

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tool result event")
	}

	trMu.Lock()
	defer trMu.Unlock()
	if got == nil {
		t.Fatal("no ToolResult published")
	}

	// (1) Vision channel intact.
	if got.Image == nil {
		t.Error("ToolResult.Image is nil — vision input would break")
	} else if string(got.Image) != string(pngMagic) {
		t.Errorf("ToolResult.Image bytes mismatch: got %v want %v", got.Image, pngMagic)
	}

	// (3) Blobref surfaced in the text the agent reads.
	if !strings.Contains(got.Content, "blobref://") {
		t.Fatalf("Content should mention blobref handle so the agent can forward bytes; got: %q", got.Content)
	}

	// Extract the ref from the content for the round-trip check.
	idx := strings.Index(got.Content, "blobref://")
	rest := got.Content[idx:]
	end := strings.IndexAny(rest, " \t\n.")
	if end == -1 {
		end = len(rest)
	}
	ref := rest[:end]

	// (2) Blob store has the same bytes under that ref, with the
	// detected mime type.
	data, mime, ok := blobs.Get(ref)
	if !ok {
		t.Fatalf("blob %q not found in store", ref)
	}
	if mime != "image/png" {
		t.Errorf("mime: got %q want image/png (PNG magic should detect)", mime)
	}
	if string(data) != string(pngMagic) {
		t.Errorf("stored bytes mismatch: got %v want %v", data, pngMagic)
	}
}

// JPEG branch coverage — most cloud backends return JPEG. Same
// shape, different magic bytes; expect mime=image/jpeg.
func TestExecuteComputerAction_DetectsJPEGMime(t *testing.T) {
	jpegMagic := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}

	bus := NewEventBus()
	blobs := NewBlobStore(DefaultBlobMaxTotal, DefaultBlobTTL)
	defer blobs.Close()

	t1 := &Thinker{
		threadID: "test",
		bus:      bus,
		blobs:    blobs,
		computer: &stubComputer{bytes: jpegMagic},
	}

	obs := bus.SubscribeAll("test-observer", 8)
	var ref string
	doneCh := make(chan struct{})
	go func() {
		for ev := range obs.C {
			if ev.Type == EventInbox && ev.ToolResult != nil {
				idx := strings.Index(ev.ToolResult.Content, "blobref://")
				if idx == -1 {
					return
				}
				rest := ev.ToolResult.Content[idx:]
				end := strings.IndexAny(rest, " \t\n.")
				if end == -1 {
					end = len(rest)
				}
				ref = rest[:end]
				close(doneCh)
				return
			}
		}
	}()

	t1.executeComputerAction(NativeToolCall{ID: "x", Name: "computer_use", Args: map[string]string{"action": "screenshot"}})

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	_, mime, ok := blobs.Get(ref)
	if !ok {
		t.Fatalf("blob %q not in store", ref)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime: got %q want image/jpeg", mime)
	}
}

// Without a BlobStore (legacy callers that built a Thinker without
// blobs), the screenshot path must still work — vision input must
// not regress just because the optional handle channel is absent.
func TestExecuteComputerAction_NoBlobStore_VisionStillWorks(t *testing.T) {
	bytesIn := []byte{0xFF, 0xD8, 0xFF}
	bus := NewEventBus()

	t1 := &Thinker{
		threadID: "test",
		bus:      bus,
		blobs:    nil, // legacy path — no blob store wired
		computer: &stubComputer{bytes: bytesIn},
	}

	obs := bus.SubscribeAll("test-observer", 8)
	var got *ToolResult
	doneCh := make(chan struct{})
	go func() {
		for ev := range obs.C {
			if ev.Type == EventInbox && ev.ToolResult != nil {
				got = ev.ToolResult
				close(doneCh)
				return
			}
		}
	}()

	t1.executeComputerAction(NativeToolCall{ID: "x", Name: "computer_use", Args: map[string]string{"action": "screenshot"}})

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	if got.Image == nil {
		t.Fatal("Image must remain attached even without BlobStore")
	}
	if strings.Contains(got.Content, "blobref://") {
		t.Errorf("no BlobStore present, but Content advertises a blobref: %q", got.Content)
	}
}
