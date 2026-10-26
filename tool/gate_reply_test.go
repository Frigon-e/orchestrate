package main

import (
	"strings"
	"testing"
)

func TestLastReplyIsTheLastAssistantMessagesText(t *testing.T) {
	text := func(s string) string {
		return `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"` + s + `"},{"type":"toolCall","name":"read"},{"type":"text","text":"!"}]}}` + "\n"
	}
	user := `{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"prompt"}]}}` + "\n"
	// A saved session holds {"type":"message"} lines, a json stream message_end ones.
	sessionLine := strings.Replace(text("{\\\"findings\\\":[]}"), "message_end", "message", 1)
	got, err := lastReply(strings.NewReader(text("first") + user + "junk\n" + sessionLine + user))
	if err != nil || got != `{"findings":[]}!` {
		t.Fatalf("lastReply = %q, %v", got, err)
	}
	// Stream events also work without a trailing newline and above Scanner's default line limit.
	large := strings.Repeat("x", 128*1024)
	if got, err := lastReply(strings.NewReader(strings.TrimSuffix(text(large), "\n"))); err != nil || got != large+"!" {
		t.Fatalf("large final stream message: got %d characters, err %v", len(got), err)
	}
	toolsOnly := `{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","name":"read"}]}}` + "\n"
	for name, in := range map[string]string{"tool calls only": text("earlier") + toolsOnly, "empty": "", "no assistant": user} {
		if _, err := lastReply(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), "no reply") {
			t.Errorf("%s: err = %v, want no reply", name, err)
		}
	}
}
