package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"kit/internal/auth"
	"kit/internal/buildinfo"
	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/review"
	"kit/internal/selector"
	"kit/internal/update"
)

type IO struct {
	In      io.Reader
	Out     io.Writer
	ErrOut  io.Writer
	InFile  *os.File
	OutFile *os.File
}

type Application struct {
	IO                         IO
	Git                        func(dir string) gitservice.Service
	Select                     func([]selector.Item, string) ([]selector.Item, error)
	Update                     func(context.Context, update.Config) (update.Result, error)
	ReviewClient               func(hosting.Repository) (review.Client, error)
	Auth                       AuthService
	AuthInit                   func() (AuthService, error)
	ReadSecret                 func(prompt string) (string, error)
	UnlockKeychain             func(*os.File) error
	IsKeychainTTY              func(*os.File) bool
	KeychainSupported          func() bool
	Build                      buildinfo.Info
	ExecPath                   string
	statusReviewRefreshTimeout time.Duration
	allowKeychainUnlock        bool
}

func New(input *os.File, output, errOutput *os.File) *Application {
	terminal := selector.Terminal{In: input, Out: output}
	application := &Application{
		IO: IO{In: input, Out: output, ErrOut: errOutput, InFile: input, OutFile: output},
		Git: func(dir string) gitservice.Service {
			return gitservice.Service{Dir: dir}
		},
		Select:   terminal.Select,
		Update:   update.Run,
		AuthInit: func() (AuthService, error) { return auth.NewDefault() },
		Build:    buildinfo.Current(),
	}
	application.ReadSecret = terminalSecretReader(input, errOutput)
	application.UnlockKeychain = unlockDarwinLoginKeychain
	application.IsKeychainTTY = isKeychainTerminal
	application.KeychainSupported = func() bool { return runtime.GOOS == "darwin" }
	application.ReviewClient = application.newReviewClient
	return application
}

type globalOptions struct {
	cwd     string
	json    bool
	noColor bool
	yes     bool
}

func (a *Application) Run(ctx context.Context, args []string) error {
	if a.IO.In == nil {
		a.IO.In = strings.NewReader("")
	}
	if a.IO.Out == nil {
		a.IO.Out = io.Discard
	}
	if a.IO.ErrOut == nil {
		a.IO.ErrOut = io.Discard
	}
	if a.Git == nil {
		a.Git = func(dir string) gitservice.Service { return gitservice.Service{Dir: dir} }
	}
	if a.Update == nil {
		a.Update = update.Run
	}
	if a.AuthInit == nil {
		a.AuthInit = func() (AuthService, error) { return auth.NewDefault() }
	}
	if a.ReadSecret == nil {
		a.ReadSecret = terminalSecretReader(a.IO.InFile, a.IO.ErrOut)
	}
	if a.UnlockKeychain == nil {
		a.UnlockKeychain = unlockDarwinLoginKeychain
	}
	if a.IsKeychainTTY == nil {
		a.IsKeychainTTY = isKeychainTerminal
	}
	if a.KeychainSupported == nil {
		a.KeychainSupported = func() bool { return runtime.GOOS == "darwin" }
	}
	if a.ReviewClient == nil {
		a.ReviewClient = a.newReviewClient
	}
	if a.Build.Version == "" {
		a.Build = buildinfo.Current()
	}

	ctx, args, verbose := stripVerboseFlag(ctx, args)
	if verbose {
		restoreGit := a.enableVerboseGit()
		defer restoreGit()
	}
	global, command, rest, err := parseRoot(args)
	if err != nil {
		return err
	}
	previousKeychainUnlock := a.allowKeychainUnlock
	a.allowKeychainUnlock = !global.json && !argumentsContainJSON(rest)
	defer func() { a.allowKeychainUnlock = previousKeychainUnlock }()
	switch command {
	case "", "help":
		printRootHelp(a.IO.Out)
		return nil
	case "status":
		return a.statusCommand(ctx, global, rest)
	case "sync":
		return a.syncCommand(ctx, global, rest)
	case "review":
		return a.gitReview(ctx, global, rest)
	case "backup":
		return a.backupCommand(ctx, global, rest)
	case "git":
		return a.gitCommand(ctx, global, rest)
	case "self":
		return a.selfCommand(ctx, global, rest)
	case "config":
		return a.configCommand(ctx, global, rest)
	case "auth":
		return a.authCommand(ctx, global, rest)
	case "compare":
		return a.compare(ctx, global, rest)
	case "pick":
		return a.pickEnhanced(ctx, global, rest)
	case "version":
		return a.version(global, rest)
	case "update":
		return a.update(ctx, global, rest)
	case "doctor":
		return a.doctorEnhanced(ctx, global, rest)
	default:
		return clierror.New(clierror.Usage, "unknown command %q\nRun 'kit help' for usage.", command)
	}
}

func argumentsContainJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" {
			return true
		}
	}
	return false
}

func parseRoot(args []string) (globalOptions, string, []string, error) {
	opts := globalOptions{cwd: "."}
	for len(args) > 0 {
		arg := args[0]
		if arg == "-h" || arg == "--help" {
			return opts, "help", nil, nil
		}
		if !strings.HasPrefix(arg, "-") {
			return opts, arg, args[1:], nil
		}
		var consumed int
		var err error
		consumed, err = parseGlobal(&opts, args)
		if err != nil {
			return opts, "", nil, err
		}
		if consumed == 0 {
			return opts, "", nil, clierror.New(clierror.Usage, "unknown global option %q", arg)
		}
		args = args[consumed:]
	}
	return opts, "", nil, nil
}

func parseGlobal(opts *globalOptions, args []string) (int, error) {
	arg := args[0]
	switch arg {
	case "--json":
		opts.json = true
		return 1, nil
	case "--no-color":
		opts.noColor = true
		return 1, nil
	case "--yes":
		opts.yes = true
		return 1, nil
	case "--cwd":
		if len(args) < 2 || args[1] == "" {
			return 0, clierror.New(clierror.Usage, "--cwd requires a path")
		}
		opts.cwd = args[1]
		return 2, nil
	default:
		if strings.HasPrefix(arg, "--cwd=") {
			opts.cwd = strings.TrimPrefix(arg, "--cwd=")
			if opts.cwd == "" {
				return 0, clierror.New(clierror.Usage, "--cwd requires a path")
			}
			return 1, nil
		}
	}
	return 0, nil
}

func (a *Application) validatedGit(ctx context.Context, dir string, revisions ...string) (gitservice.Service, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return gitservice.Service{}, clierror.Wrap(clierror.Failure, err, "resolve --cwd")
	}
	service := a.Git(absolute)
	if err := service.ValidateDependency(ctx); err != nil {
		return service, clierror.Wrap(clierror.Failure, err, "Git dependency check failed")
	}
	if err := service.ValidateRepository(ctx); err != nil {
		return service, clierror.Wrap(clierror.Failure, err, "%s is not a Git repository", absolute)
	}
	for _, revision := range revisions {
		if err := service.VerifyRevision(ctx, revision); err != nil {
			return service, clierror.Wrap(clierror.Failure, err, "branch or revision %q was not found", revision)
		}
	}
	return service, nil
}

func verifyRevisions(ctx context.Context, service gitservice.Service, revisions ...string) error {
	for _, revision := range revisions {
		if err := service.VerifyRevision(ctx, revision); err != nil {
			return clierror.Wrap(clierror.Failure, err, "branch or revision %q was not found", revision)
		}
	}
	return nil
}
