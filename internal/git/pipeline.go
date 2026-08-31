package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

type PipelineRunner interface {
	RunPipeline(ctx context.Context, dir string, leftArgs, rightArgs []string) ([]byte, error)
}

func (ExecRunner) RunPipeline(ctx context.Context, dir string, leftArgs, rightArgs []string) ([]byte, error) {
	left := exec.CommandContext(ctx, "git", leftArgs...)
	right := exec.CommandContext(ctx, "git", rightArgs...)
	left.Dir, right.Dir = dir, dir
	env := gitEnvironment(os.Environ())
	left.Env, right.Env = env, env

	pipe, err := left.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("connect Git pipeline: %w", err)
	}
	right.Stdin = pipe
	var leftErr, rightErr bytes.Buffer
	var output bytes.Buffer
	left.Stderr = &leftErr
	right.Stderr = &rightErr
	right.Stdout = &output

	if err := right.Start(); err != nil {
		return nil, commandFailure(rightArgs, rightErr.String(), err)
	}
	if err := left.Start(); err != nil {
		_ = right.Process.Kill()
		_ = right.Wait()
		return nil, commandFailure(leftArgs, leftErr.String(), err)
	}
	leftRunErr := left.Wait()
	rightRunErr := right.Wait()
	if leftRunErr != nil {
		return output.Bytes(), commandFailure(leftArgs, leftErr.String(), leftRunErr)
	}
	if rightRunErr != nil {
		return output.Bytes(), commandFailure(rightArgs, rightErr.String(), rightRunErr)
	}
	return output.Bytes(), nil
}

func (s Service) runPipeline(ctx context.Context, leftArgs, rightArgs []string) ([]byte, error) {
	runner := s.runner()
	if pipeline, ok := runner.(PipelineRunner); ok {
		return pipeline.RunPipeline(ctx, s.Dir, leftArgs, rightArgs)
	}
	// Custom test runners and embedders that implement only Runner retain the
	// previous buffered behavior. Production ExecRunner uses the streaming path.
	left, err := runner.Run(ctx, s.Dir, leftArgs...)
	if err != nil {
		return nil, err
	}
	return runner.RunInput(ctx, s.Dir, left, rightArgs...)
}

func (r TraceRunner) RunPipeline(ctx context.Context, dir string, leftArgs, rightArgs []string) ([]byte, error) {
	if r.Writer != nil {
		fmt.Fprintf(r.Writer, "+ git %s | git %s\n", SafeCommand(leftArgs), SafeCommand(rightArgs))
	}
	base := r.base()
	if pipeline, ok := base.(PipelineRunner); ok {
		return pipeline.RunPipeline(ctx, dir, leftArgs, rightArgs)
	}
	left, err := base.Run(ctx, dir, leftArgs...)
	if err != nil {
		return nil, err
	}
	return base.RunInput(ctx, dir, left, rightArgs...)
}
