/*
   Copyright (c) 2016, Percona LLC and/or its affiliates. All rights reserved.

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

var (
	DefaultTimeout = 60 * time.Second
)

var (
	ErrNotFound                = errors.New("Executable file not found in $PATH")
	ErrTimeout                 = errors.New("Timeout")
	ErrKillProcessAfterTimeout = errors.New("Failed to kill process after timeout")
)

// Wrap os/exec/Cmd so we can test commands.
type Cmd interface {
	Run() (output string, err error)
}

type CmdFactory interface {
	Make(name string, args ...string) Cmd
}

// Set in main/percona-agent/main.go to RealCmdFactory for real agent,
// else set in tests to mock.CmdFactory for testing.
var Factory CmdFactory

// --------------------------------------------------------------------------

type RealCmdFactory struct {
}

func (f *RealCmdFactory) Make(name string, args ...string) Cmd {
	return NewRealCmd(name, args...)
}

type RealCmd struct {
	Timeout time.Duration
	name    string
	args    []string
}

type result struct {
	output string
	err    error
}

func NewRealCmd(name string, args ...string) *RealCmd {
	return &RealCmd{
		name:    name,
		args:    args,
		Timeout: DefaultTimeout,
	}
}

func (c *RealCmd) Run() (output string, err error) {
	var basepath string
	osPath := os.Getenv("PATH")
	defer func() {
		os.Setenv("PATH", osPath)
	}()

	if binfile, err := os.Executable(); err != nil {
		basepath = path.Dir(binfile)
		os.Setenv("PATH", basepath+"/bin/"+string(filepath.ListSeparator)+osPath)
	} else {
		basepath = path.Dir(os.Args[0])
		os.Setenv("PATH", basepath+string(filepath.ListSeparator)+osPath)
	}

	// Redirection using > is a shell/bash feature, not part of a command
	// Here we try to detect output redirection and if there is a redirection,
	// we need to create the output file and set cmd.Stout to that file
	args := []string{}
	outFilename := ""
	var outfile *os.File

	for _, arg := range c.args {
		if strings.HasPrefix(arg, ">") {
			outFilename = strings.TrimSpace(strings.TrimPrefix(arg, ">"))
			break
		}
		args = append(args, arg)
	}
	cmd := exec.Command(c.name, args...)
	if outFilename != "" {
		outfile, err = os.Create(outFilename)
		if err != nil {
			return "", err
		}
		defer outfile.Close()
		cmd.Stdout = outfile
	}
	// Workaround for "HOME: parameter not set"
	if os.Getenv("HOME") == "" {
		cmd.Env = append(os.Environ(), "HOME=/root")
	}

	resultChan := runCmd(cmd, outFilename)
	select {
	case <-time.After(c.Timeout):
		killErr := cmd.Process.Kill()
		if killErr != nil {
			// @todo:
			// If this happens that means leaving working process,
			// plus working goroutine waiting for that process to finish.
			// And since this command is going to be run over, and over again
			// we might end up with hundreds processes and goroutines hanging.
			// Maybe in such critical cases (or after n-cases) we should shutdown whole module (e.g. qan/mm/summary)
			// and notify us (developers), because this shouldn't happen in correct working program - but you never know
			return "", ErrKillProcessAfterTimeout
		}
		return "", ErrTimeout
	case result := <-resultChan:
		execError, ok := result.err.(*exec.Error)
		if ok && execError.Err == exec.ErrNotFound {
			return "", ErrNotFound
		}
		return result.output, result.err
	}
}

func runCmd(cmd *exec.Cmd, redirectFile string) (resultChan chan result) {
	// Below channels has buffer
	// because we might get data before we would be waiting on this channel
	resultChan = make(chan result, 1)
	go func() {
		var output []byte
		var err error

		if redirectFile == "" {
			output, err = cmd.CombinedOutput()
		} else {
			output = []byte(redirectFile)
			err = cmd.Run()
		}
		select {
		case resultChan <- result{output: string(output), err: err}:
		default:
		}
	}()
	return resultChan
}
