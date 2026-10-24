// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repository_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	git_model "gitea.dev/models/git"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/lfs"
	repo_module "gitea.dev/modules/repository"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/storage"
	"gitea.dev/modules/test"
	repo_service "gitea.dev/services/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGarbageCollectLFSMetaObjects(t *testing.T) {
	unittest.PrepareTestEnv(t)

	defer test.MockVariableValue(&setting.LFS.StartServer, true)()

	err := storage.Init()
	assert.NoError(t, err)

	repo, err := repo_model.GetRepositoryByOwnerAndName(t.Context(), "user2", "repo1")
	assert.NoError(t, err)

	// add lfs object
	lfsContent := []byte("gitea1")
	lfsOid := storeObjectInRepo(t, repo.ID, &lfsContent)
	lfsContentInRepoWithoutGitDir := []byte("gitea3")
	storeObjectInRepo(t, 6, &lfsContentInRepoWithoutGitDir)

	// gc
	err = repo_service.GarbageCollectLFSMetaObjects(t.Context(), repo_service.GarbageCollectLFSMetaObjectsOptions{
		AutoFix:                 true,
		OlderThan:               time.Now().Add(7 * 24 * time.Hour).Add(5 * 24 * time.Hour),
		UpdatedLessRecentlyThan: time.Now().Add(7 * 24 * time.Hour).Add(3 * 24 * time.Hour),
	})
	assert.NoError(t, err)

	// lfs meta has been deleted
	_, err = git_model.GetLFSMetaObjectByOid(t.Context(), repo.ID, lfsOid)
	assert.ErrorIs(t, err, git_model.ErrLFSObjectNotExist)
}

func TestGarbageCollectLFSMetaObjectsForRepoAutoFix(t *testing.T) {
	unittest.PrepareTestEnv(t)

	defer test.MockVariableValue(&setting.LFS.StartServer, true)()

	err := storage.Init()
	assert.NoError(t, err)

	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})

	// add lfs object
	lfsContent := []byte("gitea2")
	lfsOid := storeObjectInRepo(t, repo.ID, &lfsContent)

	assert.NoError(t, repo_module.UpdateRepoSize(t.Context(), repo))

	err = repo_service.GarbageCollectLFSMetaObjectsForRepo(t.Context(), repo, repo_service.GarbageCollectLFSMetaObjectsOptions{
		LogDetail:               func(string, ...any) {},
		AutoFix:                 true,
		OlderThan:               time.Now().Add(24 * time.Hour * 7),
		UpdatedLessRecentlyThan: time.Now().Add(24 * time.Hour * 3),
	})
	assert.NoError(t, err)

	_, err = git_model.GetLFSMetaObjectByOid(t.Context(), repo.ID, lfsOid)
	assert.ErrorIs(t, err, git_model.ErrLFSObjectNotExist)

	// LFSSize is recalculated to reflect the removed orphaned object
	repo = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	assert.EqualValues(t, 0, repo.LFSSize)
}

func storeObjectInRepo(t *testing.T, repositoryID int64, content *[]byte) string {
	pointer, err := lfs.GeneratePointer(bytes.NewReader(*content))
	assert.NoError(t, err)

	_, err = git_model.NewLFSMetaObject(t.Context(), repositoryID, pointer)
	assert.NoError(t, err)
	contentStore := lfs.NewContentStore()
	exist, err := contentStore.Exists(pointer)
	assert.NoError(t, err)
	if !exist {
		err := contentStore.Put(pointer, bytes.NewReader(*content))
		assert.NoError(t, err)
	}
	return pointer.Oid
}

func TestGarbageCollectLFSMetaObjectsPerRepoLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		minimum    int64
		proportion float64
		remaining  []int64
	}{
		{name: "Fixed", minimum: 1, remaining: []int64{5, 3}},
		{name: "Proportional", minimum: 1, proportion: 0.5, remaining: []int64{3, 2}},
		{name: "Unlimited", proportion: 0.5, remaining: []int64{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unittest.PrepareTestEnv(t)
			defer test.MockVariableValue(&setting.LFS.StartServer, true)()
			require.NoError(t, storage.Init())
			for i, repoID := range []int64{1, 3} {
				for object := range 6 - 2*i {
					content := fmt.Appendf(nil, "gc-limit-%d-%d", repoID, object)
					storeObjectInRepo(t, repoID, &content)
				}
			}
			require.NoError(t, repo_service.GarbageCollectLFSMetaObjects(t.Context(), repo_service.GarbageCollectLFSMetaObjectsOptions{
				AutoFix:                  true,
				OlderThan:                time.Now().Add(time.Hour),
				UpdatedLessRecentlyThan:  time.Now().Add(time.Hour),
				NumberToCheckPerRepo:     tc.minimum,
				ProportionToCheckPerRepo: tc.proportion,
			}))
			for i, repoID := range []int64{1, 3} {
				count, err := git_model.CountLFSMetaObjects(t.Context(), repoID)
				require.NoError(t, err)
				assert.Equal(t, tc.remaining[i], count, "repo %d", repoID)
			}
		})
	}
}
