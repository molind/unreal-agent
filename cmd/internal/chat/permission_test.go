package chat

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestChatShellPermission(t *testing.T) {
	for _, decision := range []string{"yes", "no", "stop", "cancel"} {
		t.Run(decision, func(t *testing.T) {
			workspace := t.TempDir()
			c := launch(t, workspace, false)
			c.send("do the task")
			c.call().reply <- shellResponse("ssh() { printf invoked > proof; }; printf prefix > prefix; ssh prod")
			text := c.wait("ordinary messages never grant permission")
			match := regexp.MustCompile(`/permit (\S+) (\S+) yes`).FindStringSubmatch(text)
			if len(match) != 3 {
				t.Fatal(text)
			}
			checkAbsent := func() {
				t.Helper()
				for _, name := range []string{"prefix", "proof"} {
					if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
						t.Fatalf("unapproved side effect: %s, %v", name, err)
					}
				}
			}
			checkAbsent()
			c.send("/permit " + match[1] + " stale yes")
			c.wait("Cannot resolve shell permission")
			c.send("yes")
			c.send("/status")
			c.wait("awaiting permission —")
			checkAbsent()
			switch decision {
			case "stop":
				c.send("/stop")
				c.wait("Stopped. Send a new message")
			case "cancel":
				c.send("/cancel " + match[1])
				c.wait("Cancellation requested")
			default:
				c.send(strings.TrimSuffix(match[0], "yes") + decision)
				c.wait("Shell permission decision accepted")
			}
			if decision == "yes" {
				c.waitFile("proof")
			} else {
				checkAbsent()
			}
			c.send(match[0])
			if decision != "stop" {
				waitCount(t, c, "Cannot resolve shell permission", 2)
			}
			c.finish("/exit")
		})
	}
}
