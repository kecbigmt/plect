package population

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/kecbigmt/plecture/app/internal/cachehome"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/datahome"
	"github.com/kecbigmt/plecture/app/internal/lang"
)

type queryRunner interface {
	Poll(context.Context, Definition) ([]map[string]any, error)
	Subscribe(context.Context, Definition, func(map[string]any) error) error
}

type actionRunner struct {
	cfg *config.Config
}

func (r actionRunner) Poll(ctx context.Context, def Definition) ([]map[string]any, error) {
	execution, cleanup, err := r.resolve(def, def.Observer.Query.Poll)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cmd := commandContext(ctx, execution)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("query poll: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	var items []map[string]any
	if err := json.Unmarshal(stdout, &items); err != nil {
		return nil, fmt.Errorf("query poll output is not a JSON item array: %w", err)
	}
	if items == nil {
		return nil, fmt.Errorf("query poll output is not a JSON item array")
	}
	return items, nil
}

func (r actionRunner) Subscribe(ctx context.Context, def Definition, emit func(map[string]any) error) error {
	execution, cleanup, err := r.resolve(def, def.Observer.Query.Subscribe)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd := commandContext(ctx, execution)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var item map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("query subscribe line is not a JSON item: %w", err)
		}
		if err := emit(item); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("query subscribe: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return fmt.Errorf("query subscribe exited")
}

func (r actionRunner) resolve(def Definition, action *lang.Action) (*lang.Execution, func(), error) {
	dir, err := os.MkdirTemp("", "plect-population-query-")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	bins := config.MountedBins{Mounted: r.cfg.Plugins, SourcePath: def.Observer.SourcePath}
	eval := lang.Eval{
		Roots: lang.Roots{"inputs": def.Population.Query},
		Bin: func(ref string) (string, error) {
			return bins.ResolveBin(ref, def.Observer.Ownership())
		},
	}
	execution, err := eval.Run(filepath.Join(dir, "action"), action, nil)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return execution, cleanup, nil
}

func commandContext(ctx context.Context, execution *lang.Execution) *exec.Cmd {
	cmd := exec.CommandContext(ctx, execution.Argv[0], execution.Argv[1:]...)
	// A resource observer's poll/subscribe query is a declaration-driven
	// child the same way a task's setup/cleanup is: see the datahome
	// package for why this process's own PLECT_DATA_HOME must not leak
	// into it (XDG_DATA_HOME is deliberately left alone — an observer may
	// already depend on inheriting it for its own unrelated on-disk state).
	// PLECT_CACHE_HOME is stripped the same way, for the same reason.
	cmd.Env = cachehome.Strip(datahome.InheritableEnv())
	if len(execution.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(execution.Stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}
