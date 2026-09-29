package chat

import (
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestCLIResumeHonorsSavedModelWithoutChangingNewChatDefaults(t *testing.T) {
	workspace := t.TempDir()
	store, err := localfile.New(filepath.Join(workspace, ".harness", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("selected-model")
	if _, err := store.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInput(t.Context(), id, newInput(inbox.InputControl, inbox.ControlMessage{
		Mode: inbox.UpdateSettings, Parameters: inbox.Settings{Model: "brand-new", ReasoningEffort: "future-effort"},
	})); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	chat := launch(t, workspace, false, "-session", string(id))
	chat.wait("Model: brand-new | Reasoning effort: future-effort")
	chat.send("continue with saved model")
	call := chat.call()
	if call.request.Model.ID != "brand-new" || call.request.Model.ReasoningEffort != "future-effort" {
		t.Fatal("CLI ignored saved settings", call.request.Model)
	}
	call.reply <- reply("saved model works")
	chat.wait("assistant> saved model works")
	chat.send("/new")
	chat.wait("Selected session: new unsaved chat")
	chat.send("use defaults here")
	call = chat.call()
	if call.request.Model.ID != "gpt-6-astra" || call.request.Model.ReasoningEffort != "xhigh" {
		t.Fatal("saved model leaked to new chat", call.request.Model)
	}
	call.reply <- reply("default model works")
	chat.wait("assistant> default model works")
	chat.finish("/exit")
}
