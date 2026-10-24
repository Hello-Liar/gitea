// Copyright 2021 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	repo_module "gitea.dev/modules/repository"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/timeutil"
	"gitea.dev/services/release"
	repo_service "gitea.dev/services/repository"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateNewTagProtected(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})

	t.Run("Code", func(t *testing.T) {
		defer tests.PrintCurrentTest(t)()

		err := release.CreateNewTag(t.Context(), owner, repo, "master", "t-first", "first tag")
		assert.NoError(t, err)

		err = release.CreateNewTag(t.Context(), owner, repo, "master", "v-2", "second tag")
		assert.Error(t, err)
		assert.True(t, release.IsErrProtectedTagName(err))

		err = release.CreateNewTag(t.Context(), owner, repo, "master", "v-1.1", "third tag")
		assert.NoError(t, err)
	})

	t.Run("Git", func(t *testing.T) {
		onGiteaRun(t, func(t *testing.T, u *url.URL) {
			httpContext := NewAPITestContext(t, owner.Name, repo.Name)

			dstPath := t.TempDir()

			u.Path = httpContext.GitPath()
			u.User = url.UserPassword(owner.Name, userPassword)

			doGitClone(dstPath, u)(t)

			_, _, err := gitcmd.NewCommand("tag", "v-2").WithDir(dstPath).RunStdString(t.Context())
			assert.NoError(t, err)

			_, _, err = gitcmd.NewCommand("push", "--tags").WithDir(dstPath).RunStdString(t.Context())
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "Tag v-2 is protected")
		})
	})

	t.Run("GitTagForce", func(t *testing.T) {
		onGiteaRun(t, func(t *testing.T, u *url.URL) {
			httpContext := NewAPITestContext(t, owner.Name, repo.Name)

			dstPath := t.TempDir()

			u.Path = httpContext.GitPath()
			u.User = url.UserPassword(owner.Name, userPassword)

			doGitClone(dstPath, u)(t)

			_, _, err := gitcmd.NewCommand("tag", "v-1.1", "-m", "force update", "--force").
				WithDir(dstPath).
				RunStdString(t.Context())
			require.NoError(t, err)

			_, _, err = gitcmd.NewCommand("push", "--tags").WithDir(dstPath).RunStdString(t.Context())
			require.NoError(t, err)

			_, _, err = gitcmd.NewCommand("tag", "v-1.1", "-m", "force update v2", "--force").WithDir(dstPath).RunStdString(t.Context())
			require.NoError(t, err)

			_, _, err = gitcmd.NewCommand("push", "--tags").WithDir(dstPath).RunStdString(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the tag already exists in the remote")

			_, _, err = gitcmd.NewCommand("push", "--tags", "--force").WithDir(dstPath).RunStdString(t.Context())
			require.NoError(t, err)
			req := NewRequestf(t, "GET", "/%s/releases/tag/v-1.1", repo.FullName())
			resp := MakeRequest(t, req, http.StatusOK)
			htmlDoc := NewHTMLParser(t, resp.Body)
			tagsTab := htmlDoc.Find(".release-list-title")
			assert.Contains(t, tagsTab.Text(), "force update v2")
		})
	})

	// Cleanup
	releases, err := db.Find[repo_model.Release](t.Context(), repo_model.FindReleasesOptions{
		IncludeTags: true,
		TagNames:    []string{"v-1", "v-1.1"},
		RepoID:      repo.ID,
	})
	assert.NoError(t, err)

	for _, release := range releases {
		_, err = db.DeleteByID[repo_model.Release](t.Context(), release.ID)
		assert.NoError(t, err)
	}

	protectedTags, err := git_model.GetProtectedTags(t.Context(), repo.ID)
	assert.NoError(t, err)

	for _, protectedTag := range protectedTags {
		err = git_model.DeleteProtectedTag(t.Context(), protectedTag)
		assert.NoError(t, err)
	}
}

func TestRepushTag(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
		session := loginUser(t, owner.LowerName)
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)

		httpContext := NewAPITestContext(t, owner.Name, repo.Name)

		dstPath := t.TempDir()

		u.Path = httpContext.GitPath()
		u.User = url.UserPassword(owner.Name, userPassword)

		doGitClone(dstPath, u)(t)

		// create and push a tag
		_, _, err := gitcmd.NewCommand("tag", "v2.0").WithDir(dstPath).RunStdString(t.Context())
		assert.NoError(t, err)
		_, _, err = gitcmd.NewCommand("push", "origin", "--tags", "v2.0").WithDir(dstPath).RunStdString(t.Context())
		assert.NoError(t, err)
		// create a release for the tag
		createdRelease := createNewReleaseUsingAPI(t, token, owner, repo, "v2.0", "", "Release of v2.0", "desc")
		assert.False(t, createdRelease.IsDraft)
		// delete the tag
		_, _, err = gitcmd.NewCommand("push", "origin", "--delete", "v2.0").WithDir(dstPath).RunStdString(t.Context())
		assert.NoError(t, err)

		// query the release by API with no auth and it should be 404
		req := NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/%s/%s/releases/tags/%s", owner.Name, repo.Name, "v2.0"))
		MakeRequest(t, req, http.StatusNotFound)

		// query the release by API and it should be a draft
		req = NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/%s/%s/releases/tags/%s", owner.Name, repo.Name, "v2.0")).AddTokenAuth(token)
		resp := MakeRequest(t, req, http.StatusOK)
		respRelease := DecodeJSON(t, resp, &api.Release{})
		assert.True(t, respRelease.IsDraft)

		// re-push the tag
		_, _, err = gitcmd.NewCommand("push", "origin", "--tags", "v2.0").WithDir(dstPath).RunStdString(t.Context())
		assert.NoError(t, err)
		// query the release by API and it should not be a draft
		req = NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/%s/%s/releases/tags/%s", owner.Name, repo.Name, "v2.0"))
		resp = MakeRequest(t, req, http.StatusOK)
		respRelease = DecodeJSON(t, resp, &api.Release{})
		assert.False(t, respRelease.IsDraft)
	})
}

func TestTagWithoutTaggerDate(t *testing.T) {
	for _, sync := range []bool{false, true} {
		t.Run(fmt.Sprintf("MirrorSync=%t", sync), func(t *testing.T) {
			defer tests.PrepareTestEnv(t)()
			repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 57})
			owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
			gitRepo, err := git.OpenRepository(t.Context(), repo)
			require.NoError(t, err)
			defer gitRepo.Close()
			commit, err := gitRepo.GetBranchCommit(t.Context(), repo.DefaultBranch)
			require.NoError(t, err)
			parent, err := commit.Parent(t.Context(), gitRepo, 0)
			require.NoError(t, err)
			const tagName = "without-tagger"
			for _, target := range []*git.Commit{commit, parent} {
				data := fmt.Sprintf("object %s\ntype commit\ntag %s\n\nLegacy tag\n", target.ID.String(), tagName)
				tagID, _, runErr := gitcmd.NewCommand("hash-object", "--literally", "-t", "tag", "-w", "--stdin").WithRepo(gitRepo).WithStdinBytes([]byte(data)).RunStdString(t.Context())
				require.NoError(t, runErr)
				err = gitcmd.NewCommand("update-ref").AddDynamicArguments("refs/tags/"+tagName, strings.TrimSpace(tagID)).WithRepo(gitRepo).Run(t.Context())
				require.NoError(t, err)
				if sync {
					_, err = repo_module.SyncReleasesWithTags(t.Context(), repo, gitRepo)
				} else {
					err = repo_service.PushUpdateAddDeleteTags(t.Context(), repo, gitRepo, owner, []string{tagName}, nil)
				}
				require.NoError(t, err)
				tagRelease := unittest.AssertExistsAndLoadBean(t, &repo_model.Release{RepoID: repo.ID, TagName: tagName})
				expectedDate := timeutil.TimeStamp(target.Committer.When.Unix())
				assert.Equal(t, expectedDate, tagRelease.CreatedUnix)
				assert.Equal(t, expectedDate, tagRelease.PublishedUnix)
			}
		})
	}
}
