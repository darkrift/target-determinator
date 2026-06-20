package pkg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/target-determinator/common"
)

func Test_stringSliceContainsStartingWith(t *testing.T) {
	type args struct {
		slice   []common.RelPath
		element common.RelPath
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			"containsExact",
			args{
				[]common.RelPath{common.NewRelPath("foo")},
				common.NewRelPath("foo"),
			},
			true,
		},
		{
			"containsDirWithoutTrailingSlash",
			args{
				[]common.RelPath{common.NewRelPath("foo"), common.NewRelPath("bar/baz")},
				common.NewRelPath("foo/"),
			},
			true,
		},
		{
			"containsDirWithTrailingSlashButIsFile",
			args{
				[]common.RelPath{common.NewRelPath("foo/")},
				common.NewRelPath("foo"),
			},
			false,
		},
		{
			"containsPrefix",
			args{
				[]common.RelPath{common.NewRelPath("foo")},
				common.NewRelPath("foo/bar"),
			},
			true,
		},
		{
			"otherIsPrefix",
			args{
				[]common.RelPath{common.NewRelPath("foo/bar")},
				common.NewRelPath("foo"),
			},
			false,
		},
		{
			"doesNotContain",
			args{
				[]common.RelPath{common.NewRelPath("foo"), common.NewRelPath("bar/baz")},
				common.NewRelPath("frob"),
			},
			false,
		},
		{
			"stringPrefixButNotPathPrefix",
			args{
				[]common.RelPath{common.NewRelPath("foo/b")},
				common.NewRelPath("foo/bar"),
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringSliceContainsStartingWith(tt.args.slice, tt.args.element); got != tt.want {
				t.Errorf("stringSliceContainsStartingWith() with (slice = %v, element = %v) returns %v, want %v",
					tt.args.slice, tt.args.element.String(), got, tt.want)
			}
		})
	}
}

func Test_ParseCanonicalLabel(t *testing.T) {
	n := Normalizer{}
	for _, tt := range []string{
		"@//label",
		"@//label:package",
		"//label:package",
		":package",
		"@rules_python~0.21.0~pip~pip_boto3//:pkg",
	} {
		_, err := n.ParseCanonicalLabel(tt)
		if err != nil {
			t.Errorf("ParseCanonicalLabel() with (label=%s) produces error %s", tt, err)
		}
	}
}

func TestGitSafeCheckoutRestoresOriginalAfterPostCheckoutDirtyWorktreeFallback(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.name", "Target Determinator Test")
	runGit(t, repo, "config", "user.email", "target-determinator@example.invalid")

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("before\n"), 0644); err != nil {
		t.Fatalf("failed to write tracked file: %v", err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-m", "before")
	beforeSHA := runGit(t, repo, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored-file\n"), 0644); err != nil {
		t.Fatalf("failed to write .gitignore: %v", err)
	}
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-m", "after")
	afterSHA := runGit(t, repo, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repo, "ignored-file"), []byte("ignored\n"), 0644); err != nil {
		t.Fatalf("failed to write ignored file: %v", err)
	}

	beforeRev, err := NewLabelledGitRev(repo, beforeSHA, "before")
	if err != nil {
		t.Fatalf("failed to create before revision: %v", err)
	}
	afterRev, err := NewLabelledGitRev(repo, afterSHA, "after")
	if err != nil {
		t.Fatalf("failed to create after revision: %v", err)
	}

	context := &Context{
		WorkspacePath:        repo,
		OriginalRevision:     afterRev,
		CacheDirectory:       t.TempDir(),
		EnforceCleanRepo:     false,
		DeleteCachedWorktree: true,
	}

	worktree, err := gitSafeCheckout(context, beforeRev, nil)
	if err != nil {
		t.Fatalf("gitSafeCheckout failed: %v", err)
	}
	if worktree == "" {
		t.Fatal("expected gitSafeCheckout to use a worktree")
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(worktree); err != nil {
			t.Fatalf("failed to remove worktree: %v", err)
		}
	})

	if got := runGit(t, repo, "rev-parse", "HEAD"); got != afterSHA {
		t.Fatalf("primary checkout HEAD = %s, want %s", got, afterSHA)
	}
	if got := runGit(t, worktree, "rev-parse", "HEAD"); got != beforeSHA {
		t.Fatalf("worktree HEAD = %s, want %s", got, beforeSHA)
	}
	if _, err := os.Stat(filepath.Join(repo, "ignored-file")); err != nil {
		t.Fatalf("ignored file was not preserved in primary checkout: %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
