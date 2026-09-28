package operation

import (
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var remoteCommandWord = regexp.MustCompile(`(^|[^a-zA-Z0-9_-])(ssh|scp|rsync)($|[^a-zA-Z0-9_-])`)

// This is an accidental-execution guard, not a shell sandbox: it inspects the
// submitted source, not sourced files, arbitrary programs or runtime expansion.
func shellNeedsApproval(command string) bool { return inspectShell(command, 0) }

func inspectShell(command string, depth int) bool {
	if depth > 16 {
		return remoteCommandWord.MatchString(command)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		// A shell can execute a valid prefix before encountering a syntax error.
		return remoteCommandWord.MatchString(command)
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			found = inspectCall(n.Args, depth)
		case *syntax.Stmt:
			if call, ok := n.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 && isShell(literalWord(call.Args[0])) {
				for _, redirect := range n.Redirs {
					if redirect.Hdoc != nil && inspectShell(literalWord(redirect.Hdoc), depth+1) {
						found = true
					}
				}
			}
		}
		return !found
	})
	return found
}

func inspectCall(words []*syntax.Word, depth int) bool {
	if len(words) == 0 {
		return false
	}
	name := path.Base(literalWord(words[0]))
	switch name {
	case "ssh", "scp", "rsync":
		return true
	case "sudo", "doas", "env", "command", "exec", "nohup", "nice", "timeout", "xargs":
		return inspectLauncher(name, words[1:], depth)
	case "eval":
		var args []string
		for _, word := range words[1:] {
			args = append(args, literalWord(word))
		}
		return inspectShell(strings.Join(args, " "), depth+1)
	default:
		if isShell(name) {
			for i := 1; i+1 < len(words); i++ {
				flag := literalWord(words[i])
				if strings.HasPrefix(flag, "-") && !strings.HasPrefix(flag, "--") && strings.Contains(flag, "c") {
					return inspectShell(literalWord(words[i+1]), depth+1)
				}
			}
		}
	}
	return false
}

// Skip launcher options before examining the actual command, so e.g.
// sudo -u root echo ssh and command -v ssh remain ordinary local requests.
func inspectLauncher(name string, words []*syntax.Word, depth int) bool {
	valueOptions := map[string]string{
		"sudo": "-u -g -h -p -C -D -R -T -r -t -U --user --group --host --prompt --close-from --chdir --chroot --command-timeout --role --type --other-user",
		"doas": "-u -C", "env": "-u -C --unset --chdir", "exec": "-a",
		"nice": "-n --adjustment", "timeout": "-s -k --signal --kill-after",
		"xargs": "-n -P -I -L -d -s -E -J --max-args --max-procs --replace --max-lines --delimiter --max-chars --eof",
	}
	options, duration := true, name == "timeout"
	for i := 0; i < len(words); i++ {
		arg := literalWord(words[i])
		if options && arg == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(arg, "-") {
			if arg == "--help" || arg == "--version" {
				return false
			}
			if name == "command" && strings.ContainsAny(arg, "vV") {
				return false
			}
			if name == "env" {
				if (arg == "-S" || arg == "--split-string") && i+1 < len(words) {
					return inspectShell(literalWord(words[i+1]), depth+1)
				}
				if strings.HasPrefix(arg, "--split-string=") {
					return inspectShell(strings.TrimPrefix(arg, "--split-string="), depth+1)
				}
				if strings.HasPrefix(arg, "-S") {
					return inspectShell(strings.TrimPrefix(arg, "-S"), depth+1)
				}
			}
			if strings.Contains(" "+valueOptions[name]+" ", " "+arg+" ") {
				i++
			}
			continue
		}
		if name == "env" && strings.Contains(arg, "=") {
			continue
		}
		if duration {
			duration = false
			options = false
			continue
		}
		return inspectCall(words[i:], depth+1)
	}
	return false
}

func isShell(name string) bool {
	switch path.Base(name) {
	case "sh", "bash", "dash", "zsh", "ksh":
		return true
	}
	return false
}

// Join literal/quoted parts without evaluating variables or running substitutions.
func literalWord(word *syntax.Word) string {
	var out strings.Builder
	var parts func([]syntax.WordPart, bool)
	parts = func(values []syntax.WordPart, quoted bool) {
		for _, value := range values {
			switch p := value.(type) {
			case *syntax.Lit:
				for i := 0; i < len(p.Value); i++ {
					c := p.Value[i]
					if c == '\\' && i+1 < len(p.Value) && (!quoted || strings.ContainsRune("$`\"\\\n", rune(p.Value[i+1]))) {
						i++
						c = p.Value[i]
						if c == '\n' {
							continue
						}
					}
					out.WriteByte(c)
				}
			case *syntax.SglQuoted:
				out.WriteString(p.Value)
			case *syntax.DblQuoted:
				parts(p.Parts, true)
			default:
				out.WriteByte(0) // Unknown expansion must not join two literals.
			}
		}
	}
	parts(word.Parts, false)
	return out.String()
}
