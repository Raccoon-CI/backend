package execute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/archive"
	"github.com/Raccoon-CI/raccoonci/internal/dispatch"
)

// Local runs a job on the machine the server runs on, as this user, by
// starting `raccoonci run-instance` in a fresh directory with the source
// unpacked: what an agent does on a machine, without the machine. There is no
// isolation at all: the job's steps can read everything this user can. It is
// for developing the server and for a server on a machine that exists for
// this, with repositories whose code can be trusted; never for anyone else's.
type Local struct {
	// Binary is the raccoonci executable that has `run-instance`.
	Binary string
	// Dir is where each job gets a directory (removed afterwards).
	Dir string
}

// Clean removes what jobs left in Dir when the server died; the executor calls
// it at start, before any job runs.
func (l Local) Clean() error { return removeContents(l.Dir) }

// maxLocalSource caps what a job's source may unpack to.
const maxLocalSource = 2 << 30

// Run implements Machine.
func (l Local) Run(ctx context.Context, name string, job dispatch.Job) (dispatch.Result, error) {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return dispatch.Result{}, err
	}
	dir, err := os.MkdirTemp(l.Dir, name+"-")
	if err != nil {
		return dispatch.Result{}, err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o700); err != nil {
		return dispatch.Result{}, err
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(job.Source(pw)) }()
	err = archive.ExtractTarGz(pr, src, maxLocalSource)
	pr.CloseWithError(err) // a failed unpack must not leave the writer waiting
	if err != nil {
		return dispatch.Result{Error: "unpacking the source: " + err.Error()}, nil
	}

	command := job.Command
	if command == "" {
		command = "run"
	}
	args := append([]string{command}, job.Args...)
	env := os.Environ()
	for k, v := range job.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range job.Secrets {
		env = append(env, k+"="+v)
		args = append(args, "-secret", k)
	}
	cmd := exec.CommandContext(ctx, l.Binary, args...)
	cmd.Dir = src
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(job.Input)
	var out io.Writer = io.Discard
	if job.Log != nil {
		out = job.Log
	}
	cmd.Stdout, cmd.Stderr = out, out
	// A cancelled job is interrupted first, so its always() steps can run.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return dispatch.Result{}, nil
	case errors.As(err, &exit):
		return dispatch.Result{ExitCode: exit.ExitCode()}, nil
	case ctx.Err() != nil:
		return dispatch.Result{}, ctx.Err()
	default:
		return dispatch.Result{Error: fmt.Sprintf("starting %s: %v", l.Binary, err)}, nil
	}
}
