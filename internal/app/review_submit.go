package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
)

type reviewSubmitOptions struct {
	title, description, descriptionFile            string
	draft, removeSourceBranch, confirmed, embedded bool
	pushAttempted                                  *bool
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
	repositories, e := resolveReviewRepositories(ctx, s, c)
	if e != nil {
		return e
	}
	warnInsecureHTTP(ctx, a.IO.ErrOut, repositories.Target)
	client, e := a.ReviewClient(repositories.Target)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "initialize review provider")
	}
	upstream, e := s.Upstream(ctx)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "read branch upstream")
	}
	expected := repositories.PushRemote + "/" + branch
	if upstream != "" && upstream != expected {
		return clierror.New(clierror.Conflict, "current branch tracks %s, expected %s", upstream, expected)
	}
	if o.pushAttempted != nil {
		*o.pushAttempted = true
	}
	if e = s.PushCurrent(ctx, repositories.PushRemote, branch, upstream == ""); e != nil {
		return clierror.Wrap(clierror.Failure, e, "push %s", branch)
	}

	created, reused, e := findOrCreateReview(ctx, client, repositories, branch, c.Base, title, desc, o.draft, o.removeSourceBranch)
	if e != nil {
		return clierror.Wrap(clierror.Failure, e, "create or reuse PR after push; local and remote branch %s were kept, so retrying review submit is safe after checking provider status", branch)
	}
	if created.ID == "" || created.URL == "" || created.Number <= 0 {
		return clierror.New(clierror.Failure, "provider returned incomplete PR metadata; local and remote branch %s were kept", branch)
	}
	if _, e = savePublishedReview(ctx, s, c, branch, created); e != nil {
		return clierror.Wrap(clierror.Failure, e, "PR #%d exists but local review state could not be saved", created.Number)
	}
	if g.json {
		return writeJSON(a.IO.Out, created)
	}
	r := a.renderer(g)
	if !o.embedded {
		r.Command("review submit")
	}
	r.Success("Push", repositories.PushRemote+"/"+branch)
	if repositories.Fork {
		r.Field("Target", repositories.Target.Owner+"/"+repositories.Target.Name)
	}
	if reused {
		r.Success("PR", fmt.Sprintf("기존 #%d 재사용 · %s", created.Number, created.URL))
	} else {
		r.Success("PR", fmt.Sprintf("#%d · %s", created.Number, created.URL))
	}
	r.Next("PR merge 후 kit review finish")
	return nil
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
