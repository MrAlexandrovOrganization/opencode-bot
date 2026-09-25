package bot

import "testing"

func TestBeginTextRequestQueuesWhileBusy(t *testing.T) {
	b := &Bot{busy: true}

	ctx, start, position, full := b.beginTextRequest(42, "первое")
	if ctx != nil || start || full || position != 1 {
		t.Fatalf("first queue result = ctx:%v start:%v position:%d full:%v", ctx, start, position, full)
	}
	_, start, position, full = b.beginTextRequest(42, "второе")
	if start || full || position != 2 {
		t.Fatalf("second queue result = start:%v position:%d full:%v", start, position, full)
	}
	if got := len(b.queued); got != 2 {
		t.Fatalf("queued count = %d, want 2", got)
	}
}

func TestQueuedTextRequestsRemainFIFO(t *testing.T) {
	b := &Bot{queued: []queuedText{{chatID: 1, text: "first"}, {chatID: 2, text: "second"}}}

	ctx, first, left, ok := b.takeQueuedTextRequest()
	if !ok || ctx == nil || first.text != "first" || first.chatID != 1 || left != 1 {
		t.Fatalf("first dequeue = ctx:%v item:%+v left:%d ok:%v", ctx, first, left, ok)
	}
	b.releaseRequest(false)
	ctx, second, left, ok := b.takeQueuedTextRequest()
	if !ok || ctx == nil || second.text != "second" || second.chatID != 2 || left != 0 {
		t.Fatalf("second dequeue = ctx:%v item:%+v left:%d ok:%v", ctx, second, left, ok)
	}
}

func TestBeginTextRequestRejectsWhenQueueIsFull(t *testing.T) {
	b := &Bot{busy: true}
	for i := 0; i < maxQueuedTextMessages; i++ {
		if _, start, _, full := b.beginTextRequest(1, "message"); start || full {
			t.Fatalf("message %d was not queued", i)
		}
	}
	if _, start, position, full := b.beginTextRequest(1, "overflow"); start || !full || position != 0 {
		t.Fatalf("overflow = start:%v position:%d full:%v", start, position, full)
	}
}

func TestClearQueuedText(t *testing.T) {
	b := &Bot{queued: []queuedText{{text: "one"}, {text: "two"}}}
	if got := b.clearQueuedText(); got != 2 {
		t.Fatalf("cleared = %d, want 2", got)
	}
	if len(b.queued) != 0 {
		t.Fatalf("queue was not cleared: %+v", b.queued)
	}
}
