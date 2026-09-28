package operation

import "testing"

func TestShellNeedsApproval(t *testing.T) {
	for _, command := range []string{
		"ssh prod", "scp a prod:/tmp", "rsync -av a b", "/usr/bin/ssh prod", "./scp a b",
		"cd /tmp && ssh prod", "true; rsync a b", "cat x | ssh prod", "(scp a b) & wait",
		"if true; then ssh prod; fi", "for h in prod; do ssh $h; done",
		"X=1 ssh prod", "sudo -u root /usr/bin/scp a b", "env X=1 ssh prod", "command -- ssh prod",
		"exec ssh prod", "nohup ssh prod", "timeout 5 ssh prod", "printf prod | xargs -n 1 ssh",
		"time ssh prod", "echo $(ssh prod)", "echo `ssh prod`", "cat <(ssh prod)",
		`'ssh' prod`, `s"s"h prod`, `s\sh prod`, "s\\\nsh prod",
		`sh -c 'ssh prod'`, `bash -lc 'true; scp a b'`, `sudo sh -c 'rsync a b'`,
		`eval 'ssh prod'`, `env -S 'ssh prod'`, "sh <<'EOF'\nssh prod\nEOF\n",
		`sudo -n -u root env X=1 /usr/bin/ssh prod`, `doas -u root scp a b`,
		`env --split-string='ssh prod'`, `timeout -k 1 5 ssh prod`, `exec -a other ssh prod`,
		`bash --norc -c 'ssh prod'`, `nice -n 5 rsync a b`,
		"ssh prod\nif", // valid prefix of a malformed script
	} {
		t.Run(command, func(t *testing.T) {
			if !shellNeedsApproval(command) {
				t.Fatal("missing permission gate")
			}
		})
	}
	for _, command := range []string{
		"pwd", "git status", "go test ./...", "echo ssh scp rsync", "printf '%s' 'ssh prod'",
		"grep ssh README.md", "rg -n 'ssh|rsync|scp' .", "ssh-keygen -h", "ssh-add -l",
		"cat /tmp/ssh", "echo rsync-helper", "# ssh prod\nprintf ok", "cat <<'EOF'\nssh prod\nEOF\n",
		`sh -c 'echo ssh'`, "echo /usr/bin/ssh", "git diff -- file-ssh.go",
		"command -v ssh", "command -V scp", "sudo -u root echo ssh", "env X=1 grep ssh README.md",
		"nice -n 5 echo rsync", "timeout 5 echo scp", "printf ssh | xargs echo",
	} {
		t.Run(command, func(t *testing.T) {
			if shellNeedsApproval(command) {
				t.Fatal("unrelated command gated")
			}
		})
	}
}
