package app

import (
	"fmt"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/selector"
	"kit/internal/ui"
)

func selectCommitsByRefs(pending []gitservice.Commit, refs []string) ([]gitservice.Commit, error) {
	requested := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		ref = strings.ToLower(strings.TrimSpace(ref))
		if !isHexCommitPrefix(ref) {
			return nil, clierror.New(clierror.Usage, "--commit requires a 4-40 character hexadecimal commit prefix, got %q", ref)
		}
		matches := make([]gitservice.Commit, 0, 1)
		for _, commit := range pending {
			if strings.HasPrefix(strings.ToLower(commit.Hash), ref) {
				matches = append(matches, commit)
			}
		}
		if len(matches) == 0 {
			return nil, clierror.New(clierror.Usage, "commit %q is not a pending candidate", ref)
		}
		if len(matches) > 1 {
			return nil, clierror.New(clierror.Usage, "commit prefix %q is ambiguous among pending candidates", ref)
		}
		requested[matches[0].Hash] = struct{}{}
	}
	selected := make([]gitservice.Commit, 0, len(requested))
	for _, commit := range pending {
		if _, ok := requested[commit.Hash]; ok {
			selected = append(selected, commit)
		}
	}
	return selected, nil
}

func isHexCommitPrefix(value string) bool {
	if len(value) < 4 || len(value) > 40 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (a *Application) selectPendingCommits(pending []gitservice.Commit, refs []string, all bool, title string) ([]gitservice.Commit, error) {
	if len(refs) > 0 {
		return selectCommitsByRefs(pending, refs)
	}
	if all {
		return append([]gitservice.Commit(nil), pending...), nil
	}
	items := make([]selector.Item, 0, len(pending))
	byHash := make(map[string]gitservice.Commit, len(pending))
	for _, commit := range pending {
		subject := ui.SafeText(commit.Subject)
		items = append(items, selector.Item{
			ID:      commit.Hash,
			Display: fmt.Sprintf("%-10s  %-16s  %s", commit.ShortHash, commit.Date, subject),
			Search:  commit.ShortHash + " " + commit.Date + " " + subject,
		})
		byHash[commit.Hash] = commit
	}
	if a.Select == nil {
		return nil, clierror.New(clierror.Failure, "interactive selection is unavailable")
	}
	selectedItems, err := a.Select(items, title)
	if err != nil {
		return nil, err
	}
	selected := make([]gitservice.Commit, 0, len(selectedItems))
	for _, item := range selectedItems {
		commit, ok := byHash[item.ID]
		if !ok {
			return nil, clierror.New(clierror.Failure, "selector returned an unknown commit %q", item.ID)
		}
		selected = append(selected, commit)
	}
	return selected, nil
}
