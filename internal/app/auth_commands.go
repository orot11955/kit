package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/term"

	"kit/internal/auth"
	"kit/internal/clierror"
	"kit/internal/hosting"
	"kit/internal/review"
	"kit/internal/ui"
)

type AuthService interface {
	Login(provider, host, token string, store auth.Store) (auth.Profile, error)
	Lookup(provider, host string) (string, error)
	Status(provider, host string) (auth.Profile, error)
	List() ([]auth.Profile, error)
	Logout(provider, host string) (auth.Profile, error)
	ConfigDir() string
}

type authTargetOptions struct {
	globalOptions
	provider string
	host     string
	store    auth.Store
}

func (a *Application) authCommand(_ context.Context, global globalOptions, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAuthHelp(a.IO.Out)
		return nil
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "login":
		opts, help, err := parseAuthTarget(global, rest, true)
		if err != nil {
			return err
		}
		if help {
			printAuthLoginHelp(a.IO.Out)
			return nil
		}
		return a.authLogin(opts)
	case "status":
		opts, help, err := parseAuthTarget(global, rest, false)
		if err != nil {
			return err
		}
		if help {
			fmt.Fprintln(a.IO.Out, "Usage: kit auth status gitea --host <host> [--json]")
			return nil
		}
		return a.authStatus(opts)
	case "list":
		parsed, help, err := parseAuthList(global, rest)
		if err != nil {
			return err
		}
		if help {
			fmt.Fprintln(a.IO.Out, "Usage: kit auth list [--json]")
			return nil
		}
		return a.authList(parsed)
	case "logout":
		opts, help, err := parseAuthTarget(global, rest, false)
		if err != nil {
			return err
		}
		if help {
			fmt.Fprintln(a.IO.Out, "Usage: kit auth logout gitea --host <host> [--yes] [--json]")
			return nil
		}
		return a.authLogout(opts)
	default:
		return clierror.New(clierror.Usage, "unknown auth command %q", subcommand)
	}
}

func parseAuthTarget(global globalOptions, args []string, allowStore bool) (authTargetOptions, bool, error) {
	opts := authTargetOptions{globalOptions: global, store: auth.StoreAuto}
	positionals := make([]string, 0, 1)
	for len(args) > 0 {
		arg := args[0]
		if arg == "-h" || arg == "--help" {
			return opts, true, nil
		}
		if consumed, err := parseGlobal(&opts.globalOptions, args); err != nil {
			return opts, false, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch {
		case arg == "--host":
			if len(args) < 2 || args[1] == "" {
				return opts, false, clierror.New(clierror.Usage, "--host requires exact lowercase host[:port]")
			}
			opts.host, args = args[1], args[2:]
		case strings.HasPrefix(arg, "--host="):
			opts.host, args = strings.TrimPrefix(arg, "--host="), args[1:]
		case allowStore && arg == "--store":
			if len(args) < 2 {
				return opts, false, clierror.New(clierror.Usage, "--store requires auto, keyring, or file")
			}
			store, err := auth.ParseStore(args[1])
			if err != nil {
				return opts, false, clierror.Wrap(clierror.Usage, err, "invalid --store")
			}
			opts.store, args = store, args[2:]
		case allowStore && strings.HasPrefix(arg, "--store="):
			store, err := auth.ParseStore(strings.TrimPrefix(arg, "--store="))
			if err != nil {
				return opts, false, clierror.Wrap(clierror.Usage, err, "invalid --store")
			}
			opts.store, args = store, args[1:]
		case strings.HasPrefix(arg, "-"):
			return opts, false, clierror.New(clierror.Usage, "unknown auth option %q", arg)
		default:
			positionals, args = append(positionals, arg), args[1:]
		}
	}
	if len(positionals) != 1 || positionals[0] != auth.ProviderGitea {
		return opts, false, clierror.New(clierror.Usage, "auth requires the canonical provider gitea")
	}
	opts.provider = positionals[0]
	if opts.host == "" {
		return opts, false, clierror.New(clierror.Usage, "--host is required")
	}
	if _, _, err := auth.NormalizeProfile(opts.provider, opts.host); err != nil {
		return opts, false, clierror.Wrap(clierror.Usage, err, "invalid auth profile")
	}
	return opts, false, nil
}

func parseAuthList(global globalOptions, args []string) (globalOptions, bool, error) {
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			return global, true, nil
		}
		consumed, err := parseGlobal(&global, args)
		if err != nil {
			return global, false, err
		}
		if consumed == 0 {
			return global, false, clierror.New(clierror.Usage, "auth list accepts no arguments")
		}
		args = args[consumed:]
	}
	return global, false, nil
}

func (a *Application) authLogin(opts authTargetOptions) error {
	service, err := a.authService()
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "initialize credential storage")
	}
	token, err := a.ReadSecret("Gitea token: ")
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read Gitea token")
	}
	if token == "" {
		return clierror.New(clierror.Usage, "Gitea token must not be empty")
	}
	profile, err := service.Login(opts.provider, opts.host, token, opts.store)
	if err != nil && opts.store == auth.StoreAuto && runtime.GOOS == "linux" && errors.Is(err, auth.ErrKeyringUnavailable) {
		confirmed, confirmErr := confirm(a.IO.In, a.IO.ErrOut, "Secret Service를 사용할 수 없습니다. 0600 파일 저장소를 사용하시겠습니까? [y/N] ")
		if confirmErr != nil {
			return confirmErr
		}
		if !confirmed {
			return clierror.New(clierror.Failure, "credential was not stored")
		}
		profile, err = service.Login(opts.provider, opts.host, token, auth.StoreFile)
	}
	if err != nil {
		if opts.store == auth.StoreAuto && runtime.GOOS == "darwin" {
			return clierror.Wrap(clierror.Failure, err,
				"store Gitea credential; unlock the login Keychain and retry, or explicitly use --store file")
		}
		return clierror.Wrap(clierror.Failure, err, "store Gitea credential")
	}
	if profile.Store == auth.StoreFile {
		fmt.Fprintf(a.IO.ErrOut, "경고: token이 OS keyring이 아닌 평문 파일로 저장되었습니다. 디렉터리: %s (0700, credential files 0600)\n", ui.SafeText(service.ConfigDir()))
	}
	if opts.json {
		return writeJSON(a.IO.Out, profile)
	}
	renderer := a.renderer(opts.globalOptions)
	renderer.Command("auth login")
	renderer.Success("Provider", profile.Provider)
	renderer.Field("Host", profile.Host)
	renderer.Field("Store", string(profile.Store))
	return nil
}

func (a *Application) authStatus(opts authTargetOptions) error {
	service, err := a.authService()
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "initialize credential storage")
	}
	profile, err := service.Status(opts.provider, opts.host)
	if errors.Is(err, auth.ErrCredentialNotFound) {
		return clierror.New(clierror.Failure, "credential not found; run kit auth login gitea --host %s", ui.ShellQuote(opts.host))
	}
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read credential status")
	}
	if opts.json {
		return writeJSON(a.IO.Out, profile)
	}
	renderer := a.renderer(opts.globalOptions)
	renderer.Command("auth status")
	renderer.Success("Provider", profile.Provider)
	renderer.Field("Host", profile.Host)
	renderer.Field("Store", string(profile.Store))
	return nil
}

func (a *Application) authList(global globalOptions) error {
	service, err := a.authService()
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "initialize credential storage")
	}
	profiles, err := service.List()
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list credential profiles")
	}
	if global.json {
		return writeJSON(a.IO.Out, profiles)
	}
	renderer := a.renderer(global)
	renderer.Command("auth list")
	if len(profiles) == 0 {
		renderer.Field("Profiles", "none")
		return nil
	}
	for _, profile := range profiles {
		renderer.Field(profile.Provider, profile.Host+" · "+string(profile.Store))
	}
	return nil
}

func (a *Application) authLogout(opts authTargetOptions) error {
	if opts.json && !opts.yes {
		return clierror.New(clierror.Usage, "auth logout --json requires --yes")
	}
	if !opts.yes {
		prompt := fmt.Sprintf("%s의 Gitea credential을 삭제하시겠습니까? [y/N] ", ui.SafeText(opts.host))
		confirmed, err := confirm(a.IO.In, a.IO.Out, prompt)
		if err != nil {
			return err
		}
		if !confirmed {
			return nil
		}
	}
	service, err := a.authService()
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "initialize credential storage")
	}
	profile, err := service.Logout(opts.provider, opts.host)
	if errors.Is(err, auth.ErrCredentialNotFound) {
		return clierror.New(clierror.Failure, "credential not found for %s", ui.ShellQuote(opts.host))
	}
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "delete Gitea credential")
	}
	if opts.json {
		return writeJSON(a.IO.Out, profile)
	}
	renderer := a.renderer(opts.globalOptions)
	renderer.Command("auth logout")
	renderer.Success("Removed", profile.Provider)
	renderer.Field("Host", profile.Host)
	return nil
}

func (a *Application) authService() (AuthService, error) {
	if a.Auth != nil {
		return a.Auth, nil
	}
	service, err := a.AuthInit()
	if err != nil {
		return nil, err
	}
	a.Auth = service
	return service, nil
}

func (a *Application) newReviewClient(repository hosting.Repository) (review.Client, error) {
	if strings.ToLower(repository.Provider) != auth.ProviderGitea {
		return review.NewClient(repository)
	}
	service, err := a.authService()
	if err != nil {
		return nil, err
	}
	return review.NewClientWithOptions(repository, review.Options{Lookup: func(provider, host string) (string, error) {
		return a.lookupReviewCredential(service, provider, host)
	}})
}

func (a *Application) lookupReviewCredential(service AuthService, provider, host string) (string, error) {
	token, err := service.Lookup(provider, host)
	if !errors.Is(err, auth.ErrKeychainInteractionRequired) {
		return token, err
	}
	if !a.keychainUnlockSupported() || !a.allowKeychainUnlock || a.IO.InFile == nil || !a.keychainTTY(a.IO.InFile) {
		return "", fmt.Errorf("%w; run security unlock-keychain \"$HOME/Library/Keychains/login.keychain-db\" in an interactive terminal, then retry", err)
	}

	fmt.Fprintln(a.IO.ErrOut, "kit · auth")
	fmt.Fprintln(a.IO.ErrOut, "\n  ! Keychain    login Keychain 잠금을 해제합니다. macOS 비밀번호를 입력하세요.")
	if unlockErr := a.keychainUnlocker()(a.IO.InFile); unlockErr != nil {
		return "", fmt.Errorf("unlock macOS login Keychain: %w", unlockErr)
	}
	return service.Lookup(provider, host)
}

func (a *Application) keychainUnlockSupported() bool {
	if a.KeychainSupported != nil {
		return a.KeychainSupported()
	}
	return runtime.GOOS == "darwin"
}

func (a *Application) keychainTTY(input *os.File) bool {
	if a.IsKeychainTTY != nil {
		return a.IsKeychainTTY(input)
	}
	return isKeychainTerminal(input)
}

func (a *Application) keychainUnlocker() func(*os.File) error {
	if a.UnlockKeychain != nil {
		return a.UnlockKeychain
	}
	return unlockDarwinLoginKeychain
}

func isKeychainTerminal(input *os.File) bool {
	return input != nil && term.IsTerminal(int(input.Fd()))
}

func unlockDarwinLoginKeychain(terminal *os.File) error {
	if terminal == nil {
		return errors.New("interactive terminal is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	keychainPath := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	command := exec.Command("/usr/bin/security", "unlock-keychain", keychainPath)
	command.Stdin = terminal
	command.Stdout = terminal
	command.Stderr = terminal
	return command.Run()
}

func terminalSecretReader(input *os.File, output io.Writer) func(string) (string, error) {
	return func(prompt string) (string, error) {
		if input == nil || !term.IsTerminal(int(input.Fd())) {
			return "", errors.New("token input requires a TTY")
		}
		fmt.Fprint(output, prompt)
		secret, err := term.ReadPassword(int(input.Fd()))
		fmt.Fprintln(output)
		if err != nil {
			return "", err
		}
		return string(secret), nil
	}
}

func printAuthHelp(writer io.Writer) {
	fmt.Fprint(writer, `Usage: kit auth <command>

Commands:
  login gitea    Store a Gitea credential
  status gitea   Show one stored profile
  list           List stored profiles
  logout gitea   Delete one stored profile
`)
}

func printAuthLoginHelp(writer io.Writer) {
	fmt.Fprint(writer, `Usage: kit auth login gitea --host <host> [--store auto|keyring|file] [--json]

The token is read from a hidden TTY prompt. File storage is an explicit
macOS/Linux fallback and is never selected automatically on macOS.
`)
}
