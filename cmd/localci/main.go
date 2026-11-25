// Command localci runs a project's CI checks locally on an exact commit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/PrashantsCodeRepo/LocalCI/internal/gitx"
	"github.com/PrashantsCodeRepo/LocalCI/internal/localci"
)

var version = "dev"

const usage = `localci runs a project's CI checks locally on an exact commit.

Usage:
  localci run [--tier full] [--status] [<rev>]   check a commit (default HEAD)
  localci push [--tier full] [-- <git push args>] check HEAD, git push, post the status
  localci hook install [--force]                  write a pre-push hook that runs the checks
  localci version

The checks come from .localci.yml in the commit itself.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	// Git exports GIT_DIR and friends to hooks; clear them before anything else.
	if err := gitx.ClearLocalEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "localci:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:])
	case "push":
		return cmdPush(ctx, args[1:])
	case "hook":
		return cmdHook(ctx, args[1:])
	case "version", "--version":
		fmt.Println("localci", version)
		return 0
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "localci: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func cmdRun(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	tier := fs.String("tier", "", `"full" also runs tier: full steps`)
	status := fs.Bool("status", false, "post a GitHub commit status (the commit must be on GitHub)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "localci run: at most one revision")
		return 2
	}
	o, err := localci.Run(ctx, localci.Options{Rev: fs.Arg(0), Tier: *tier})
	if *status && o != nil && o.SHA != "" {
		if perr := localci.Post(context.Background(), o, err, os.Stdout); perr != nil {
			fmt.Fprintln(os.Stderr, "localci:", perr)
			return 1
		}
	}
	return exitCode(o, err)
}

func cmdPush(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	tier := fs.String("tier", "", `"full" also runs tier: full steps`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	o, err := localci.Run(ctx, localci.Options{Tier: *tier})
	if code := exitCode(o, err); code != 0 {
		fmt.Fprintln(os.Stderr, "localci: not pushing")
		return code
	}
	push := exec.CommandContext(ctx, "git", append([]string{"push"}, fs.Args()...)...)
	push.Dir = o.Repo
	push.Env = append(os.Environ(), "LOCALCI_SKIP=1") // the pre-push hook would run it all again
	push.Stdin, push.Stdout, push.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := push.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "localci: git push failed:", err)
		return 1
	}
	if err := localci.Post(context.Background(), o, nil, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "localci:", err)
		return 1
	}
	return 0
}

func cmdHook(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "install" {
		fmt.Fprintln(os.Stderr, "usage: localci hook install [--force]")
		return 2
	}
	fs := flag.NewFlagSet("hook install", flag.ContinueOnError)
	force := fs.Bool("force", false, "replace an existing pre-push hook")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	path, err := localci.InstallHook(ctx, ".", *force)
	if err != nil {
		fmt.Fprintln(os.Stderr, "localci:", err)
		return 1
	}
	fmt.Println("localci: wrote", path)
	return 0
}

func exitCode(o *localci.Outcome, err error) int {
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "localci:", err)
		}
		return 2
	}
	if o == nil || !o.Passed {
		return 1
	}
	return 0
}
