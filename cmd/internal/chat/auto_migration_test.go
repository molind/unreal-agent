package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestChatStartupAutomaticallyMigratesAndResumes(t *testing.T) {
	workspace := t.TempDir()
	seedContextChat(t, workspace)
	c := launch(t, workspace, false, "-storage-format", "sqlite", "-session-directory", filepath.Join(workspace, "state"), "-session", "context-case")
	c.wait("completed-6")
	if _, err := os.Stat(filepath.Join(workspace, ".harness")); !os.IsNotExist(err) {
		t.Fatal("legacy .harness remains", err)
	}
	c.send("continue after automatic migration")
	call := c.call()
	if user := strings.Join(messages(call.request, llm.RoleUser), "\n"); !strings.Contains(user, "old-task-0") || !strings.Contains(user, "continue after automatic migration") {
		t.Fatal("lost migrated context")
	}
	call.reply <- reply("automatic migration done")
	c.wait("automatic migration done")
	c.finish("/exit")
}
