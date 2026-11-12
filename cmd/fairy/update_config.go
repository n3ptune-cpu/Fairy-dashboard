package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The check of config.yaml before `fairy update` replaces the program (r3-p4-01).
//
// The panel reads config.yaml only when it starts, so a mistake made since the last start shows at the
// update's restart: the new version refuses the file, the old one that is put back refuses it too, and the
// panel is down. The update therefore checks the file before the swap, with the one check `fairy serve` starts
// with (config.LoadFull), and it asks the version that will run AFTER the swap: the downloaded program, once
// its signature and hash are verified (the update runs it at that point anyway, for `version`), is started as
//
//	<new program> check-config <file>
//
// and answers in one line on its standard output:
//
//	fairy check-config 1 ok
//	fairy check-config 1 refused <why, with the line where there is one>
//
// A program that answers anything else is a release from before this command (only `--force` installs one):
// then the installed version's own check is what the update has.

// checkConfigCmd is the sub-command; checkConfigLead begins its answer.
const (
	checkConfigCmd  = "check-config"
	checkConfigLead = "fairy check-config 1 "
)

// checkConfig is `fairy check-config FILE`: it reads the file only, and changes nothing.
func checkConfig(args []string, out io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(out, checkConfigLead+"refused usage: fairy check-config FILE")
		return 2
	}
	if _, err := config.LoadFull(args[0]); err != nil {
		fmt.Fprintln(out, checkConfigLead+"refused "+configProblem(err))
		return 1
	}
	fmt.Fprintln(out, checkConfigLead+"ok")
	return 0
}

// configFileProblem is why the next start would refuse this config file ("": it would not), with the file's
// name: what the running panel says before it starts its own update.
func configFileProblem(path string) string {
	if _, err := config.LoadFull(path); err != nil {
		return path + ": " + configProblem(err)
	}
	return ""
}

// configProblem is a refusal of config.yaml in one line.
func configProblem(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "the file does not exist"
	}
	return oneLine(err.Error())
}

// What the new version said about the file.
const (
	newAccepts = iota + 1
	newRefuses
	newCannotSay // it does not know the command, or did not answer
)

// askNewVersion runs the downloaded, verified program's check of the config file.
func askNewVersion(program, file string) (verdict int, problem string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, program, checkConfigCmd, file).Output() // the answer decides, not the exit code
	return readCheckAnswer(string(out))
}

// readCheckAnswer reads the first line of the new program's answer.
func readCheckAnswer(out string) (verdict int, problem string) {
	line, _ := bufio.NewReader(strings.NewReader(out)).ReadString('\n')
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), checkConfigLead)
	switch {
	case ok && rest == "ok":
		return newAccepts, ""
	case ok && strings.HasPrefix(rest, "refused ") && len(rest) > len("refused "):
		return newRefuses, strings.TrimPrefix(rest, "refused ")
	}
	return newCannotSay, ""
}

// newVersion is one run's question to the version it is about to install, and the answer.
type newVersion struct {
	file    string // the config file the service starts with
	to      string // the new version, once the run knows it
	problem string // why the new version refuses the file ("": it does not, or could not say)
}

// runNew is release.CLI's RunNew: the downloaded program must run here (`version`), and it must not refuse the
// config file. An error ends the run before anything is replaced, and the CLI deletes the download.
func (n *newVersion) runNew(program string) (string, error) {
	out, err := exec.Command(program, "version").Output()
	if err != nil {
		return string(out), err
	}
	if verdict, problem := askNewVersion(program, n.file); verdict == newRefuses {
		n.problem = problem
		return string(out), errors.New("it refuses the config file") // what the run says instead: stops
	}
	return string(out), nil
}

// step is release.CLI's Progress: it remembers the new version and returns the message for the status file.
func (n *newVersion) step(state, to, msg string) string {
	if to != "" {
		n.to = to
	}
	if n.problem != "" && state == "failed" {
		return n.stops()
	}
	return msg
}

// stops is what a run says, on the console and in the status the page shows, when the version it was about to
// install refuses the config file: a run by hand and the button's run both stop there, before the swap.
func (n *newVersion) stops() string {
	lead := "the new version"
	if n.to != "" {
		lead += " (" + n.to + ")"
	}
	lead += " refuses " + n.file + ": "
	const tail = ". Nothing was changed. Correct the file on the panel's host, then update again."
	return lead + clipTo(n.problem, 300-len(lead)-len(tail)) + tail // the status file keeps 300 bytes, and the way out must fit
}

// clipTo cuts s to at most room bytes at a character's start, with "…" for what is left out.
func clipTo(s string, room int) string {
	if len(s) <= room {
		return s
	}
	room -= len("…")
	for room > 0 && !utf8.RuneStart(s[room]) {
		room--
	}
	if room < 0 {
		room = 0
	}
	return s[:room] + "…"
}
