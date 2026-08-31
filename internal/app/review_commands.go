package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/review"
	"kit/internal/reviewstate"
	"kit/internal/ui"
)

type reviewSubmitOptions struct {
	title, description, descriptionFile            string
	draft, removeSourceBranch, confirmed, embedded bool
	pushAttempted                                  *bool
}
type reviewWaitOptions struct{ branch string }
type reviewFinishOptions struct{ branch string }
type insecureHTTPWarningContextKey struct{}

const insecureHTTPWarning = "Gitea token과 review API 데이터가 암호화되지 않은 HTTP로 전송됩니다."

func withInsecureHTTPWarningOnce(ctx context.Context) context.Context {
	if _, ok := ctx.Value(insecureHTTPWarningContextKey{}).(*sync.Once); ok {
		return ctx
	}
	return context.WithValue(ctx, insecureHTTPWarningContextKey{}, &sync.Once{})
}

func (a *Application) gitReview(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printReviewHelp(a.IO.Out)
		return nil
	}
	switch args[0] {
	case "submit":
		opts, g, err := parseReviewSubmit(global, args[1:])
		if err != nil {
			return err
		}
		return a.reviewSubmit(ctx, g, opts, nil)
	case "status":
		b, g, err := parseReviewBranchCommand(global, args[1:], "status")
		if err != nil {
			return err
		}
		return a.reviewStatus(ctx, g, b)
	case "wait":
		b, g, err := parseReviewBranchCommand(global, args[1:], "wait")
		if err != nil {
			return err
		}
		return a.reviewWait(ctx, g, reviewWaitOptions{branch: b})
	case "finish":
		b, g, err := parseReviewBranchCommand(global, args[1:], "finish")
		if err != nil {
			return err
		}
		return a.reviewFinish(ctx, g, reviewFinishOptions{branch: b})
	case "list":
		g, rest, err := parseAllGlobals(global, args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return clierror.New(clierror.Usage, "review list accepts no arguments")
		}
		return a.reviewList(ctx, g)
	default:
		return clierror.New(clierror.Usage, "unknown review command %q", args[0])
	}
}

func parseReviewSubmit(global globalOptions, args []string) (reviewSubmitOptions, globalOptions, error) {
	o := reviewSubmitOptions{removeSourceBranch: true}
	for len(args) > 0 {
		if n, e := parseGlobal(&global, args); e != nil {
			return o, global, e
		} else if n > 0 {
			args = args[n:]
			continue
		}
		if len(args) >= 2 && (args[0] == "--title" || args[0] == "--description" || args[0] == "--description-file") {
			if args[1] == "" {
				return o, global, clierror.New(clierror.Usage, "%s requires a value", args[0])
			}
			if args[0] == "--title" {
				o.title = args[1]
			} else if args[0] == "--description" {
				o.description = args[1]
			} else {
				o.descriptionFile = args[1]
			}
			args = args[2:]
			continue
		}
		switch args[0] {
		case "--draft":
			o.draft = true
		case "--wait":
		case "--remove-source-branch":
			o.removeSourceBranch = true
		case "--keep-source-branch":
			o.removeSourceBranch = false
		default:
			return o, global, clierror.New(clierror.Usage, "unknown review submit option %q", args[0])
		}
		args = args[1:]
	}
	if o.description != "" && o.descriptionFile != "" {
		return o, global, clierror.New(clierror.Usage, "--description and --description-file cannot be used together")
	}
	return o, global, nil
}

func parseReviewBranchCommand(global globalOptions, args []string, command string) (string, globalOptions, error) {
	g, rest, e := parseAllGlobals(global, args)
	if e != nil {
		return "", g, e
	}
	if len(rest) > 1 {
		return "", g, clierror.New(clierror.Usage, "review %s accepts at most one branch", command)
	}
	if len(rest) == 1 {
		return rest[0], g, nil
	}
	return "", g, nil
}

func parseAllGlobals(global globalOptions, args []string) (globalOptions, []string, error) {
	r := make([]string, 0, len(args))
	for len(args) > 0 {
		if n, e := parseGlobal(&global, args); e != nil {
			return global, nil, e
		} else if n > 0 {
			args = args[n:]
			continue
		}
		r, args = append(r, args[0]), args[1:]
	}
	return global, r, nil
}

func (a *Application) reviewSubmit(ctx context.Context, g globalOptions, o reviewSubmitOptions, override *gitservice.Service) error {
	if g.json && !g.yes && !o.confirmed {
		return clierror.New(clierror.Usage, "review submit --json requires --yes")
	}
	s, e := a.reviewGitService(ctx, g, override)
	if e != nil {
		return e
	}
	c := s.WorkflowConfig(ctx)
	_, branch, e := s.Head(ctx)
	if e != nil || branch == "" {
		return clierror.New(clierror.Failure, "review submit requires a local branch checkout")
	}
	if isProtectedReviewBranch(branch, c) {
		return clierror.New(clierror.Failure, "refusing to submit protected workflow branch %q", branch)
	}
	clean, e := s.IsClean(ctx)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "check working tree")
	}
	if !clean {
		return clierror.New(clierror.Failure, "working tree has changes; commit or stash them before review submit")
	}
	title, desc, e := a.reviewText(ctx, s, c.Base, branch, o)
	if e != nil {
		return e
	}
	if !o.confirmed && !g.yes {
		ok, e := confirm(a.IO.In, a.IO.Out, "브랜치를 push하고 PR을 생성하시겠습니까? [y/N] ")
		if e != nil {
			return e
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	repo, e := reviewRepository(ctx, s, c)
	if e != nil {
		return e
	}
	warnInsecureHTTP(ctx, a.IO.ErrOut, repo)
	client, e := a.ReviewClient(repo)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "initialize review provider")
	}
	upstream, e := s.Upstream(ctx)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "read branch upstream")
	}
	expected := c.Remote + "/" + branch
	if upstream != "" && upstream != expected {
		return clierror.New(clierror.Conflict, "current branch tracks %s, expected %s", upstream, expected)
	}
	if o.pushAttempted != nil {
		*o.pushAttempted = true
	}
	if e = s.PushCurrent(ctx, c.Remote, branch, upstream == ""); e != nil {
		return clierror.Wrap(clierror.Failure, e, "push %s", branch)
	}

	created := review.Review{}
	reused := false
	if finder, ok := client.(review.OpenFinder); ok {
		created, reused, e = finder.FindOpen(ctx, branch, c.Base)
		if e != nil {
			return clierror.Wrap(clierror.Failure, e, "check for an existing PR after push; no new PR was attempted")
		}
	}
	if !reused {
		created, e = client.Create(ctx, review.CreateRequest{SourceBranch: branch, TargetBranch: c.Base, Title: title, Description: desc, Draft: o.draft, RemoveSourceBranch: o.removeSourceBranch})
		if e != nil {
			return clierror.Wrap(clierror.Failure, e, "create PR after push; local and remote branch %s were kept, so retrying review submit is safe after checking provider status", branch)
		}
	}
	if created.ID == "" || created.URL == "" || created.Number <= 0 {
		return clierror.New(clierror.Failure, "provider returned incomplete PR metadata; local and remote branch %s were kept", branch)
	}
	state, e := savePublishedReview(ctx, s, c, branch, created)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "PR #%d exists but local review state could not be saved", created.Number)
	}
	if g.json {
		return writeJSON(a.IO.Out, state)
	}
	r := a.renderer(g)
	if !o.embedded {
		r.Command("review submit")
	}
	r.Success("Push", c.Remote+"/"+branch)
	if reused {
		r.Success("PR", fmt.Sprintf("기존 #%d 재사용 · %s", created.Number, created.URL))
	} else {
		r.Success("PR", fmt.Sprintf("#%d · %s", created.Number, created.URL))
	}
	r.Next("PR merge 후 kit review finish")
	return nil
}

func (a *Application) reviewStatus(ctx context.Context, g globalOptions, b string) error {
	s, e := a.reviewGitService(ctx, g, nil)
	if e != nil {
		return e
	}
	b, e = resolveReviewBranch(ctx, s, b)
	if e != nil {
		return e
	}
	state, e := reviewstate.Load(ctx, s, b)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "load review state")
	}
	state, refreshed, e := a.refreshReviewState(ctx, s, state)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "refresh PR #%d", state.ReviewNumber)
	}
	return a.printReviewStates(g, "review status", []reviewstate.State{state}, refreshed)
}

func (a *Application) reviewWait(_ context.Context, g globalOptions, _ reviewWaitOptions) error {
	return a.deprecatedReviewNoop(g, "wait")
}

func (a *Application) reviewFinish(ctx context.Context, g globalOptions, o reviewFinishOptions) error {
	if g.json {
		return clierror.New(clierror.Usage, "review finish does not support --json yet; use review status --json and kit sync --yes --json for automation")
	}
	s, e := a.reviewGitService(ctx, g, nil)
	if e != nil {
		return e
	}
	branch, e := resolveReviewBranch(ctx, s, o.branch)
	if e != nil {
		return e
	}
	state, e := reviewstate.Load(ctx, s, branch)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "load review state")
	}
	state, refreshed, e := a.refreshReviewState(ctx, s, state)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "refresh PR #%d", state.ReviewNumber)
	}
	if !refreshed {
		return clierror.New(clierror.Failure, "provider %q cannot refresh this cached review; verify the merge manually and run kit sync", state.Provider)
	}
	if state.Stage == reviewstate.StageCleaned {
		return a.printReviewStates(g, "review finish", []reviewstate.State{state}, true)
	}
	switch state.Status {
	case review.StatusOpen:
		return clierror.New(clierror.Conflict, "PR #%d is still open: %s", state.ReviewNumber, state.ReviewURL)
	case review.StatusClosed:
		return clierror.New(clierror.Failure, "PR #%d was closed without merge: %s", state.ReviewNumber, state.ReviewURL)
	case review.StatusMerged:
	default:
		return clierror.New(clierror.Failure, "PR #%d has unknown status %q", state.ReviewNumber, state.Status)
	}
	if !g.yes {
		ok, confirmErr := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("PR #%d merge를 확인했습니다. base/work를 동기화하고 안전한 local review branch를 정리하시겠습니까? [y/N] ", state.ReviewNumber))
		if confirmErr != nil {
			return confirmErr
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	syncGlobal := g
	syncGlobal.yes = true
	if e := a.gitSyncWithOptions(ctx, syncGlobal, syncOptions{}); e != nil {
		return clierror.Wrap(clierror.Code(e), e, "PR #%d is merged but repository sync did not finish", state.ReviewNumber)
	}
	now := time.Now().UTC()
	state.Stage = reviewstate.StageSynced
	state.SyncedAt = &now
	exists, existsErr := s.LocalBranchExists(ctx, state.Branch)
	if existsErr != nil {
		return clierror.Wrap(clierror.Failure, existsErr, "sync completed but review branch cleanup state could not be verified")
	}
	if !exists {
		state.Stage = reviewstate.StageCleaned
		state.CleanedAt = &now
	}
	if e := reviewstate.Save(ctx, s, state); e != nil {
		return clierror.Wrap(clierror.Failure, e, "sync completed but review state could not be finalized")
	}
	return a.printReviewStates(g, "review finish", []reviewstate.State{state}, true)
}

func (a *Application) deprecatedReviewNoop(g globalOptions, command string) error {
	if g.json {
		return writeJSON(a.IO.Out, map[string]any{"deprecated": true, "command": "review " + command, "next": "kit review status"})
	}
	r := a.renderer(g)
	r.Notice("호환 명령")
	r.Warning("kit review "+command, "더 이상 대기하지 않습니다. 현재 상태는 'kit review status'로 확인하고 merge 후 'kit review finish'를 실행하세요.")
	return nil
}

func (a *Application) reviewList(ctx context.Context, g globalOptions) error {
	s, e := a.reviewGitService(ctx, g, nil)
	if e != nil {
		return e
	}
	states, e := reviewstate.List(ctx, s)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "list review states")
	}
	return a.printReviewStates(g, "review list", states, false)
}

func (a *Application) printReviewStates(g globalOptions, command string, states []reviewstate.State, refreshed bool) error {
	if g.json {
		return writeJSON(a.IO.Out, states)
	}
	r := a.renderer(g)
	r.Command(command)
	r.Section("리뷰 상태")
	if len(states) == 0 {
		r.Field("State", "저장된 리뷰 상태가 없습니다.")
		return nil
	}
	for _, state := range states {
		value := fmt.Sprintf("%s → %s · %s", state.Branch, state.TargetBranch, reviewStageLabel(state.Stage))
		if state.ReviewNumber > 0 {
			value = fmt.Sprintf("%s → %s · #%d · %s", state.Branch, state.TargetBranch, state.ReviewNumber, reviewStageLabel(state.Stage))
		}
		r.Field("Review", value)
		if state.ReviewURL != "" {
			r.Field("URL", state.ReviewURL)
		}
	}
	if refreshed {
		r.Success("Provider", "현재 상태로 갱신")
	}
	return nil
}

func savePublishedReview(ctx context.Context, s gitservice.Service, c gitservice.WorkflowConfig, branch string, item review.Review) (reviewstate.State, error) {
	tip, e := s.RevisionHash(ctx, branch)
	if e != nil {
		return reviewstate.State{}, e
	}
	commits, e := s.Candidates(ctx, c.Base, branch, true)
	if e != nil {
		return reviewstate.State{}, e
	}
	hashes := make([]string, 0, len(commits))
	for _, commit := range commits {
		hashes = append(hashes, commit.Hash)
	}
	publishedTip := item.SourceSHA
	if publishedTip == "" {
		publishedTip = tip
	}
	state := reviewstate.State{
		Stage:         reviewStage(item.Status),
		Provider:      item.Provider,
		Remote:        c.Remote,
		Branch:        branch,
		SourceBranch:  c.Source,
		TargetBranch:  c.Base,
		SourceCommits: hashes,
		ReviewID:      item.ID,
		ReviewNumber:  item.Number,
		ReviewURL:     item.URL,
		Status:        item.Status,
		PickedTip:     tip,
		PublishedTip:  publishedTip,
		MergeSHA:      item.MergeSHA,
		MergedAt:      item.MergedAt,
	}
	if previous, loadErr := reviewstate.Load(ctx, s, branch); loadErr == nil {
		state.CreatedAt = previous.CreatedAt
		if previous.PickedTip != "" {
			state.PickedTip = previous.PickedTip
		}
	}
	if e := reviewstate.Save(ctx, s, state); e != nil {
		return reviewstate.State{}, e
	}
	return reviewstate.Load(ctx, s, branch)
}

func (a *Application) refreshReviewState(ctx context.Context, s gitservice.Service, state reviewstate.State) (reviewstate.State, bool, error) {
	if state.ReviewNumber <= 0 {
		return state, false, nil
	}
	c := s.WorkflowConfig(ctx)
	repo, e := reviewRepository(ctx, s, c)
	if e != nil {
		return state, false, e
	}
	if state.Provider != "" && repo.Provider != state.Provider {
		return state, false, fmt.Errorf("configured provider %q does not match saved review provider %q", repo.Provider, state.Provider)
	}
	warnInsecureHTTP(ctx, a.IO.ErrOut, repo)
	client, e := a.ReviewClient(repo)
	if e != nil {
		return state, false, e
	}
	getter, ok := client.(review.Getter)
	if !ok {
		return state, false, nil
	}
	item, e := getter.Get(ctx, state.ReviewNumber)
	if e != nil {
		return state, true, e
	}
	if item.SourceBranch != state.Branch || item.TargetBranch != state.TargetBranch {
		return state, true, fmt.Errorf("provider review branches changed: got %s → %s, expected %s → %s", item.SourceBranch, item.TargetBranch, state.Branch, state.TargetBranch)
	}
	state.Provider = item.Provider
	state.ReviewID = item.ID
	state.ReviewNumber = item.Number
	state.ReviewURL = item.URL
	state.Status = item.Status
	if item.SourceSHA != "" {
		state.PublishedTip = item.SourceSHA
	}
	state.MergeSHA = item.MergeSHA
	state.MergedAt = item.MergedAt
	if !(item.Status == review.StatusMerged && (state.Stage == reviewstate.StageSynced || state.Stage == reviewstate.StageCleaned)) {
		state.Stage = reviewStage(item.Status)
	}
	if e := reviewstate.Save(ctx, s, state); e != nil {
		return state, true, e
	}
	updated, e := reviewstate.Load(ctx, s, state.Branch)
	return updated, true, e
}

func reviewStage(status review.Status) reviewstate.Stage {
	switch status {
	case review.StatusMerged:
		return reviewstate.StageMerged
	case review.StatusClosed:
		return reviewstate.StageClosed
	default:
		return reviewstate.StageOpen
	}
}

func (a *Application) reviewGitService(ctx context.Context, g globalOptions, o *gitservice.Service) (gitservice.Service, error) {
	if o != nil {
		return *o, nil
	}
	return a.validatedGit(ctx, g.cwd)
}

func reviewRepository(ctx context.Context, s gitservice.Service, c gitservice.WorkflowConfig) (hosting.Repository, error) {
	u, e := s.RemoteURL(ctx, c.Remote)
	if e != nil {
		return hosting.Repository{}, clierror.Wrap(clierror.Failure, e, "read remote %s", c.Remote)
	}
	r := hosting.Resolve(c.Provider, u)
	r.AllowInsecureHTTP = c.AllowInsecureHTTP
	if r.Provider != "gitea" && r.Provider != "gitlab" && r.Provider != "forgejo" {
		return hosting.Repository{}, clierror.New(clierror.Failure, "provider %q does not support automated reviews", r.Provider)
	}
	return r, nil
}

func warnInsecureHTTP(ctx context.Context, w io.Writer, r hosting.Repository) {
	if !r.InsecureHTTPAllowed() {
		return
	}
	f := func() { u := ui.Renderer{Writer: w}; u.Notice("보안 경고"); u.Warning("HTTP", insecureHTTPWarning) }
	if once, ok := ctx.Value(insecureHTTPWarningContextKey{}).(*sync.Once); ok {
		once.Do(f)
		return
	}
	f()
}

func resolveReviewBranch(ctx context.Context, s gitservice.Service, b string) (string, error) {
	if b != "" {
		return b, nil
	}
	_, current, e := s.Head(ctx)
	if e == nil && current != "" {
		if _, e = reviewstate.Load(ctx, s, current); e == nil {
			return current, nil
		}
	}
	states, e := reviewstate.List(ctx, s)
	if e != nil {
		return "", clierror.Wrap(clierror.Failure, e, "list review states")
	}
	active := make([]reviewstate.State, 0, len(states))
	for _, state := range states {
		if state.Stage != reviewstate.StageCleaned {
			active = append(active, state)
		}
	}
	if len(active) == 1 {
		return active[0].Branch, nil
	}
	if len(active) == 0 {
		return "", clierror.New(clierror.Failure, "no active review state was found")
	}
	return "", clierror.New(clierror.Usage, "multiple active reviews exist; specify a branch")
}

func (a *Application) reviewText(ctx context.Context, s gitservice.Service, target, branch string, o reviewSubmitOptions) (string, string, error) {
	commits, e := s.Candidates(ctx, target, branch, true)
	if e != nil {
		return "", "", clierror.Wrap(clierror.Failure, e, "list review commits")
	}
	title := strings.TrimSpace(o.title)
	if title == "" && len(commits) > 0 {
		title = commits[0].Subject
	}
	if title == "" {
		return "", "", clierror.New(clierror.Usage, "review title is required; use --title")
	}
	d := o.description
	if o.descriptionFile != "" {
		p := o.descriptionFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(s.Dir, p)
		}
		data, e := readReviewDescription(p)
		if e != nil {
			return "", "", clierror.Wrap(clierror.Failure, e, "read review description file")
		}
		d = string(data)
	}
	if d == "" {
		var b strings.Builder
		b.WriteString("## 변경 커밋\n\n")
		for _, c := range commits {
			fmt.Fprintf(&b, "- `%s` %s\n", c.ShortHash, c.Subject)
		}
		d = b.String()
	}
	return title, d, nil
}

func readReviewDescription(p string) ([]byte, error) {
	i, e := os.Lstat(p)
	if e != nil {
		return nil, e
	}
	if !i.Mode().IsRegular() {
		return nil, errors.New("description path must be a regular file (symlinks and special files are not allowed)")
	}
	if i.Size() > 1<<20 {
		return nil, errors.New("review description file exceeds 1 MiB")
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New("description path changed before it could be read")
	}
	if !os.SameFile(i, opened) {
		return nil, errors.New("description path changed before it could be read")
	}
	data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(data) > 1<<20 {
		return nil, errors.New("review description file exceeds 1 MiB")
	}
	return data, nil
}

func isProtectedReviewBranch(b string, c gitservice.WorkflowConfig) bool {
	return b == c.Stable || b == c.Base || b == c.Source
}

func printReviewHelp(w io.Writer) {
	fmt.Fprint(w, "Usage: kit review <command>\n\nCommands:\n  submit [options]    Push the current branch and create or reuse a Gitea PR\n  status [branch]     Refresh and show the saved PR state when supported\n  wait [branch]       Deprecated no-op; use status instead\n  finish [branch]     Verify merge, sync work, and finalize local cleanup\n  list                List locally saved review states\n")
}
